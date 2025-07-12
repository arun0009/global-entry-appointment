package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/kelseyhightower/envconfig"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// corsHeaders defines the CORS headers for responses
var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":      "https://arun0009.github.io",
	"Access-Control-Allow-Methods":     "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers":     "Content-Type",
	"Access-Control-Allow-Credentials": "true",
}

var validNtfyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var ErrMaxNtfyFailures = errors.New("maximum ntfy failures reached")

type (
	// Config holds environment variables
	Config struct {
		MongoDBPassword          string        `envconfig:"MONGODB_PASSWORD" required:"true"`
		NtfyServer               string        `envconfig:"NTFY_SERVER" default:"https://ntfy.sh/"`
		HTTPTimeout              time.Duration `envconfig:"HTTP_TIMEOUT_SECONDS" default:"5s"`
		NotificationCooldownTime time.Duration `envconfig:"NOTIFICATION_COOLDOWN_TIME" default:"15m"`
		MongoConnectTimeout      time.Duration `envconfig:"MONGO_CONNECT_TIMEOUT" default:"10s"`
		SubscriptionTTL          time.Duration `envconfig:"SUBSCRIPTION_TTL_DAYS" default:"720h"`
		MaxNotifications         int           `envconfig:"MAX_NOTIFICATIONS" default:"10"`
		MaxRetries               int           `envconfig:"MAX_RETRIES" default:"1"`
		MaxNtfyFailures          int           `envconfig:"MAX_NTFY_FAILURES" default:"2"`
	}

	// Subscription represents a subscription document
	Subscription struct {
		ID             bson.ObjectID `bson:"_id"`
		Location       string        `bson:"location"`
		NtfyTopic      string        `bson:"ntfyTopic"`
		CreatedAt      time.Time     `bson:"createdAt"`
		LastNotifiedAt time.Time     `bson:"lastNotifiedAt,omitempty"`
	}

	// LocationTopics represents aggregated data
	LocationTopics struct {
		Location      string         `bson:"_id"`
		Subscriptions []Subscription `bson:"subscriptions"`
	}

	// Appointment from Global Entry API
	Appointment struct {
		LocationID     int    `json:"locationId"`
		StartTimestamp string `json:"startTimestamp"`
		EndTimestamp   string `json:"endTimestamp"`
		Active         bool   `json:"active"`
		Duration       int    `json:"duration"`
		RemoteInd      bool   `json:"remoteInd"`
	}

	// SubscriptionRequest for registration/unsubscription
	SubscriptionRequest struct {
		Action    string `json:"action"`
		Location  string `json:"location"`
		NtfyTopic string `json:"ntfyTopic"`
	}

	// LambdaHandler holds dependencies
	LambdaHandler struct {
		Config          Config
		URL             string
		Client          *mongo.Client
		HTTPClient      *retryablehttp.Client
		failedNtfyCount int // Global counter for notification failures
	}
)

// NewLambdaHandler creates a new LambdaHandler
func NewLambdaHandler(config Config, url string, client *mongo.Client) *LambdaHandler {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = config.MaxRetries
	retryClient.RetryWaitMin = 100 * time.Millisecond
	retryClient.RetryWaitMax = 200 * time.Millisecond
	retryClient.HTTPClient.Timeout = config.HTTPTimeout
	retryClient.Logger = nil // Use slog instead

	return &LambdaHandler{
		Config:     config,
		URL:        url,
		Client:     client,
		HTTPClient: retryClient,
	}
}

// fetchAppointments retrieves appointment slots from the API
func (h *LambdaHandler) fetchAppointments(ctx context.Context, location string) ([]Appointment, error) {
	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(h.URL, location), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		slog.Warn("Failed to get appointment slots", "location", location, "error", err)
		return nil, fmt.Errorf("failed to fetch appointments: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("Non-OK status from API", "location", location, "status", resp.StatusCode)
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	var appointments []Appointment
	if err := json.Unmarshal(body, &appointments); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %v", err)
	}
	return appointments, nil
}

// sendNtfyNotification sends a notification to the specified ntfy topic
func (h *LambdaHandler) sendNtfyNotification(ctx context.Context, topic, title, message string) error {
	if h.failedNtfyCount >= h.Config.MaxNtfyFailures {
		return ErrMaxNtfyFailures
	}

	payload := map[string]string{
		"topic":   topic,
		"message": message,
		"title":   title,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodPost, h.Config.NtfyServer, bytes.NewBuffer(payloadBytes))
	if err != nil {
		h.failedNtfyCount++
		return fmt.Errorf("failed to create ntfy request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.HTTPClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err != nil {
			slog.Warn("Failed to send ntfy notification", "topic", topic, "error", err)
		} else {
			slog.Warn("Non-OK status from ntfy", "topic", topic, "status", resp.StatusCode)
			resp.Body.Close()
		}
		h.failedNtfyCount++
		if err != nil {
			return fmt.Errorf("failed to send ntfy notification: %v", err)
		}
		return fmt.Errorf("ntfy returned status %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	slog.Info("Sent ntfy notification", "topic", topic, "title", title)
	return nil
}

// updateLastNotified updates the lastNotifiedAt timestamp for multiple subscriptions
func (h *LambdaHandler) updateLastNotified(ctx context.Context, coll *mongo.Collection, updates []mongo.WriteModel) error {
	if len(updates) == 0 {
		return nil
	}

	result, err := coll.BulkWrite(ctx, updates)
	if err != nil {
		return fmt.Errorf("failed to update lastNotifiedAt: %v", err)
	}
	slog.Debug("Updated lastNotifiedAt", "modifiedCount", result.ModifiedCount)
	return nil
}

// notifyEligibleSubscribers sends notifications to eligible subscribers
func (h *LambdaHandler) notifyEligibleSubscribers(ctx context.Context, coll *mongo.Collection, location string, subscriptions []Subscription, appointments []Appointment, globalNotifiedCount int) (int, error) {
	if len(appointments) == 0 || !appointments[0].Active {
		return globalNotifiedCount, nil
	}

	now := time.Now().UTC()
	var bulkUpdates []mongo.WriteModel
	localNotifiedCount := 0

	for _, sub := range subscriptions {
		if h.failedNtfyCount >= h.Config.MaxNtfyFailures {
			return globalNotifiedCount, ErrMaxNtfyFailures
		}

		if globalNotifiedCount >= h.Config.MaxNotifications {
			slog.Info("Reached global notification limit")
			return globalNotifiedCount, nil
		}

		message := fmt.Sprintf("Appointment available at %s on %s", location, appointments[0].StartTimestamp)
		if err := h.sendNtfyNotification(ctx, sub.NtfyTopic, "Global Entry Appointment Notification", message); err != nil {
			slog.Error("Failed to send appointment notification", "topic", sub.NtfyTopic, "error", err)
			if errors.Is(err, ErrMaxNtfyFailures) {
				return globalNotifiedCount, err
			}
			continue
		}

		bulkUpdates = append(bulkUpdates, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": sub.ID}).
			SetUpdate(bson.M{"$set": bson.M{"lastNotifiedAt": now}}))
		globalNotifiedCount++
		localNotifiedCount++
	}

	if err := h.updateLastNotified(ctx, coll, bulkUpdates); err != nil {
		slog.Warn("Failed to update lastNotifiedAt", "error", err)
	}

	if localNotifiedCount > 0 {
		slog.Info("Sent notifications", "count", localNotifiedCount, "location", location)
	}
	return globalNotifiedCount, nil
}

// checkAvailabilityAndNotify checks availability and notifies eligible subscribers
func (h *LambdaHandler) checkAvailabilityAndNotify(ctx context.Context, location string, subscriptions []Subscription, globalNotifiedCount int) (int, error) {
	appointments, err := h.fetchAppointments(ctx, location)
	if err != nil {
		return globalNotifiedCount, fmt.Errorf("failed to fetch appointments: %v", err)
	}

	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")
	return h.notifyEligibleSubscribers(ctx, coll, location, subscriptions, appointments, globalNotifiedCount)
}

// deleteExpiredSubscription deletes a single expired subscription
func (h *LambdaHandler) deleteExpiredSubscription(ctx context.Context, coll *mongo.Collection, sub Subscription) error {
	message := "Your Global Entry appointment subscription has expired."
	if err := h.sendNtfyNotification(ctx, sub.NtfyTopic, "Global Entry Subscription Expired", message); err != nil {
		slog.Error("Failed to send expiration notification", "topic", sub.NtfyTopic, "error", err)
	}

	_, err := coll.DeleteOne(ctx, bson.M{"_id": sub.ID})
	if err != nil {
		return fmt.Errorf("failed to delete subscription %s: %v", sub.ID, err)
	}
	slog.Info("Deleted expired subscription", "id", sub.ID)
	return nil
}

// handleExpiringSubscriptions deletes subscriptions older than TTL
func (h *LambdaHandler) handleExpiringSubscriptions(ctx context.Context, coll *mongo.Collection) error {
	ttlThreshold := time.Now().UTC().Add(-h.Config.SubscriptionTTL)
	slog.Debug("Checking for expiring subscriptions", "ttlThreshold", ttlThreshold)

	cursor, err := coll.Find(ctx, bson.M{"createdAt": bson.M{"$lte": ttlThreshold}})
	if err != nil {
		return fmt.Errorf("failed to find expiring subscriptions: %v", err)
	}
	defer cursor.Close(ctx)

	var subscriptions []Subscription
	if err := cursor.All(ctx, &subscriptions); err != nil {
		return fmt.Errorf("failed to decode expiring subscriptions: %v", err)
	}

	for _, sub := range subscriptions {
		if err := h.deleteExpiredSubscription(ctx, coll, sub); err != nil {
			slog.Error("Failed to process expired subscription", "id", sub.ID, "error", err)
		}
	}
	return nil
}

// validateSubscriptionRequest validates the subscription request
func (h *LambdaHandler) validateSubscriptionRequest(req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	if req.Location == "" || req.NtfyTopic == "" {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "location and ntfyTopic are required"}`,
		}, nil
	}

	if !validNtfyPattern.MatchString(req.NtfyTopic) {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "Ntfy Topic must not contain spaces or special characters"}`,
		}, nil
	}
	return events.APIGatewayV2HTTPResponse{}, nil
}

// handleSubscribe processes subscription requests
func (h *LambdaHandler) handleSubscribe(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	count, err := coll.CountDocuments(ctx, bson.M{"location": req.Location, "ntfyTopic": req.NtfyTopic})
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    corsHeaders,
			Body:       `{"error": "failed to check existing subscription"}`,
		}, fmt.Errorf("failed to check existing subscription: %v", err)
	}
	if count > 0 {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "subscription already exists"}`,
		}, nil
	}

	_, err = coll.InsertOne(ctx, bson.M{
		"_id":            bson.NewObjectID(),
		"location":       req.Location,
		"ntfyTopic":      req.NtfyTopic,
		"createdAt":      time.Now().UTC(),
		"lastNotifiedAt": time.Time{},
	})
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    corsHeaders,
			Body:       `{"error": "failed to insert subscription"}`,
		}, fmt.Errorf("failed to insert subscription: %v", err)
	}
	slog.Info("Added subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic)

	message := fmt.Sprintf("You're all set! We'll notify you when an appointment slot is available at %s.", req.Location)
	if err := h.sendNtfyNotification(ctx, req.NtfyTopic, "Global Entry Subscription Confirmation", message); err != nil {
		slog.Error("Failed to send confirmation notification", "topic", req.NtfyTopic, "error", err)
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders,
		Body:       `{"message": "Subscribed successfully"}`,
	}, nil
}

// handleUnsubscribe processes unsubscription requests
func (h *LambdaHandler) handleUnsubscribe(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	result, err := coll.DeleteOne(ctx, bson.M{"location": req.Location, "ntfyTopic": req.NtfyTopic})
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    corsHeaders,
			Body:       `{"error": "failed to delete subscription"}`,
		}, fmt.Errorf("failed to delete subscription: %v", err)
	}
	if result.DeletedCount == 0 {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 404,
			Headers:    corsHeaders,
			Body:       `{"error": "subscription not found"}`,
		}, nil
	}
	slog.Info("Removed subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic)
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders,
		Body:       `{"message": "Unsubscribed successfully"}`,
	}, nil
}

// handleSubscription manages subscribe/unsubscribe requests
func (h *LambdaHandler) handleSubscription(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	if resp, err := h.validateSubscriptionRequest(req); err != nil || resp.StatusCode != 0 {
		return resp, err
	}

	switch req.Action {
	case "subscribe":
		return h.handleSubscribe(ctx, coll, req)
	case "unsubscribe":
		return h.handleUnsubscribe(ctx, coll, req)
	default:
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "invalid action, use subscribe or unsubscribe"}`,
		}, nil
	}
}

// HandleRequest handles Scheduled Events and API requests
func (h *LambdaHandler) HandleRequest(ctx context.Context, event json.RawMessage) (events.APIGatewayV2HTTPResponse, error) {
	h.failedNtfyCount = 0
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")

	var eventMap map[string]interface{}
	if err := json.Unmarshal(event, &eventMap); err != nil {
		slog.Error("Failed to parse event as JSON", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "invalid event format"}`,
		}, nil
	}

	// Handle OPTIONS request
	if req, ok := eventMap["requestContext"].(map[string]interface{}); ok {
		if httpInfo, ok := req["http"].(map[string]interface{}); ok {
			if method, ok := httpInfo["method"].(string); ok && method == "OPTIONS" {
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 200,
					Headers:    corsHeaders,
					Body:       "",
				}, nil
			}
		}
	}

	// Handle CloudWatch Event
	if source, ok := eventMap["source"].(string); ok && source == "aws.events" {
		if err := h.handleExpiringSubscriptions(ctx, coll); err != nil {
			slog.Error("Failed to handle expiring subscriptions", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Headers:    corsHeaders,
				Body:       `{"error": "failed to handle expiring subscriptions"}`,
			}, nil
		}

		cooldownThreshold := time.Now().UTC().Add(-h.Config.NotificationCooldownTime)
		pipeline := mongo.Pipeline{
			bson.D{{
				"$match", bson.M{
					"$or": []bson.M{
						{"lastNotifiedAt": time.Time{}},
						{"lastNotifiedAt": bson.M{"$lte": cooldownThreshold}},
					},
				},
			}},
			bson.D{{
				"$addFields", bson.M{
					"isZero": bson.M{
						"$eq": []interface{}{"$lastNotifiedAt", time.Time{}},
					},
				},
			}},
			bson.D{{
				"$sort", bson.M{
					"isZero":         -1,
					"lastNotifiedAt": -1,
				},
			}},
			bson.D{{
				"$group", bson.D{
					{"_id", "$location"},
					{"subscriptions", bson.D{{
						"$push", bson.M{
							"_id":            "$_id",
							"location":       "$location",
							"ntfyTopic":      "$ntfyTopic",
							"createdAt":      "$createdAt",
							"lastNotifiedAt": "$lastNotifiedAt",
						},
					}}},
				},
			}},
		}
		cursor, err := coll.Aggregate(ctx, pipeline)
		if err != nil {
			slog.Error("Failed to execute aggregation", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Headers:    corsHeaders,
				Body:       `{"error": "failed to execute aggregation"}`,
			}, nil
		}
		defer cursor.Close(ctx)

		var locationTopics []LocationTopics
		if err := cursor.All(ctx, &locationTopics); err != nil {
			slog.Error("Failed to decode aggregation results", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Headers:    corsHeaders,
				Body:       `{"error": "failed to decode aggregation results"}`,
			}, nil
		}

		globalNotifiedCount := 0
		for _, lt := range locationTopics {
			if h.failedNtfyCount >= h.Config.MaxNtfyFailures {
				slog.Error("Terminating due to maximum ntfy failures", "location", lt.Location)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    corsHeaders,
					Body:       `{"error": "maximum notification failures reached"}`,
				}, ErrMaxNtfyFailures
			}

			if globalNotifiedCount >= h.Config.MaxNotifications {
				slog.Info("Terminating due to global notification limit")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 200,
					Headers:    corsHeaders,
					Body:       `{"message": "cloudwatch event processed"}`,
				}, nil
			}

			var err error
			globalNotifiedCount, err = h.checkAvailabilityAndNotify(ctx, lt.Location, lt.Subscriptions, globalNotifiedCount)
			if err != nil {
				if errors.Is(err, ErrMaxNtfyFailures) {
					slog.Error("Terminating due to maximum ntfy failures", "location", lt.Location)
					return events.APIGatewayV2HTTPResponse{
						StatusCode: 500,
						Headers:    corsHeaders,
						Body:       `{"error": "maximum notification failures reached"}`,
					}, ErrMaxNtfyFailures
				}
				slog.Warn("Failed to check availability", "location", lt.Location, "error", err)
			}
		}

		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    corsHeaders,
			Body:       `{"message": "cloudwatch event processed"}`,
		}, nil
	}

	// Handle API Gateway V2 HTTP event
	if rawPath, ok := eventMap["rawPath"].(string); ok {
		requestContext, ok := eventMap["requestContext"].(map[string]interface{})
		if !ok {
			slog.Error("Missing requestContext in event")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    corsHeaders,
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		httpInfo, ok := requestContext["http"].(map[string]interface{})
		if !ok {
			slog.Error("Missing http info in requestContext")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    corsHeaders,
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		method, ok := httpInfo["method"].(string)
		if !ok {
			slog.Error("Missing method in http info")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    corsHeaders,
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		body, _ := eventMap["body"].(string)

		if method == "POST" && strings.HasSuffix(rawPath, "/subscriptions") {
			if body == "" {
				slog.Error("Invalid request: missing body")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    corsHeaders,
					Body:       `{"error": "missing request body"}`,
				}, nil
			}
			var subReq SubscriptionRequest
			if err := json.Unmarshal([]byte(body), &subReq); err != nil {
				slog.Error("Failed to parse request body", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    corsHeaders,
					Body:       `{"error": "invalid request body"}`,
				}, nil
			}
			if subReq.Action == "" || subReq.Location == "" || subReq.NtfyTopic == "" {
				slog.Error("Invalid subscription request: missing required fields")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    corsHeaders,
					Body:       `{"error": "missing required fields"}`,
				}, nil
			}
			resp, err := h.handleSubscription(ctx, coll, subReq)
			if err != nil {
				slog.Error("Failed to handle subscription", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    corsHeaders,
					Body:       `{"error": "internal server error"}`,
				}, fmt.Errorf("failed to handle subscription: %v", err)
			}
			resp.Headers = corsHeaders // Ensure CORS headers are set
			slog.Debug("Returning response", "statusCode", resp.StatusCode, "body", resp.Body)
			return resp, nil
		}
	}

	slog.Error("Unsupported event type", "event", string(event))
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 400,
		Headers:    corsHeaders,
		Body:       `{"error": "unsupported event type"}`,
	}, nil
}

func main() {
	var config Config
	if err := envconfig.Process("", &config); err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	url := "https://ttp.cbp.dhs.gov/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1"
	dbURL := "mongodb+srv://arun0009:%s@global-entry-appointmen.fcwlj2v.mongodb.net/?retryWrites=true&w=majority&appName=global-entry-appointment-cluster"
	serverAPI := options.ServerAPI(options.ServerAPIVersion1)
	opts := options.Client().ApplyURI(fmt.Sprintf(dbURL, config.MongoDBPassword)).SetServerAPIOptions(serverAPI).SetConnectTimeout(config.MongoConnectTimeout)
	client, err := mongo.Connect(opts)
	if err != nil {
		slog.Error("Failed to connect to MongoDB", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.MongoConnectTimeout)
	defer cancel()

	defer client.Disconnect(ctx)

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	handler := NewLambdaHandler(config, url, client)
	lambda.Start(handler.HandleRequest)
}

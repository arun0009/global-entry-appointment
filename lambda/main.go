package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/kelseyhightower/envconfig"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":      "https://arun0009.github.io",
	"Access-Control-Allow-Methods":     "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers":     "Content-Type",
	"Access-Control-Allow-Credentials": "true",
}

var validNtfyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type (
	// Config holds environment variables
	Config struct {
		MongoDBPassword          string        `envconfig:"MONGODB_PASSWORD" required:"true"`
		NtfyServer               string        `envconfig:"NTFY_SERVER" default:"https://ntfy.sh/"`
		HTTPTimeout              time.Duration `envconfig:"HTTP_TIMEOUT_SECONDS" default:"5s"`
		NotificationCooldownTime time.Duration `envconfig:"NOTIFICATION_COOLDOWN_TIME" default:"60m"`
	}

	// Subscription represents a subscription document
	Subscription struct {
		ID             bson.ObjectID `bson:"_id"`
		Location       string        `bson:"location"`
		NtfyTopic      string        `bson:"ntfyTopic"`
		CreatedAt      time.Time     `bson:"createdAt"`
		LastNotifiedAt time.Time     `bson:"lastNotifiedAt,omitempty"` // Optional, zero value if never notified
	}

	// LocationTopics represents aggregated data: location and its ntfyTopics array
	LocationTopics struct {
		Location   string   `bson:"_id"`
		NtfyTopics []string `bson:"ntfyTopics"`
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
		Action    string `json:"action"` // "subscribe" or "unsubscribe"
		Location  string `json:"location"`
		NtfyTopic string `json:"ntfyTopic"`
	}

	// LambdaHandler holds dependencies
	LambdaHandler struct {
		Config     Config
		URL        string
		Client     *mongo.Client
		HTTPClient *http.Client
	}
)

// NewLambdaHandler creates a new LambdaHandler
func NewLambdaHandler(config Config, url string, client *mongo.Client) *LambdaHandler {
	return &LambdaHandler{
		Config: config,
		URL:    url,
		Client: client,
		HTTPClient: &http.Client{
			Timeout: config.HTTPTimeout,
		},
	}
}

// sendNtfyNotification sends a notification to the specified ntfy topic
func (h *LambdaHandler) sendNtfyNotification(ctx context.Context, topic, title, message string) error {
	payload := map[string]string{
		"topic":   topic,
		"message": message,
		"title":   title,
	}
	payloadBytes, _ := json.Marshal(payload)

	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Config.NtfyServer, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return fmt.Errorf("failed to create ntfy request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := h.HTTPClient.Do(req)
		if err != nil {
			slog.Warn("Failed to send ntfy notification", "topic", topic, "attempt", attempt, "error", err)
			if attempt == 3 {
				slog.Error("Failed to send ntfy notification after 3 attempts", "topic", topic, "error", err)
				return fmt.Errorf("failed to send ntfy notification after 3 attempts: %v", err)
			}
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			slog.Info("Sent ntfy notification", "topic", topic, "title", title, "message", message)
			return nil
		}
		slog.Warn("Non-OK status from ntfy", "topic", topic, "status", resp.StatusCode)
	}
	return fmt.Errorf("failed to send ntfy notification after receiving non-OK status")
}

// checkAvailabilityAndNotify checks appointment availability and notifies topics
func (h *LambdaHandler) checkAvailabilityAndNotify(ctx context.Context, location string, topics []string) error {
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")
	now := time.Now().UTC()

	// Retry logic for API call
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(h.URL, location), nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %v", err)
		}

		resp, err := h.HTTPClient.Do(req)
		if err != nil {
			slog.Warn("Failed to get appointment slots", "location", location, "attempt", attempt, "error", err)
			if attempt == 3 {
				return fmt.Errorf("failed after %d attempts: %v", attempt, err)
			}
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			slog.Warn("Non-OK status from API", "location", location, "status", resp.StatusCode)
			return fmt.Errorf("API returned status %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %v", err)
		}

		var appointments []Appointment
		if err := json.Unmarshal(body, &appointments); err != nil {
			return fmt.Errorf("failed to unmarshal response: %v", err)
		}

		if len(appointments) > 0 && appointments[0].Active {
			for _, topic := range topics {
				// Check if notification can be sent based on cooldown
				var sub Subscription
				err := coll.FindOne(ctx, bson.M{"location": location, "ntfyTopic": topic}).Decode(&sub)
				if err != nil {
					slog.Error("Failed to fetch subscription", "location", location, "ntfyTopic", topic, "error", err)
					continue
				}

				// Skip if last notification was sent within cooldown period
				if !sub.LastNotifiedAt.IsZero() && now.Sub(sub.LastNotifiedAt) < h.Config.NotificationCooldownTime {
					slog.Debug("Skipping notification due to cooldown", "location", location, "ntfyTopic", topic, "lastNotifiedAt", sub.LastNotifiedAt)
					continue
				}

				message := fmt.Sprintf("Appointment available at %s on %s", location, appointments[0].StartTimestamp)
				if err := h.sendNtfyNotification(ctx, topic, "Global Entry Appointment Notification", message); err != nil {
					slog.Error("Failed to send appointment notification", "topic", topic, "error", err)
					continue
				}

				// Update lastNotifiedAt for the subscription
				_, err = coll.UpdateOne(
					ctx,
					bson.M{"_id": sub.ID},
					bson.M{"$set": bson.M{"lastNotifiedAt": now}},
				)
				if err != nil {
					slog.Error("Failed to update lastNotifiedAt", "id", sub.ID, "error", err)
				} else {
					slog.Debug("Updated lastNotifiedAt", "id", sub.ID, "time", now)
				}
			}
		}
		return nil
	}
	return nil
}

// handleExpiringSubscriptions deletes subscriptions older than 30 days and notifies
func (h *LambdaHandler) handleExpiringSubscriptions(ctx context.Context, coll *mongo.Collection) error {
	now := time.Now().UTC()
	ttlThreshold := now.Add(-30 * 24 * time.Hour) // 30 days ago
	slog.Debug("Checking for expiring subscriptions", "now", now, "ttlThreshold", ttlThreshold)

	filter := bson.M{
		"createdAt": bson.M{
			"$lte": ttlThreshold,
		},
	}
	cursor, err := coll.Find(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to find expiring subscriptions: %v", err)
	}
	defer cursor.Close(ctx)

	var subscriptions []Subscription
	if err := cursor.All(ctx, &subscriptions); err != nil {
		return fmt.Errorf("failed to decode expiring subscriptions: %v", err)
	}

	for _, sub := range subscriptions {
		slog.Debug("Found expiring subscription", "id", sub.ID, "createdAt", sub.CreatedAt)
		// Send expiration notification
		message := "Your Global Entry appointment subscription has expired."
		if err := h.sendNtfyNotification(ctx, sub.NtfyTopic, "Global Entry Subscription Expired", message); err != nil {
			slog.Error("Failed to send expiration notification", "topic", sub.NtfyTopic, "error", err)
		}

		// Delete the subscription
		_, err := coll.DeleteOne(ctx, bson.M{"_id": sub.ID})
		if err != nil {
			slog.Error("Failed to delete subscription", "id", sub.ID, "error", err)
		} else {
			slog.Info("Deleted expired subscription", "id", sub.ID)
		}
	}
	return nil
}

// handleSubscription manages subscribe/unsubscribe requests
func (h *LambdaHandler) handleSubscription(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
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

	switch req.Action {
	case "subscribe":
		// Check if subscription already exists
		count, err := coll.CountDocuments(ctx, bson.M{"location": req.Location, "ntfyTopic": req.NtfyTopic})
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 500}, fmt.Errorf("failed to check existing subscription: %v", err)
		}
		if count > 0 {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    corsHeaders,
				Body:       `{"error": "subscription already exists"}`,
			}, nil
		}

		// Insert new subscription
		_, err = coll.InsertOne(ctx, bson.M{
			"_id":            bson.NewObjectID(),
			"location":       req.Location,
			"ntfyTopic":      req.NtfyTopic,
			"createdAt":      time.Now().UTC(),
			"lastNotifiedAt": time.Time{}, // Explicitly set to zero value
		})
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 500}, fmt.Errorf("failed to insert subscription: %v", err)
		}
		slog.Info("Added subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic)

		// Send confirmation notification
		message := fmt.Sprintf("You're all set! We'll notify you when an appointment slot is available at %s.", req.Location)
		if err := h.sendNtfyNotification(ctx, req.NtfyTopic, "Global Entry Subscription Confirmation", message); err != nil {
			slog.Error("Failed to send confirmation notification", "topic", req.NtfyTopic, "error", err)
			// Don't fail the subscription if notification fails
		}

		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    corsHeaders,
			Body:       `{"message": "Subscribed successfully"}`,
		}, nil

	case "unsubscribe":
		result, err := coll.DeleteOne(ctx, bson.M{"location": req.Location, "ntfyTopic": req.NtfyTopic})
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 500}, fmt.Errorf("failed to delete subscription: %v", err)
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
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")

	// Parse event as JSON map
	var eventMap map[string]interface{}
	if err := json.Unmarshal(event, &eventMap); err != nil {
		slog.Error("Failed to parse event as JSON", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeaders,
			Body:       `{"error": "invalid event format"}`,
		}, nil
	}

	// Handle front end OPTIONS request
	if req, ok := eventMap["requestContext"].(map[string]interface{}); ok {
		if method, ok := req["http"].(map[string]interface{})["method"].(string); ok && method == "OPTIONS" {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 200,
				Headers:    corsHeaders,
				Body:       "",
			}, nil
		}
	}

	// Check for CloudWatch Event
	if source, ok := eventMap["source"].(string); ok && source == "aws.events" {
		if err := h.handleExpiringSubscriptions(ctx, coll); err != nil {
			slog.Error("Failed to handle expiring subscriptions", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Body:       `{"error": "failed to handle expiring subscriptions"}`,
			}, nil
		}

		pipeline := mongo.Pipeline{
			bson.D{{
				"$group", bson.D{
					{"_id", "$location"},
					{"ntfyTopics", bson.D{{"$push", "$ntfyTopic"}}},
				},
			}},
		}
		cursor, err := coll.Aggregate(ctx, pipeline)
		if err != nil {
			slog.Error("Failed to execute aggregation", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Body:       `{"error": "failed to execute aggregation"}`,
			}, nil
		}
		defer cursor.Close(ctx)

		var locationTopics []LocationTopics
		if err := cursor.All(ctx, &locationTopics); err != nil {
			slog.Error("Failed to decode aggregation results", "error", err)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Body:       `{"error": "failed to decode aggregation results"}`,
			}, nil
		}

		var wg sync.WaitGroup
		semaphore := make(chan struct{}, 10)
		for _, lt := range locationTopics {
			wg.Add(1)
			go func(lt LocationTopics) {
				defer wg.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				if err := h.checkAvailabilityAndNotify(ctx, lt.Location, lt.NtfyTopics); err != nil {
					slog.Error("Failed to check availability", "location", lt.Location, "error", err)
				}
			}(lt)
		}
		wg.Wait()

		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
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
			slog.Debug("Calling handleSubscription", "action", subReq.Action, "location", subReq.Location)
			resp, err := h.handleSubscription(ctx, coll, subReq)
			if err != nil {
				return resp, err
			}
			// Ensure response body is JSON string
			if resp.Body != "" {
				var bodyMap interface{}
				if json.Unmarshal([]byte(resp.Body), &bodyMap) == nil {
					b, err := json.Marshal(bodyMap)
					if err != nil {
						slog.Error("Failed to marshal response body", "error", err)
					}
					resp.Body = string(b)
				}
			}
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
		panic(fmt.Sprintf("failed to load config: %v", err))
	}

	url := "https://ttp.cbp.dhs.gov/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1"
	dbURL := "mongodb+srv://arun0009:%s@global-entry-appointmen.fcwlj2v.mongodb.net/?retryWrites=true&w=majority&appName=global-entry-appointment-cluster"
	serverAPI := options.ServerAPI(options.ServerAPIVersion1)
	opts := options.Client().ApplyURI(fmt.Sprintf(dbURL, config.MongoDBPassword)).SetServerAPIOptions(serverAPI).SetConnectTimeout(10 * time.Second)
	client, err := mongo.Connect(opts)
	if err != nil {
		panic(fmt.Sprintf("failed to connect to MongoDB: %v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	coll := client.Database("global-entry-appointment-db").Collection("subscriptions")

	// Initialize lastNotifiedAt for existing subscriptions
	_, err = coll.UpdateMany(
		ctx,
		bson.M{"lastNotifiedAt": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"lastNotifiedAt": time.Time{}}},
	)
	if err != nil {
		slog.Error("Failed to initialize lastNotifiedAt for existing subscriptions", "error", err)
	} else {
		slog.Info("Initialized lastNotifiedAt for existing subscriptions")
	}

	defer client.Disconnect(context.Background())

	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	handler := NewLambdaHandler(config, url, client)
	lambda.Start(handler.HandleRequest)
}

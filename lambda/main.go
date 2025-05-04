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

type (
	// Config holds environment variables
	Config struct {
		MongoDBPassword string `envconfig:"MONGODB_PASSWORD" required:"true"`
		NtfyServer      string `envconfig:"NTFY_SERVER" default:"https://ntfy.sh"`
	}

	// Subscription represents a subscription document
	Subscription struct {
		ID        string    `bson:"_id"`
		Location  string    `bson:"location"`
		NtfyTopic string    `bson:"ntfyTopic"`
		CreatedAt time.Time `bson:"createdAt"`
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
			Timeout: 10 * time.Second,
		},
	}
}

// checkAvailabilityAndNotify checks appointment availability and notifies topics
func (h *LambdaHandler) checkAvailabilityAndNotify(ctx context.Context, location string, topics []string) error {
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
				message := fmt.Sprintf("Appointment available at %s on %s", location, appointments[0].StartTimestamp)
				payload := map[string]string{
					"topic":   topic,
					"message": message,
					"title":   "Global Entry Appointment Notification",
				}
				payloadBytes, _ := json.Marshal(payload)

				for ntfyAttempt := 1; ntfyAttempt <= 3; ntfyAttempt++ {
					ntfyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Config.NtfyServer, bytes.NewBuffer(payloadBytes))
					if err != nil {
						return fmt.Errorf("failed to create ntfy request: %v", err)
					}
					ntfyReq.Header.Set("Content-Type", "application/json")

					ntfyResp, err := h.HTTPClient.Do(ntfyReq)
					if err != nil {
						slog.Warn("Failed to send ntfy notification", "topic", topic, "attempt", ntfyAttempt, "error", err)
						if ntfyAttempt == 3 {
							return fmt.Errorf("failed to send ntfy notification after %d attempts: %v", ntfyAttempt, err)
						}
						time.Sleep(time.Duration(ntfyAttempt) * 100 * time.Millisecond)
						continue
					}
					ntfyResp.Body.Close()
					if ntfyResp.StatusCode == http.StatusOK {
						slog.Info("Sent notification", "topic", topic, "location", location)
						break
					}
					slog.Warn("Non-OK status from ntfy", "topic", topic, "status", ntfyResp.StatusCode)
				}
			}
		}
		return nil
	}
	return nil
}

// handleExpiringSubscriptions deletes subscriptions exactly 30 days old and notifies
func (h *LambdaHandler) handleExpiringSubscriptions(ctx context.Context, coll *mongo.Collection) error {
	now := time.Now().UTC()
	// Calculate the 5-minute window for subscriptions exactly 30 days old
	ttlThreshold := now.Add(-30 * 24 * time.Hour)         // 30 days ago
	expireStart := ttlThreshold.Truncate(5 * time.Minute) // Start of the 5-minute block
	expireEnd := expireStart.Add(5 * time.Minute)         // End of the 5-minute block

	filter := bson.M{
		"createdAt": bson.M{
			"$gte": expireStart,
			"$lt":  expireEnd,
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
		// Send expiration notification
		message := "Your Global Entry appointment subscription has expired."
		payload := map[string]string{
			"topic":   sub.NtfyTopic,
			"message": message,
			"title":   "Global Entry Subscription Expired",
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
				slog.Warn("Failed to send expiration notification", "topic", sub.NtfyTopic, "attempt", attempt, "error", err)
				if attempt == 3 {
					slog.Error("Failed to send expiration notification after 3 attempts", "topic", sub.NtfyTopic, "error", err)
					continue
				}
				time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("Sent expiration notification", "topic", sub.NtfyTopic)
				break
			}
			slog.Warn("Non-OK status from ntfy", "topic", sub.NtfyTopic, "status", resp.StatusCode)
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
			Body:       `{"error": "location and ntfyTopic are required"}`,
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
				Body:       `{"error": "subscription already exists"}`,
			}, nil
		}

		// Insert new subscription
		_, err = coll.InsertOne(ctx, bson.M{
			"location":  req.Location,
			"ntfyTopic": req.NtfyTopic,
			"createdAt": time.Now().UTC(),
		})
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 500}, fmt.Errorf("failed to insert subscription: %v", err)
		}
		slog.Info("Added subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
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
				Body:       `{"error": "subscription not found"}`,
			}, nil
		}
		slog.Info("Removed subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Body:       `{"message": "Unsubscribed successfully"}`,
		}, nil

	default:
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Body:       `{"error": "invalid action, use subscribe or unsubscribe"}`,
		}, nil
	}
}

// HandleRequest handles Scheduled Events and API requests
func (h *LambdaHandler) HandleRequest(ctx context.Context, event json.RawMessage) (events.APIGatewayV2HTTPResponse, error) {
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")

	// Log raw event
	slog.Info("Received event", "event", string(event))

	// Parse event as JSON map
	var eventMap map[string]interface{}
	if err := json.Unmarshal(event, &eventMap); err != nil {
		slog.Error("Failed to parse event as JSON", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Body:       `{"error": "invalid event format"}`,
		}, nil
	}

	// Check for CloudWatch Event
	if source, ok := eventMap["source"].(string); ok && source == "aws.events" {
		slog.Info("Parsed as CloudWatch event", "source", source)
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
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		httpInfo, ok := requestContext["http"].(map[string]interface{})
		if !ok {
			slog.Error("Missing http info in requestContext")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		method, ok := httpInfo["method"].(string)
		if !ok {
			slog.Error("Missing method in http info")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		body, _ := eventMap["body"].(string)

		slog.Info("Parsed as HTTP event", "rawPath", rawPath, "method", method, "body", body)

		if method == "POST" && strings.HasSuffix(rawPath, "/subscriptions") {
			if body == "" {
				slog.Error("Invalid request: missing body")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Body:       `{"error": "missing request body"}`,
				}, nil
			}
			var subReq SubscriptionRequest
			if err := json.Unmarshal([]byte(body), &subReq); err != nil {
				slog.Error("Failed to parse request body", "body", body, "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Body:       `{"error": "invalid request body"}`,
				}, nil
			}
			if subReq.Action == "" || subReq.Location == "" || subReq.NtfyTopic == "" {
				slog.Error("Invalid subscription request: missing required fields")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Body:       `{"error": "missing required fields"}`,
				}, nil
			}
			slog.Info("Calling handleSubscription", "action", subReq.Action, "location", subReq.Location)
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
						slog.Error("Failed to marshal response body", "body", body, "error", err)
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
	defer client.Disconnect(context.Background())

	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	handler := NewLambdaHandler(config, url, client)
	lambda.Start(handler.HandleRequest)
}

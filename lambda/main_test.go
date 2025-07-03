package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// setupTestHandler creates a LambdaHandler and MongoDB container for testing
func setupTestHandler(t *testing.T) (*LambdaHandler, *mongo.Collection, func()) {
	t.Helper()

	// Disable logging
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Start MongoDB container
	ctx := context.Background()
	mongodbContainer, err := mongodb.Run(ctx, "mongo:7")
	if err != nil {
		t.Fatalf("failed to start MongoDB container: %v", err)
	}

	// Get connection string
	connString, err := mongodbContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get MongoDB connection string: %v", err)
	}

	// Create MongoDB client
	client, err := mongo.Connect(options.Client().ApplyURI(connString).SetConnectTimeout(10 * time.Second))
	if err != nil {
		t.Fatalf("failed to connect to MongoDB: %v", err)
	}

	// Create collection
	coll := client.Database("global-entry-appointment-db").Collection("subscriptions")

	// Configure handler
	config := Config{
		MongoDBPassword: "test",
		NtfyServer:      "http://localhost", // Will be overridden by httptest
	}
	url := "http://localhost/%s" // Will be overridden by httptest
	handler := NewLambdaHandler(config, url, client)

	// Cleanup function
	cleanup := func() {
		client.Disconnect(ctx)
		mongodbContainer.Terminate(ctx)
	}

	return handler, coll, cleanup
}

func TestHandleRequest_CloudWatchEvent(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test data
	_, err := coll.InsertMany(ctx, []interface{}{
		bson.M{"location": "JFK", "ntfyTopic": "user1-jfk", "createdAt": time.Now().UTC()},
		bson.M{"location": "JFK", "ntfyTopic": "user2-jfk", "createdAt": time.Now().UTC()},
	})
	assert.NoError(t, err)

	// Mock HTTP server for Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: 123, StartTimestamp: "2025-05-04T10:00:00Z", Active: true},
		})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "Global Entry Appointment Notification", payload["title"])
		assert.Contains(t, payload["message"], "Appointment available at JFK")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = &http.Client{Timeout: 2 * time.Second}

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "cloudwatch event processed"}`, resp.Body)

	// Verify ntfy calls
	assert.Equal(t, 2, ntfyCalls, "Expected two ntfy notifications")
}

func TestHandleRequest_APIGatewaySubscribe(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Create API Gateway V2 request
	req := SubscriptionRequest{Action: "subscribe", Location: "JFK", NtfyTopic: "user1-jfk"}
	body, _ := json.Marshal(req)
	apiReq := events.APIGatewayV2HTTPRequest{
		Version:  "2.0",
		RouteKey: "POST /subscriptions",
		RawPath:  "/subscriptions",
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/subscriptions",
			},
		},
		Body:            string(body),
		IsBase64Encoded: false,
	}
	eventJSON, _ := json.Marshal(apiReq)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)

	// Verify response
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "Subscribed successfully"}`, resp.Body)

	// Verify subscription in database
	count, err := coll.CountDocuments(ctx, bson.M{"location": "JFK", "ntfyTopic": "user1-jfk"})
	assert.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestHandleRequest_APIGatewayUnsubscribe(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription
	_, err := coll.InsertOne(ctx, bson.M{
		"location":  "JFK",
		"ntfyTopic": "user1-jfk",
		"createdAt": time.Now().UTC(),
	})
	assert.NoError(t, err)

	// Create API Gateway V2 request
	req := SubscriptionRequest{Action: "unsubscribe", Location: "JFK", NtfyTopic: "user1-jfk"}
	body, _ := json.Marshal(req)
	apiReq := events.APIGatewayV2HTTPRequest{
		Version:  "2.0",
		RouteKey: "POST /subscriptions",
		RawPath:  "/subscriptions",
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/subscriptions",
			},
		},
		Body:            string(body),
		IsBase64Encoded: false,
	}
	eventJSON, _ := json.Marshal(apiReq)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)

	// Verify response
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "Unsubscribed successfully"}`, resp.Body)

	// Verify subscription removed
	count, err := coll.CountDocuments(ctx, bson.M{"location": "JFK", "ntfyTopic": "user1-jfk"})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

func TestHandleRequest_InvalidEvent(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Invalid event
	eventJSON := []byte(`{}`)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)

	// Verify response
	assert.Equal(t, 400, resp.StatusCode)
	assert.JSONEq(t, `{"error": "unsupported event type"}`, resp.Body)
}

func TestCheckAvailabilityAndNotify_Success(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Mock Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: 123, StartTimestamp: "2025-05-04T10:00:00Z", Active: true},
		})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-jfk", payload["topic"])
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = &http.Client{Timeout: 2 * time.Second}

	// Call function
	err := handler.checkAvailabilityAndNotify(ctx, "JFK", []string{"user1-jfk"})
	assert.NoError(t, err)
	assert.Equal(t, 1, ntfyCalls)
}

func TestCheckAvailabilityAndNotify_NoAppointments(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Mock Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = &http.Client{Timeout: 2 * time.Second}

	// Call function
	err := handler.checkAvailabilityAndNotify(ctx, "JFK", []string{"user1-jfk"})
	assert.NoError(t, err)
	assert.Equal(t, 0, ntfyCalls)
}

func TestCheckAvailabilityAndNotify_APIFailure(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Mock Global Entry API (failing)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Server Error", http.StatusInternalServerError)
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"
	handler.HTTPClient = &http.Client{Timeout: 2 * time.Second}

	// Call function
	err := handler.checkAvailabilityAndNotify(ctx, "JFK", []string{"user1-jfk"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "API returned status 500")
}

func TestHandleExpiringSubscriptions(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription (7 days old)
	expireTime := time.Now().UTC().Add(-7 * 24 * time.Hour)
	_, err := coll.InsertOne(ctx, bson.M{
		"_id":       "123",
		"location":  "JFK",
		"ntfyTopic": "user1-jfk",
		"createdAt": expireTime,
	})
	assert.NoError(t, err)

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-jfk", payload["topic"])
		assert.Equal(t, "Global Entry Subscription Expired", payload["title"])
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = &http.Client{Timeout: 2 * time.Second}

	// Call function
	err = handler.handleExpiringSubscriptions(ctx, coll)
	assert.NoError(t, err)
	assert.Equal(t, 1, ntfyCalls)

	// Verify subscription removed
	count, err := coll.CountDocuments(ctx, bson.M{"_id": "123"})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

func TestHandleSubscription_InvalidInput(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Test missing location
	req := SubscriptionRequest{Action: "subscribe", NtfyTopic: "user1-jfk"}
	resp, err := handler.handleSubscription(ctx, coll, req)
	assert.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
	assert.JSONEq(t, `{"error": "location and ntfyTopic are required"}`, resp.Body)
}

func TestHandleSubscription_Duplicate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert existing subscription
	_, err := coll.InsertOne(ctx, bson.M{
		"location":  "JFK",
		"ntfyTopic": "user1-jfk",
		"createdAt": time.Now().UTC(),
	})
	assert.NoError(t, err)

	// Test duplicate subscription
	req := SubscriptionRequest{Action: "subscribe", Location: "JFK", NtfyTopic: "user1-jfk"}
	resp, err := handler.handleSubscription(ctx, coll, req)
	assert.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
	assert.JSONEq(t, `{"error": "subscription already exists"}`, resp.Body)
}

func TestHandleSubscription_UnsubscribeNotFound(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Test unsubscribe not found
	req := SubscriptionRequest{Action: "unsubscribe", Location: "JFK", NtfyTopic: "user1-jfk"}
	resp, err := handler.handleSubscription(ctx, coll, req)
	assert.NoError(t, err)
	assert.Equal(t, 404, resp.StatusCode)
	assert.JSONEq(t, `{"error": "subscription not found"}`, resp.Body)
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

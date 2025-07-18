package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// newRetryableHTTPClient creates a configured retryable HTTP client
func newRetryableHTTPClient(config Config) *retryablehttp.Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = config.MaxRetries
	retryClient.RetryWaitMin = 100 * time.Millisecond
	retryClient.RetryWaitMax = 200 * time.Millisecond
	retryClient.HTTPClient.Timeout = config.HTTPTimeout
	retryClient.Logger = nil
	return retryClient
}

// insertSubscription inserts a subscription into the collection and returns its ID
func insertSubscription(t *testing.T, ctx context.Context, coll *mongo.Collection, location, shortName, timezone, ntfyTopic string, createdAt, lastNotifiedAt time.Time) bson.ObjectID {
	t.Helper()
	id := bson.NewObjectID()
	_, err := coll.InsertOne(ctx, bson.M{
		"_id":            id,
		"location":       location,
		"shortName":      shortName,
		"timezone":       timezone,
		"ntfyTopic":      ntfyTopic,
		"createdAt":      createdAt,
		"lastNotifiedAt": lastNotifiedAt,
	})
	assert.NoError(t, err, "Failed to insert subscription")
	return id
}

// setupTestHandler creates a LambdaHandler and MongoDB container for testing
func setupTestHandler(t *testing.T) (*LambdaHandler, *mongo.Collection, func()) {
	t.Helper()

	// Enable debug logging for tests
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))

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
		MongoDBPassword:          "test",
		NtfyServer:               "http://localhost",
		HTTPTimeout:              2 * time.Second,
		NotificationCooldownTime: 60 * time.Minute,
		MongoConnectTimeout:      10 * time.Second,
		SubscriptionTTL:          30 * 24 * time.Hour,
		MaxNotifications:         10,
		MaxRetries:               1,
		MaxNtfyFailures:          2,
	}
	url := "http://localhost/%s"
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

	// Insert test data: subscriptions across different locations with varying lastNotifiedAt
	now := time.Now().UTC()
	// Oldest subscription (user2-sf, San Francisco)
	insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user2-sf", now, now.Add(-(handler.Config.NotificationCooldownTime + 5*time.Minute)))
	// Middle subscription (user1-ny, New York)
	insertSubscription(t, ctx, coll, "5001", "NY Enrollment Center", "America/New_York", "user1-ny", now, now.Add(-(handler.Config.NotificationCooldownTime + 1*time.Minute)))
	// Newest subscription (user3-sf, San Francisco, within cooldown)
	insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user3-sf", now, now.Add(-(handler.Config.NotificationCooldownTime - 10*time.Minute)))

	// Mock Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract location from URL
		parts := strings.Split(r.URL.Path, "/")
		location := parts[len(parts)-1]
		var locationID int
		if location == "5446" {
			locationID = 5446
		} else if location == "5001" {
			locationID = 5001
		} else {
			http.Error(w, "Invalid location", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: locationID, StartTimestamp: "2025-07-22T14:00", EndTimestamp: "2025-07-22T14:10", Active: true, Duration: 10, RemoteInd: false},
		})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server
	ntfyCalls := 0
	ntfyTopics := []string{}
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		ntfyTopics = append(ntfyTopics, payload["topic"])
		assert.Equal(t, "Global Entry Appointment Notification", payload["title"])
		if payload["topic"] == "user2-sf" {
			assert.Contains(t, payload["message"], "Appointment available at SF Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM PDT")
		} else if payload["topic"] == "user1-ny" {
			assert.Contains(t, payload["message"], "Appointment available at NY Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM EDT")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "cloudwatch event processed"}`, resp.Body)

	// Verify ntfy calls (only for user2-sf and user1-ny, as user3-sf is within cooldown)
	assert.Equal(t, 2, ntfyCalls, "Expected two ntfy notifications")
	// Verify order of notifications (user2-sf should be first, then user1-ny)
	assert.Equal(t, []string{"user2-sf", "user1-ny"}, ntfyTopics, "Expected notifications in order of oldest lastNotifiedAt")

	// Verify lastNotifiedAt was updated for user2-sf and user1-ny
	var sub1, sub2, sub3 Subscription
	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user2-sf"}).Decode(&sub1)
	assert.NoError(t, err)
	assert.True(t, sub1.LastNotifiedAt.After(now.Add(-handler.Config.NotificationCooldownTime)), "Expected lastNotifiedAt to be updated for user2-sf")

	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user1-ny"}).Decode(&sub2)
	assert.NoError(t, err)
	assert.True(t, sub2.LastNotifiedAt.After(now.Add(-handler.Config.NotificationCooldownTime)), "Expected lastNotifiedAt to be updated for user1-ny")

	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user3-sf"}).Decode(&sub3)
	assert.NoError(t, err)
	assert.True(t, sub3.LastNotifiedAt.Before(now.Add(-(handler.Config.NotificationCooldownTime - 10*time.Minute))), "Expected lastNotifiedAt to remain unchanged for user3-sf")
}

func TestHandleRequest_CloudWatchEvent_WithinCooldown(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test data: subscription within cooldown period
	now := time.Now().UTC()
	insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-5*time.Minute))

	// Mock Global Entry API with available appointment
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: 5446, StartTimestamp: "2025-07-22T14:00", EndTimestamp: "2025-07-22T14:10", Active: true, Duration: 10, RemoteInd: false},
		})
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
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "cloudwatch event processed"}`, resp.Body)

	// Verify no ntfy calls (since subscription is within cooldown)
	assert.Equal(t, 0, ntfyCalls, "Expected no ntfy notifications due to cooldown")

	// Verify lastNotifiedAt was not updated
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user1-sf"}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.Before(now.Add(-4*time.Minute)), "Expected lastNotifiedAt to remain unchanged for user1-sf")
	assert.True(t, sub.LastNotifiedAt.After(now.Add(-6*time.Minute)), "Expected lastNotifiedAt to remain around 5 minutes ago")
}

func TestHandleRequest_APIGatewaySubscribe(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-sf", payload["topic"])
		assert.Equal(t, "Global Entry Subscription Confirmation", payload["title"])
		assert.Contains(t, payload["message"], "You're all set! We'll notify you when an appointment slot is available at SF Enrollment Center.")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create API Gateway V2 request
	req := SubscriptionRequest{Action: "subscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles", NtfyTopic: "user1-sf"}
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
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"location": "5446", "ntfyTopic": "user1-sf"}).Decode(&sub)
	assert.NoError(t, err)
	assert.Equal(t, "5446", sub.Location)
	assert.Equal(t, "SF Enrollment Center", sub.ShortName)
	assert.Equal(t, "America/Los_Angeles", sub.Timezone)
	assert.Equal(t, "user1-sf", sub.NtfyTopic)
	assert.False(t, sub.CreatedAt.IsZero(), "Expected createdAt to be set")
	assert.True(t, sub.LastNotifiedAt.Before(time.Now().UTC().Add(-(handler.Config.NotificationCooldownTime - 1*time.Minute))), fmt.Sprintf("Expected lastNotifiedAt to be ~%d minutes ago", handler.Config.NotificationCooldownTime))
	assert.True(t, sub.LastNotifiedAt.After(time.Now().UTC().Add(-(handler.Config.NotificationCooldownTime + 1*time.Minute))), fmt.Sprintf("Expected lastNotifiedAt to be ~%d minutes ago", handler.Config.NotificationCooldownTime))

	// Verify ntfy notification
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification for subscription confirmation")
}

func TestHandleRequest_APIGatewayUnsubscribe(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription
	now := time.Now().UTC()
	_ = insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-handler.Config.NotificationCooldownTime))

	// Create API Gateway V2 request
	req := SubscriptionRequest{Action: "unsubscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles", NtfyTopic: "user1-sf"}
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
	count, err := coll.CountDocuments(ctx, bson.M{"location": "5446", "ntfyTopic": "user1-sf"})
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
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription
	now := time.Now().UTC()
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-handler.Config.NotificationCooldownTime))

	// Create subscription slice
	subscriptions := []Subscription{{
		ID:             id,
		Location:       "5446",
		ShortName:      "SF Enrollment Center",
		Timezone:       "America/Los_Angeles",
		NtfyTopic:      "user1-sf",
		CreatedAt:      now,
		LastNotifiedAt: now.Add(-handler.Config.NotificationCooldownTime),
	}}

	// Mock Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: 5446, StartTimestamp: "2025-07-22T14:00", EndTimestamp: "2025-07-22T14:10", Active: true, Duration: 10, RemoteInd: false},
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
		assert.Equal(t, "user1-sf", payload["topic"])
		assert.Equal(t, "Global Entry Appointment Notification", payload["title"])
		assert.Contains(t, payload["message"], "Appointment available at SF Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM PDT")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Call function
	globalNotifiedCount := 0
	var err error
	globalNotifiedCount, err = handler.checkAvailabilityAndNotify(ctx, subscriptions, globalNotifiedCount)
	assert.NoError(t, err)
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification")
	assert.Equal(t, 1, globalNotifiedCount, "Expected globalNotifiedCount to be incremented")

	// Verify lastNotifiedAt was updated
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"_id": id}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.After(now.Add(-handler.Config.NotificationCooldownTime)), "Expected lastNotifiedAt to be updated")
}

func TestCheckAvailabilityAndNotify_NoAppointments(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription
	now := time.Now().UTC()
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-handler.Config.NotificationCooldownTime))

	// Create subscription slice
	subscriptions := []Subscription{{
		ID:             id,
		Location:       "5446",
		ShortName:      "SF Enrollment Center",
		Timezone:       "America/Los_Angeles",
		NtfyTopic:      "user1-sf",
		CreatedAt:      now,
		LastNotifiedAt: now.Add(-handler.Config.NotificationCooldownTime),
	}}

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
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Call function
	globalNotifiedCount := 0
	var err error
	globalNotifiedCount, err = handler.checkAvailabilityAndNotify(ctx, subscriptions, globalNotifiedCount)
	assert.NoError(t, err)
	assert.Equal(t, 0, ntfyCalls, "Expected no ntfy notifications")
	assert.Equal(t, 0, globalNotifiedCount, "Expected globalNotifiedCount to remain zero")
}

func TestCheckAvailabilityAndNotify_APIFailure(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription
	now := time.Now().UTC()
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-handler.Config.NotificationCooldownTime))

	// Create subscription slice
	subscriptions := []Subscription{{
		ID:             id,
		Location:       "5446",
		ShortName:      "SF Enrollment Center",
		Timezone:       "America/Los_Angeles",
		NtfyTopic:      "user1-sf",
		CreatedAt:      now,
		LastNotifiedAt: now.Add(-handler.Config.NotificationCooldownTime),
	}}

	// Mock Global Entry API (failing)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Server Error", http.StatusInternalServerError)
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Create HTTP client with custom retry policy to not retry on 500 status
	httpClient := newRetryableHTTPClient(handler.Config)
	httpClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if resp != nil && resp.StatusCode == http.StatusInternalServerError {
			return false, nil // Do not retry on 500 status
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}
	handler.HTTPClient = httpClient

	// Call function
	globalNotifiedCount := 0
	var err error
	globalNotifiedCount, err = handler.checkAvailabilityAndNotify(ctx, subscriptions, globalNotifiedCount)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch appointments for all locations")
	assert.Contains(t, err.Error(), "location 5446: API returned status 500")
	assert.Equal(t, 0, globalNotifiedCount, "Expected globalNotifiedCount to remain zero")
}

func TestCheckAvailabilityAndNotify_GreedyFetching(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Override MaxNotifications to a low value
	handler.Config.MaxNotifications = 3

	// Insert test subscriptions: 6 subscriptions across 3 locations
	now := time.Now().UTC()
	locations := []struct {
		id             bson.ObjectID
		location       string
		ntfyTopic      string
		lastNotifiedAt time.Time
	}{
		{id: insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-(handler.Config.NotificationCooldownTime + 5*time.Minute))), location: "5446", ntfyTopic: "user1-sf", lastNotifiedAt: now.Add(-(handler.Config.NotificationCooldownTime + 5*time.Minute))},
		{id: insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user2-sf", now, now.Add(-(handler.Config.NotificationCooldownTime + 4*time.Minute))), location: "5446", ntfyTopic: "user2-sf", lastNotifiedAt: now.Add(-(handler.Config.NotificationCooldownTime + 4*time.Minute))},
		{id: insertSubscription(t, ctx, coll, "5001", "NY Enrollment Center", "America/New_York", "user1-ny", now, now.Add(-(handler.Config.NotificationCooldownTime + 3*time.Minute))), location: "5001", ntfyTopic: "user1-ny", lastNotifiedAt: now.Add(-(handler.Config.NotificationCooldownTime + 3*time.Minute))},
		{id: insertSubscription(t, ctx, coll, "5001", "NY Enrollment Center", "America/New_York", "user2-ny", now, now.Add(-(handler.Config.NotificationCooldownTime + 2*time.Minute))), location: "5001", ntfyTopic: "user2-ny", lastNotifiedAt: now.Add(-(handler.Config.NotificationCooldownTime + 2*time.Minute))},
		{id: insertSubscription(t, ctx, coll, "1234", "Chicago Enrollment Center", "America/Chicago", "user1-chi", now, now.Add(-(handler.Config.NotificationCooldownTime + 1*time.Minute))), location: "1234", ntfyTopic: "user1-chi", lastNotifiedAt: now.Add(-(handler.Config.NotificationCooldownTime + 1*time.Minute))},
		{id: insertSubscription(t, ctx, coll, "1234", "Chicago Enrollment Center", "America/Chicago", "user2-chi", now, now.Add(-handler.Config.NotificationCooldownTime)), location: "1234", ntfyTopic: "user2-chi", lastNotifiedAt: now.Add(-handler.Config.NotificationCooldownTime)},
	}

	// Create subscription slice, sorted by lastNotifiedAt (oldest first)
	subscriptions := make([]Subscription, len(locations))
	shortNameMap := map[string]string{
		"5446": "SF Enrollment Center",
		"5001": "NY Enrollment Center",
		"1234": "Chicago Enrollment Center",
	}
	timezoneMap := map[string]string{
		"5446": "America/Los_Angeles",
		"5001": "America/New_York",
		"1234": "America/Chicago",
	}
	for i, loc := range locations {
		subscriptions[i] = Subscription{
			ID:             loc.id,
			Location:       loc.location,
			ShortName:      shortNameMap[loc.location],
			Timezone:       timezoneMap[loc.location],
			NtfyTopic:      loc.ntfyTopic,
			CreatedAt:      now,
			LastNotifiedAt: loc.lastNotifiedAt,
		}
	}

	// Mock Global Entry API with fetch counter
	fetchCalls := 0
	fetchedLocations := make(map[string]bool)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCalls++
		parts := strings.Split(r.URL.Path, "/")
		location := parts[len(parts)-1]
		fetchedLocations[location] = true
		var locationID int
		switch location {
		case "5446":
			locationID = 5446
		case "5001":
			locationID = 5001
		case "1234":
			locationID = 1234
		default:
			http.Error(w, "Invalid location", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: locationID, StartTimestamp: "2025-07-22T14:00", EndTimestamp: "2025-07-22T14:10", Active: true, Duration: 10, RemoteInd: false},
		})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server
	ntfyCalls := 0
	ntfyTopics := []string{}
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		ntfyTopics = append(ntfyTopics, payload["topic"])
		assert.Equal(t, "Global Entry Appointment Notification", payload["title"])
		if payload["topic"] == "user1-sf" {
			assert.Contains(t, payload["message"], "Appointment available at SF Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM PDT")
		} else if payload["topic"] == "user2-sf" {
			assert.Contains(t, payload["message"], "Appointment available at SF Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM PDT")
		} else if payload["topic"] == "user1-ny" {
			assert.Contains(t, payload["message"], "Appointment available at NY Enrollment Center on Tue, Jul 22, 2025 at 2:00 PM EDT")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Call function
	globalNotifiedCount := 0
	var err error
	globalNotifiedCount, err = handler.checkAvailabilityAndNotify(ctx, subscriptions, globalNotifiedCount)
	assert.NoError(t, err)
	assert.Equal(t, handler.Config.MaxNotifications, ntfyCalls, "Expected %d ntfy notifications", handler.Config.MaxNotifications)
	assert.Equal(t, handler.Config.MaxNotifications, globalNotifiedCount, "Expected globalNotifiedCount to be %d", handler.Config.MaxNotifications)
	assert.LessOrEqual(t, fetchCalls, 2, "Expected at most 2 fetch calls (for 5446 and 5001)")

	// Verify fetched locations
	expectedLocations := map[string]bool{"5446": true, "5001": true}
	assert.Equal(t, expectedLocations, fetchedLocations, "Expected fetches only for locations 5446 and 5001")

	// Verify lastNotifiedAt was updated for notified subscriptions
	for _, topic := range []string{"user1-sf", "user2-sf", "user1-ny"} {
		var sub Subscription
		err = coll.FindOne(ctx, bson.M{"ntfyTopic": topic}).Decode(&sub)
		assert.NoError(t, err)
		assert.True(t, sub.LastNotifiedAt.After(now.Add(-handler.Config.NotificationCooldownTime)), "Expected lastNotifiedAt to be updated for %s", topic)
	}

	// Verify lastNotifiedAt was not updated for non-notified subscriptions
	for _, topic := range []string{"user2-ny", "user1-chi", "user2-chi"} {
		var sub Subscription
		err = coll.FindOne(ctx, bson.M{"ntfyTopic": topic}).Decode(&sub)
		assert.NoError(t, err)
		assert.True(t, sub.LastNotifiedAt.Before(now.Add(-(handler.Config.NotificationCooldownTime - 1*time.Minute))), "Expected lastNotifiedAt to remain unchanged for %s", topic)
	}

	// Verify notification order
	assert.Equal(t, []string{"user1-sf", "user2-sf", "user1-ny"}, ntfyTopics, "Expected notifications in order of oldest lastNotifiedAt")
}

func TestHandleExpiringSubscriptions(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription (older than SubscriptionTTL)
	expireTime := time.Now().UTC().Add(-(handler.Config.SubscriptionTTL + 24*time.Hour))
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", expireTime, expireTime)

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-sf", payload["topic"])
		assert.Equal(t, "Global Entry Subscription Expired", payload["title"])
		assert.Contains(t, payload["message"], "Your Global Entry appointment subscription has expired")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Call function
	err := handler.handleExpiringSubscriptions(ctx, coll)
	assert.NoError(t, err)
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification")

	// Verify subscription removed
	count, err := coll.CountDocuments(ctx, bson.M{"_id": id})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count, "Expected subscription to be removed")
}

func TestHandleExpiringSubscriptions_NotYetExpired(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscription (less than SubscriptionTTL)
	createdTime := time.Now().UTC().Add(-(handler.Config.SubscriptionTTL - 24*time.Hour))
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", createdTime, createdTime.Add(-handler.Config.NotificationCooldownTime))

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Call function
	err := handler.handleExpiringSubscriptions(ctx, coll)
	assert.NoError(t, err)
	assert.Equal(t, 0, ntfyCalls, "Expected no ntfy notifications")

	// Verify subscription still exists
	count, err := coll.CountDocuments(ctx, bson.M{"_id": id})
	assert.NoError(t, err)
	assert.Equal(t, int64(1), count, "Expected subscription to remain")
}

func TestHandleSubscription_InvalidInput(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Test cases for invalid inputs
	tests := []struct {
		name     string
		req      SubscriptionRequest
		expected string
	}{
		{
			name:     "Missing location",
			req:      SubscriptionRequest{Action: "subscribe", NtfyTopic: "user1-sf"},
			expected: "location and ntfyTopic are required",
		},
		{
			name:     "Missing ntfyTopic",
			req:      SubscriptionRequest{Action: "subscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles"},
			expected: "location and ntfyTopic are required",
		},
		{
			name:     "Invalid ntfyTopic",
			req:      SubscriptionRequest{Action: "subscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles", NtfyTopic: "user1 sf"},
			expected: "Ntfy Topic must not contain spaces or special characters",
		},
		{
			name:     "Missing shortName",
			req:      SubscriptionRequest{Action: "subscribe", Location: "5446", Timezone: "America/Los_Angeles", NtfyTopic: "user1-sf"},
			expected: "shortName and timezone must be provided from location data",
		},
		{
			name:     "Missing timezone",
			req:      SubscriptionRequest{Action: "subscribe", Location: "5446", ShortName: "SF Enrollment Center", NtfyTopic: "user1-sf"},
			expected: "shortName and timezone must be provided from location data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := handler.handleSubscription(ctx, coll, tt.req)
			assert.NoError(t, err)
			assert.Equal(t, 400, resp.StatusCode)
			assert.JSONEq(t, fmt.Sprintf(`{"error": %q}`, tt.expected), resp.Body)
		})
	}
}

func TestHandleSubscription_Duplicate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert existing subscription
	now := time.Now().UTC()
	_ = insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-handler.Config.NotificationCooldownTime))

	// Test duplicate subscription
	req := SubscriptionRequest{Action: "subscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles", NtfyTopic: "user1-sf"}
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
	req := SubscriptionRequest{Action: "unsubscribe", Location: "5446", ShortName: "SF Enrollment Center", Timezone: "America/Los_Angeles", NtfyTopic: "user1-sf"}
	resp, err := handler.handleSubscription(ctx, coll, req)
	assert.NoError(t, err)
	assert.Equal(t, 404, resp.StatusCode)
	assert.JSONEq(t, `{"error": "subscription not found"}`, resp.Body)
}

func TestHandleRequest_MaxNtfyFailures(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Insert test subscriptions for 10 locations
	now := time.Now().UTC()
	locations := []string{"5446", "1234", "5678", "9012", "3456", "7890", "2345", "6789", "0123", "4567"}
	for i, loc := range locations {
		insertSubscription(t, ctx, coll, loc, "SF Enrollment Center", "America/Los_Angeles", fmt.Sprintf("user%d-sf", i+1), now, now.Add(-handler.Config.NotificationCooldownTime))
	}

	// Mock Global Entry API
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{LocationID: 5446, StartTimestamp: "2025-07-22T14:00", EndTimestamp: "2025-07-22T14:10", Active: true, Duration: 10, RemoteInd: false},
		})
	}))
	defer apiServer.Close()
	handler.URL = apiServer.URL + "/%s"

	// Mock ntfy server to fail all requests
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		http.Error(w, "Ntfy Server Error", http.StatusInternalServerError)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrMaxNtfyFailures), "Expected ErrMaxNtfyFailures")
	assert.Equal(t, 500, resp.StatusCode)
	assert.JSONEq(t, `{"error": "maximum notification failures reached"}`, resp.Body)

	// Verify failure count and ntfy calls
	assert.Equal(t, handler.Config.MaxNtfyFailures, handler.failedNtfyCount, "Expected exactly %d failures", handler.Config.MaxNtfyFailures)
	assert.Equal(t, handler.Config.MaxNtfyFailures*(handler.Config.MaxRetries+1), ntfyCalls, "Expected exactly %d ntfy attempts (%d failures with %d retries each)", handler.Config.MaxNtfyFailures*(handler.Config.MaxRetries+1), handler.Config.MaxNtfyFailures, handler.Config.MaxRetries)

	// Verify no subscriptions were updated (since all notifications failed)
	for i, loc := range locations {
		var sub Subscription
		err := coll.FindOne(ctx, bson.M{"ntfyTopic": fmt.Sprintf("user%d-sf", i+1)}).Decode(&sub)
		assert.NoError(t, err)
		assert.True(t, sub.LastNotifiedAt.Before(now.Add(-14*time.Minute)), "Expected lastNotifiedAt to remain unchanged for %s", loc)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

package main

import (
	"context"
	"encoding/json"
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
func insertSubscription(t *testing.T, ctx context.Context, coll *mongo.Collection, location, shortName, timezone, ntfyTopic string, createdAt, lastNotifiedAt, latestDate time.Time, notificationCount int) bson.ObjectID {
	t.Helper()
	id := bson.NewObjectID()
	_, err := coll.InsertOne(ctx, bson.M{
		"_id":               id,
		"location":          location,
		"shortName":         shortName,
		"timezone":          timezone,
		"ntfyTopic":         ntfyTopic,
		"createdAt":         createdAt,
		"lastNotifiedAt":    lastNotifiedAt,
		"latestDate":        latestDate,
		"notificationCount": notificationCount,
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
		RecaptchaURL:             "http://localhost/recaptcha",
		HTTPTimeout:              2 * time.Second,
		NotificationCooldownTime: 30 * time.Minute,
		MongoConnectTimeout:      10 * time.Second,
		SubscriptionTTL:          30 * 24 * time.Hour,
		MaxNotifications:         10,
		MaxRetries:               1,
		MaxNtfyFailures:          2,
		MaxNotificationCount:     30,
		RecaptchaSecretKey:       "test-recaptcha-secret",
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

// setupRecaptchaServer creates a mock reCAPTCHA server
func setupRecaptchaServer(t *testing.T, valid bool, score float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}

		secret := r.FormValue("secret")
		token := r.FormValue("response")

		if secret != "test-recaptcha-secret" {
			http.Error(w, "Invalid secret key", http.StatusUnauthorized)
			return
		}

		if token == "" {
			http.Error(w, "Missing token", http.StatusBadRequest)
			return
		}

		response := map[string]interface{}{
			"success": valid,
			"score":   score,
			"action":  "submit",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
}

// setupTest clears the MongoDB collection before each test
func setupTest(t *testing.T, coll *mongo.Collection) {
	t.Helper()
	ctx := context.Background()
	_, err := coll.DeleteMany(ctx, bson.M{})
	assert.NoError(t, err, "Failed to clear collection")
}

func TestHandleRequest_CloudWatchAvailabilityEvent_AppointmentWithinLatestDate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Insert test subscription with lastNotifiedAt and latestDate
	now := time.Now().UTC()
	latestDate := now.Add(30 * 24 * time.Hour) // 30 days from now
	insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-2*time.Hour), latestDate, 0)

	// Mock Global Entry API with dynamic appointment time (5 days from now)
	appointmentTime := now.Add(5 * 24 * time.Hour)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if parts[len(parts)-1] != "5446" {
			http.Error(w, "Invalid location", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]Appointment{
			{
				LocationID:     5446,
				StartTimestamp: appointmentTime.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				EndTimestamp:   appointmentTime.Add(10 * time.Minute).In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				Active:         true,
				Duration:       10,
				RemoteInd:      false,
			},
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
		assert.Contains(t, payload["message"], fmt.Sprintf("Appointment available at SF Enrollment Center on %s", appointmentTime.In(time.FixedZone("PDT", -7*3600)).Format("Mon, Jan 2, 2006 at 3:04 PM MST")))
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events.availability"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "availability check processed"}`, resp.Body)

	// Verify ntfy notification was sent
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification")

	// Verify subscription was updated
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user1-sf"}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.After(now.Add(-2*time.Hour)), "Expected lastNotifiedAt to be updated")
	assert.Equal(t, 1, sub.NotificationCount, "Expected notificationCount to be 1")
}

func TestHandleRequest_CloudWatchAvailabilityEvent_AppointmentBeyondLatestDate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Insert test subscription with lastNotifiedAt and latestDate
	now := time.Now().UTC()
	latestDate := now.Add(10 * 24 * time.Hour) // 10 days from now
	lastNotifiedAt := now.Add(-2 * time.Hour)
	insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, lastNotifiedAt, latestDate, 0)

	// Mock Global Entry API with appointment beyond latestDate
	appointmentTime := latestDate.Add(5 * 24 * time.Hour) // 15 days from now
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if parts[len(parts)-1] != "5446" {
			http.Error(w, "Invalid location", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]Appointment{
			{
				LocationID:     5446,
				StartTimestamp: appointmentTime.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				EndTimestamp:   appointmentTime.Add(10 * time.Minute).In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				Active:         true,
				Duration:       10,
				RemoteInd:      false,
			},
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
	event := events.CloudWatchEvent{Source: "aws.events.availability"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "availability check processed"}`, resp.Body)

	// Verify no ntfy notification was sent
	assert.Equal(t, 0, ntfyCalls, "Expected no ntfy notifications due to appointment beyond latestDate")

	// Verify subscription was not updated
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"ntfyTopic": "user1-sf"}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.Truncate(time.Millisecond).Equal(lastNotifiedAt.Truncate(time.Millisecond)), "Expected lastNotifiedAt to remain unchanged")
	assert.Equal(t, 0, sub.NotificationCount, "Expected notificationCount to remain 0")
}

func TestHandleRequest_APIGatewaySubscribe_DefaultFutureLatestDate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Mock reCAPTCHA server
	recaptchaServer := setupRecaptchaServer(t, true, 0.9)
	defer recaptchaServer.Close()
	handler.Config.RecaptchaURL = recaptchaServer.URL

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-sf", payload["topic"])
		assert.Equal(t, "Global Entry Subscription Confirmation", payload["title"])
		assert.Contains(t, payload["message"], "You're all set! We'll notify you when an appointment slot is available at SF Enrollment Center before")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create API Gateway V2 request
	now := time.Now().UTC()
	latestDate := now.Add(30 * 24 * time.Hour) // 30 days from now
	req := SubscriptionRequest{
		Action:         "subscribe",
		Location:       "5446",
		ShortName:      "SF Enrollment Center",
		Timezone:       "America/Los_Angeles",
		NtfyTopic:      "user1-sf",
		LatestDate:     latestDate.Format("2006-01-02"),
		RecaptchaToken: "valid-token",
	}
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
	assert.WithinDuration(t, latestDate, sub.LatestDate, 24*time.Hour, "Expected latestDate to be set to 30 days from now")
	assert.True(t, sub.LastNotifiedAt.Before(now), "Expected lastNotifiedAt to be in the past")
	assert.Equal(t, 1, sub.NotificationCount, "Expected notificationCount to be 1 due to confirmation notification")

	// Verify ntfy notification
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification for subscription confirmation")
}

func TestHandleRequest_CloudWatchAvailabilityEvent_MultipleScenarios(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Insert test subscriptions with different lastNotifiedAt and latestDate
	now := time.Now().UTC()
	latestDate := now.Add(30 * 24 * time.Hour)     // 30 days from now
	latestDateUser3 := now.Add(3 * 24 * time.Hour) // 3 days from now
	// Case 1: Appointment within latestDate, outside cooldown
	user1ID := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-2*time.Hour), latestDate, 0)
	// Case 2: Within cooldown, should not notify
	user2ID := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user2-sf", now, now.Add(-10*time.Minute), latestDate, 0)
	// Case 3: Appointment beyond latestDate, should not notify
	user3ID := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user3-sf", now, now.Add(-2*time.Hour), latestDateUser3, 0)

	// Mock Global Entry API with appointments
	withinRangeTime := now.Add(5 * 24 * time.Hour)  // 5 days from now, within user1-sf's latestDate
	beyondRangeTime := now.Add(10 * 24 * time.Hour) // 10 days from now, beyond user3-sf's latestDate
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if parts[len(parts)-1] != "5446" {
			http.Error(w, "Invalid location", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]Appointment{
			{
				LocationID:     5446,
				StartTimestamp: withinRangeTime.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				EndTimestamp:   withinRangeTime.Add(10 * time.Minute).In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				Active:         true,
				Duration:       10,
				RemoteInd:      false,
			},
			{
				LocationID:     5446,
				StartTimestamp: beyondRangeTime.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				EndTimestamp:   beyondRangeTime.Add(10 * time.Minute).In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				Active:         true,
				Duration:       10,
				RemoteInd:      false,
			},
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
		assert.Contains(t, payload["message"], fmt.Sprintf("Appointment available at SF Enrollment Center on %s", withinRangeTime.In(time.FixedZone("PDT", -7*3600)).Format("Mon, Jan 2, 2006 at 3:04 PM MST")))
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch event
	event := events.CloudWatchEvent{Source: "aws.events.availability"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "availability check processed"}`, resp.Body)

	// Verify only user1-sf was notified
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification")
	assert.Equal(t, []string{"user1-sf"}, ntfyTopics, "Expected notification only for user1-sf")

	// Verify subscription updates
	var sub Subscription
	// User1: Should be updated
	err = coll.FindOne(ctx, bson.M{"_id": user1ID}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.After(now.Add(-2*time.Hour)), "Expected lastNotifiedAt to be updated for user1-sf")
	assert.Equal(t, 1, sub.NotificationCount, "Expected notificationCount to be 1 for user1-sf")

	// User2: Should not be updated (within cooldown)
	err = coll.FindOne(ctx, bson.M{"_id": user2ID}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.Truncate(time.Millisecond).Equal(now.Add(-10*time.Minute).Truncate(time.Millisecond)), "Expected lastNotifiedAt to remain unchanged for user2-sf")
	assert.Equal(t, 0, sub.NotificationCount, "Expected notificationCount to remain 0 for user2-sf")

	// User3: Should not be updated (appointment beyond latestDate)
	err = coll.FindOne(ctx, bson.M{"_id": user3ID}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.Truncate(time.Millisecond).Equal(now.Add(-2*time.Hour).Truncate(time.Millisecond)), "Expected lastNotifiedAt to remain unchanged for user3-sf")
	assert.Equal(t, 0, sub.NotificationCount, "Expected notificationCount to remain 0 for user3-sf")
}

func TestHandleRequest_CloudWatchExpirationEvent(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Insert test subscription (past latestDate)
	now := time.Now().UTC()
	expiredLatestDate := now.Add(-1 * 24 * time.Hour) // Yesterday
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-2*time.Hour), expiredLatestDate, 0)

	// Mock ntfy server
	ntfyCalls := 0
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyCalls++
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		assert.Equal(t, "user1-sf", payload["topic"])
		assert.Equal(t, "Global Entry Subscription Expired", payload["title"])
		assert.Equal(t, fmt.Sprintf("Your Global Entry appointment subscription for SF Enrollment Center has expired(30 days subscription) or the latest appointment date set (%s) has passed.", expiredLatestDate.Format("2006-01-02")), payload["message"])
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	handler.Config.NtfyServer = ntfyServer.URL
	handler.HTTPClient = newRetryableHTTPClient(handler.Config)

	// Create CloudWatch expiration event
	event := events.CloudWatchEvent{Source: "aws.events.expiration"}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "expiration check processed"}`, resp.Body)

	// Verify ntfy notification
	assert.Equal(t, 1, ntfyCalls, "Expected one ntfy notification for expiration")

	// Verify subscription removed
	count, err := coll.CountDocuments(ctx, bson.M{"_id": id})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count, "Expected subscription to be removed")
}

func TestCheckAvailabilityAndNotify_AppointmentWithinLatestDate(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Insert test subscription
	now := time.Now().UTC()
	latestDate := now.Add(30 * 24 * time.Hour) // 30 days from now
	id := insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-2*time.Hour), latestDate, 0)

	// Create subscription slice
	subscriptions := []Subscription{{
		ID:                id,
		Location:          "5446",
		ShortName:         "SF Enrollment Center",
		Timezone:          "America/Los_Angeles",
		NtfyTopic:         "user1-sf",
		CreatedAt:         now,
		LastNotifiedAt:    now.Add(-2 * time.Hour),
		LatestDate:        latestDate,
		NotificationCount: 0,
	}}

	// Mock Global Entry API with appointment within latestDate
	appointmentTime := now.Add(5 * 24 * time.Hour) // 5 days from now
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Appointment{
			{
				LocationID:     5446,
				StartTimestamp: appointmentTime.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				EndTimestamp:   appointmentTime.Add(10 * time.Minute).In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04"),
				Active:         true,
				Duration:       10,
				RemoteInd:      false,
			},
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
		assert.Contains(t, payload["message"], fmt.Sprintf("Appointment available at SF Enrollment Center on %s", appointmentTime.In(time.FixedZone("PDT", -7*3600)).Format("Mon, Jan 2, 2006 at 3:04 PM MST")))
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

	// Verify subscription was updated
	var sub Subscription
	err = coll.FindOne(ctx, bson.M{"_id": id}).Decode(&sub)
	assert.NoError(t, err)
	assert.True(t, sub.LastNotifiedAt.After(now.Add(-2*time.Hour)), "Expected lastNotifiedAt to be updated")
	assert.Equal(t, 1, sub.NotificationCount, "Expected notificationCount to be 1")
}

func TestHandleRequest_APIGatewayUnsubscribe(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Mock reCAPTCHA server
	recaptchaServer := setupRecaptchaServer(t, true, 0.9)
	defer recaptchaServer.Close()
	handler.Config.RecaptchaURL = recaptchaServer.URL

	// Insert test subscription with future latestDate
	now := time.Now().UTC()
	latestDate := now.Add(30 * 24 * time.Hour) // 30 days from now
	_ = insertSubscription(t, ctx, coll, "5446", "SF Enrollment Center", "America/Los_Angeles", "user1-sf", now, now.Add(-2*time.Hour), latestDate, 0)

	// Create API Gateway V2 request
	req := SubscriptionRequest{
		Action:         "unsubscribe",
		Location:       "5446",
		ShortName:      "SF Enrollment Center",
		Timezone:       "America/Los_Angeles",
		NtfyTopic:      "user1-sf",
		RecaptchaToken: "valid-token",
	}
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
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"message": "Unsubscribed successfully"}`, resp.Body)

	// Verify subscription was removed
	count, err := coll.CountDocuments(ctx, bson.M{"location": "5446", "ntfyTopic": "user1-sf"})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count, "Expected subscription to be removed")
}

func TestHandleRequest_InvalidEvent(t *testing.T) {
	handler, coll, cleanup := setupTestHandler(t)
	defer cleanup()
	ctx := context.Background()

	// Clear collection
	setupTest(t, coll)

	// Create invalid event
	event := map[string]interface{}{
		"invalid": "event",
	}
	eventJSON, _ := json.Marshal(event)

	// Invoke handler
	resp, err := handler.HandleRequest(ctx, eventJSON)
	assert.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
	assert.JSONEq(t, `{"error": "invalid event format"}`, resp.Body)

	// Verify no subscriptions were created
	count, err := coll.CountDocuments(ctx, bson.M{})
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count, "Expected no subscriptions in database")
}

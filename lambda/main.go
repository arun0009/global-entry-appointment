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
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/kelseyhightower/envconfig"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/sync/errgroup"
)

const primarySiteOrigin = "https://getglobalentryalerts.com"

// supportURL is included in end-of-subscription notices — the one moment users
// are most grateful and most likely to support the project.
const supportURL = "https://buymeacoffee.com/arun0009"

// bookingURL is where slot alerts deep-link so users can book immediately.
const bookingURL = "https://ttp.dhs.gov/schedulerui/"

// cbpLocationsURL is the upstream CBP scheduler API for Global Entry enrollment centers.
const cbpLocationsURL = "https://ttp.cbp.dhs.gov/schedulerapi/locations/?temporary=false&inviteOnly=false&operational=true&serviceName=Global%20Entry"

// locationsCache is a package-level in-memory cache for the CBP locations list.
// Lambda execution environments are reused across invocations, so a warm instance
// serves the cached list without hitting CBP on every request.
var (
	locationsCacheMu  sync.RWMutex
	locationsCache    json.RawMessage
	locationsCachedAt time.Time
	locationsCacheTTL = 1 * time.Hour
)

// Browser Origin for GitHub project Pages is https://arun0009.github.io (path not included).
var allowedCORSOrigins = map[string]bool{
	primarySiteOrigin:                      true,
	"https://www.getglobalentryalerts.com": true,
	"https://arun0009.github.io":           true,
}

// originFromEvent extracts the request Origin header from an API Gateway event.
// HTTP API v2 lowercases header names; REST API v1 and SAM local don't, so we
// check both common spellings.
func originFromEvent(eventMap map[string]interface{}) string {
	h, _ := eventMap["headers"].(map[string]interface{})
	if v, _ := h["origin"].(string); v != "" {
		return v
	}
	if v, _ := h["Origin"].(string); v != "" {
		return v
	}
	return ""
}

// corsHeadersForOrigin returns the CORS response headers, echoing the request
// Origin when it's allow-listed and falling back to the primary site otherwise.
func corsHeadersForOrigin(origin string) map[string]string {
	allow := primarySiteOrigin
	if allowedCORSOrigins[origin] {
		allow = origin
	}
	return map[string]string{
		"Access-Control-Allow-Origin":      allow,
		"Access-Control-Allow-Methods":     "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers":     "Content-Type",
		"Access-Control-Allow-Credentials": "true",
	}
}

var validNtfyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// CBP location IDs are numeric; reject anything else to keep the upstream URL safe.
var validLocationPattern = regexp.MustCompile(`^[0-9]+$`)

// maxLocationFetchConcurrency caps parallel HTTP calls to the CBP API per cron run.
const maxLocationFetchConcurrency = 8

var ErrMaxNtfyFailures = errors.New("maximum ntfy failures reached")

type (
	// Config holds environment variables
	Config struct {
		MongoDBPassword          string        `envconfig:"MONGODB_PASSWORD" required:"true"`
		NtfyServer               string        `envconfig:"NTFY_SERVER" default:"https://ntfy.sh/"`
		HTTPTimeout              time.Duration `envconfig:"HTTP_TIMEOUT_SECONDS" default:"5s"`
		NotificationCooldownTime time.Duration `envconfig:"NOTIFICATION_COOLDOWN_TIME" default:"30m"`
		MongoConnectTimeout      time.Duration `envconfig:"MONGO_CONNECT_TIMEOUT" default:"10s"`
		SubscriptionTTL          time.Duration `envconfig:"SUBSCRIPTION_TTL_DAYS" default:"720h"`
		MaxNotifications         int           `envconfig:"MAX_NOTIFICATIONS" default:"10"`
		MaxRetries               int           `envconfig:"MAX_RETRIES" default:"1"`
		MaxNtfyFailures          int           `envconfig:"MAX_NTFY_FAILURES" default:"2"`
		MaxNotificationCount     int           `envconfig:"MAX_NOTIFICATION_COUNT" default:"30"`
		RecaptchaSecretKey       string        `envconfig:"RECAPTCHA_SECRET_KEY" required:"true"`
		RecaptchaURL             string        `envconfig:"RECAPTCHA_URL" default:"https://www.google.com/recaptcha/api/siteverify"`
		VAPIDPublicKey           string        `envconfig:"VAPID_PUBLIC_KEY"`
		VAPIDPrivateKey          string        `envconfig:"VAPID_PRIVATE_KEY"`
		VAPIDSubscriber          string        `envconfig:"VAPID_SUBJECT" default:"hello@getglobalentryalerts.com"`
	}

	// WebPushKeys holds the browser push subscription key material.
	WebPushKeys struct {
		Auth   string `json:"auth" bson:"auth"`
		P256dh string `json:"p256dh" bson:"p256dh"`
	}

	// WebPushSubscription is one device subscription (PushSubscription JSON shape).
	WebPushSubscription struct {
		Endpoint string      `json:"endpoint" bson:"endpoint"`
		Keys     WebPushKeys `json:"keys" bson:"keys"`
	}

	// Subscription represents a subscription document
	Subscription struct {
		ID                   bson.ObjectID         `bson:"_id"`
		Location             string                `bson:"location"`
		ShortName            string                `bson:"shortName"`
		Timezone             string                `bson:"timezone"`
		NtfyTopic            string                `bson:"ntfyTopic"`
		WebPushSubscriptions []WebPushSubscription `bson:"webPushSubscriptions,omitempty"`
		LatestDate           time.Time             `bson:"latestDate"`
		CreatedAt            time.Time             `bson:"createdAt"`
		LastNotifiedAt       time.Time             `bson:"lastNotifiedAt"`
		NotificationCount    int                   `bson:"notificationCount"`
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
		Action          string               `json:"action"`
		Location        string               `json:"location"`
		ShortName       string               `json:"shortName"`
		Timezone        string               `json:"timezone"`
		NtfyTopic       string               `json:"ntfyTopic"`
		WebPush         *WebPushSubscription `json:"webPush,omitempty"`
		WebPushEndpoint string               `json:"webPushEndpoint,omitempty"`
		LatestDate      string               `json:"latestDate"`
		RecaptchaToken  string               `json:"recaptchaToken"`
	}

	// LambdaHandler holds dependencies plus per-invocation request state.
	// Lambda runs one request per execution environment at a time, so the
	// per-request fields are reset at the top of HandleRequest.
	LambdaHandler struct {
		Config          Config
		URL             string
		Client          *mongo.Client
		HTTPClient      *retryablehttp.Client
		failedNtfyCount int
		requestCORS     map[string]string
	}
)

// channels reports whether the request uses ntfy (non-empty topic) and/or a Web Push body subscription.
func (r SubscriptionRequest) channels() (hasNtfy, hasWeb bool) {
	hasNtfy = strings.TrimSpace(r.NtfyTopic) != ""
	hasWeb = r.WebPush != nil && strings.TrimSpace(r.WebPush.Endpoint) != ""
	return
}

// cors returns the CORS headers for the current request, falling back to the
// primary-origin defaults if the request hasn't resolved them yet.
func (h *LambdaHandler) cors() map[string]string {
	if h.requestCORS != nil {
		return h.requestCORS
	}
	return corsHeadersForOrigin("")
}

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

// verifyRecaptchaToken verifies the reCAPTCHA token with Google's API
func (h *LambdaHandler) verifyRecaptchaToken(ctx context.Context, token string) (bool, float64, error) {
	form := url.Values{}
	form.Set("secret", h.Config.RecaptchaSecretKey)
	form.Set("response", token)

	req, err := retryablehttp.NewRequestWithContext(
		ctx,
		http.MethodPost,
		h.Config.RecaptchaURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return false, 0, fmt.Errorf("failed to create reCAPTCHA verification request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		return false, 0, fmt.Errorf("failed to verify reCAPTCHA token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("reCAPTCHA verification returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, 0, fmt.Errorf("failed to read reCAPTCHA response body: %v", err)
	}

	var result struct {
		Success    bool     `json:"success"`
		Score      float64  `json:"score"`
		Action     string   `json:"action"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, 0, fmt.Errorf("failed to unmarshal reCAPTCHA response: %v", err)
	}

	slog.Info("reCAPTCHA parsed result", "success", result.Success, "score", result.Score, "action", result.Action, "error-codes", result.ErrorCodes)

	if result.Action != "submit" {
		return false, 0, fmt.Errorf("reCAPTCHA action mismatch: expected 'submit', got '%s'", result.Action)
	}

	return result.Success, result.Score, nil
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

// sendNtfyNotification sends a notification to the specified ntfy topic.
// clickURL is optional; when set, tapping the notification opens that URL.
func (h *LambdaHandler) sendNtfyNotification(ctx context.Context, topic, title, message, clickURL string) error {
	if h.failedNtfyCount >= h.Config.MaxNtfyFailures {
		return ErrMaxNtfyFailures
	}

	payload := map[string]string{
		"topic":   topic,
		"message": message,
		"title":   title,
	}
	if strings.TrimSpace(clickURL) != "" {
		payload["click"] = clickURL
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

	slog.Info("Notification sent",
		"event", "notification",
		"topic", topic,
		"timestamp", time.Now().UTC(),
		"message", message,
	)

	return nil
}

func (h *LambdaHandler) webPushConfigured() bool {
	return h.Config.VAPIDPublicKey != "" && h.Config.VAPIDPrivateKey != ""
}

func validateWebPushSubscription(w *WebPushSubscription) error {
	if w == nil || strings.TrimSpace(w.Endpoint) == "" {
		return errors.New("web push endpoint required")
	}
	u, err := url.Parse(w.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("invalid push endpoint URL")
	}
	if strings.TrimSpace(w.Keys.P256dh) == "" || strings.TrimSpace(w.Keys.Auth) == "" {
		return errors.New("web push keys required")
	}
	return nil
}

func (h *LambdaHandler) sendOneWebPush(ctx context.Context, docID bson.ObjectID, sub WebPushSubscription, title, body, clickURL string) (int, error) {
	if clickURL == "" {
		clickURL = "/"
	}
	payload, err := json.Marshal(map[string]string{
		"title": title,
		"body":  body,
		"url":   clickURL,
	})
	if err != nil {
		return 0, err
	}
	wps := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			Auth:   sub.Keys.Auth,
			P256dh: sub.Keys.P256dh,
		},
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, wps, &webpush.Options{
		Subscriber:      h.Config.VAPIDSubscriber,
		VAPIDPublicKey:  h.Config.VAPIDPublicKey,
		VAPIDPrivateKey: h.Config.VAPIDPrivateKey,
		TTL:             86400,
		HTTPClient:      h.HTTPClient.StandardClient(),
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Warn("Web push non-success response", "subscriptionId", docID.Hex(), "status", resp.StatusCode, "reason", strings.TrimSpace(string(reason)), "endpoint", sub.Endpoint)
	}
	return resp.StatusCode, nil
}

// sendWebPushNotifications delivers to each stored browser subscription and drops dead endpoints (410/404).
// clickURL sets where tapping the notification navigates; empty means the site root.
func (h *LambdaHandler) sendWebPushNotifications(ctx context.Context, coll *mongo.Collection, docID bson.ObjectID, title, body, clickURL string, subs []WebPushSubscription) bool {
	if !h.webPushConfigured() || len(subs) == 0 {
		return false
	}
	anyOK := false
	for _, s := range subs {
		status, err := h.sendOneWebPush(ctx, docID, s, title, body, clickURL)
		if err != nil {
			slog.Warn("Web push send failed", "subscriptionId", docID.Hex(), "endpoint", s.Endpoint, "error", err)
			continue
		}
		if status >= 200 && status < 300 {
			anyOK = true
			continue
		}
		if status == http.StatusGone || status == http.StatusNotFound {
			if _, err := coll.UpdateOne(ctx, bson.M{"_id": docID}, bson.M{"$pull": bson.M{"webPushSubscriptions": bson.M{"endpoint": s.Endpoint}}}); err != nil {
				slog.Warn("Failed to remove stale web push subscription", "subscriptionId", docID.Hex(), "error", err)
			}
		}
		// Other non-2xx statuses are already logged in sendOneWebPush with response body when available.
	}
	return anyOK
}

// notifySubscription sends the same title/message on ntfy (if topic is set) and Web Push for one stored row.
// Used for lifecycle notices; not used for slot alerts, which need distinct ntfy error handling.
func (h *LambdaHandler) notifySubscription(ctx context.Context, coll *mongo.Collection, sub *Subscription, title, message, clickURL string) {
	if strings.TrimSpace(sub.NtfyTopic) != "" {
		if err := h.sendNtfyNotification(ctx, sub.NtfyTopic, title, message, clickURL); err != nil {
			slog.Error("Failed to send ntfy notification", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic, "error", err)
		}
	}
	h.sendWebPushNotifications(ctx, coll, sub.ID, title, message, clickURL, sub.WebPushSubscriptions)
}

// updateLastNotified updates the lastNotifiedAt timestamp and notificationCount for multiple subscriptions
func (h *LambdaHandler) updateLastNotified(ctx context.Context, coll *mongo.Collection, updates []mongo.WriteModel) error {
	if len(updates) == 0 {
		return nil
	}

	result, err := coll.BulkWrite(ctx, updates)
	if err != nil {
		slog.Error("Failed to update lastNotifiedAt and notificationCount", "error", err)
		return fmt.Errorf("failed to update lastNotifiedAt and notificationCount: %v", err)
	}
	slog.Debug("Updated lastNotifiedAt and notificationCount", "modifiedCount", result.ModifiedCount)
	return nil
}

// fetchAppointmentsForLocations fetches CBP appointments for each unique location in
// the subscription set concurrently with bounded parallelism. Failures are returned
// per-location; the caller decides how to react.
func (h *LambdaHandler) fetchAppointmentsForLocations(ctx context.Context, subscriptions []Subscription) (map[string][]Appointment, []error) {
	uniqueLocations := make(map[string]struct{}, len(subscriptions))
	for _, s := range subscriptions {
		uniqueLocations[s.Location] = struct{}{}
	}

	appointments := make(map[string][]Appointment, len(uniqueLocations))
	var (
		fetchErrors []error
		appsMu      sync.Mutex
		errMu       sync.Mutex
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxLocationFetchConcurrency)

	for loc := range uniqueLocations {
		loc := loc
		g.Go(func() error {
			apps, err := h.fetchAppointments(gctx, loc)
			if err != nil {
				slog.Warn("Failed to fetch appointments", "location", loc, "error", err)
				errMu.Lock()
				fetchErrors = append(fetchErrors, fmt.Errorf("location %s: %w", loc, err))
				errMu.Unlock()
				// Best-effort: don't propagate so other locations continue.
				return nil
			}
			appsMu.Lock()
			appointments[loc] = apps
			appsMu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	return appointments, fetchErrors
}

// checkAvailabilityAndNotify checks availability and notifies eligible subscribers
func (h *LambdaHandler) checkAvailabilityAndNotify(ctx context.Context, subscriptions []Subscription, globalNotifiedCount int) (int, error) {
	now := time.Now().UTC()
	var bulkUpdates []mongo.WriteModel
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")

	// Fan out CBP API calls for unique locations; this is the dominant cost per cron run.
	appointments, fetchErrors := h.fetchAppointmentsForLocations(ctx, subscriptions)

	for _, sub := range subscriptions {
		// Stop if we've reached the global notification limit
		if globalNotifiedCount >= h.Config.MaxNotifications {
			slog.Info("Reached global notification limit")
			break
		}

		// Get appointments for the subscription's location
		apps, exists := appointments[sub.Location]
		if !exists || len(apps) == 0 {
			slog.Debug("No appointments for location", "subscriptionId", sub.ID.Hex(), "location", sub.Location)
			continue
		}

		// Load timezone from subscription
		loc, err := time.LoadLocation(sub.Timezone)
		if err != nil {
			slog.Error("Failed to load timezone", "subscriptionId", sub.ID.Hex(), "timezone", sub.Timezone, "error", err)
			continue
		}

		// Process each appointment for the subscription
		var eligibleAppointment *Appointment
		for _, app := range apps {
			if !app.Active {
				continue
			}
			// Parse the timestamp in the subscription's timezone
			t, err := time.ParseInLocation("2006-01-02T15:04", app.StartTimestamp, loc)
			if err != nil {
				slog.Error("Failed to parse timestamp", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic, "error", err)
				continue
			}
			// Debug: Log subscription eligibility
			slog.Debug("Checking eligibility", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic, "appointmentTime", t, "latestDate", sub.LatestDate)

			// Check if appointment is within latestDate
			if t.Before(sub.LatestDate) || t.Equal(sub.LatestDate) {
				eligibleAppointment = &app
				break
			}
			slog.Debug("Appointment after latestDate", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic, "appointmentTime", t, "latestDate", sub.LatestDate)
		}

		if eligibleAppointment == nil {
			slog.Debug("No eligible appointments for subscription", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic)
			continue
		}

		// Format time for notification — punchy copy + deep-link to CBP booking.
		t, _ := time.ParseInLocation("2006-01-02T15:04", eligibleAppointment.StartTimestamp, loc)
		formattedTime := t.Format("Mon, Jan 2 at 3:04 PM MST")
		title := fmt.Sprintf("%s slot open", sub.ShortName)
		message := fmt.Sprintf("%s — book now", formattedTime)

		ntfyOK := false
		var ntfyErr error
		if sub.NtfyTopic != "" {
			ntfyErr = h.sendNtfyNotification(ctx, sub.NtfyTopic, title, message, bookingURL)
			if ntfyErr == nil {
				ntfyOK = true
			} else {
				slog.Error("Failed to send appointment notification", "subscriptionId", sub.ID.Hex(), "topic", sub.NtfyTopic, "error", ntfyErr)
				if errors.Is(ntfyErr, ErrMaxNtfyFailures) {
					return globalNotifiedCount, ErrMaxNtfyFailures
				}
			}
		}
		webOK := h.sendWebPushNotifications(ctx, coll, sub.ID, title, message, bookingURL, sub.WebPushSubscriptions)
		if !ntfyOK && !webOK {
			continue
		}

		// Check if subscription has exceeded max notification count after sending
		if sub.NotificationCount+1 >= h.Config.MaxNotificationCount {
			expireMessage := fmt.Sprintf("Your Global Entry appointment subscription for %s has ended as we sent %d alerts. We hope you secured an appointment! If we helped, a coffee is always appreciated: %s", sub.ShortName, sub.NotificationCount+1, supportURL)
			expireTitle := "Global Entry Subscription Ended"
			h.notifySubscription(ctx, coll, &sub, expireTitle, expireMessage, supportURL)
			_, err := coll.DeleteOne(ctx, bson.M{"_id": sub.ID})
			if err != nil {
				slog.Error("Failed to delete subscription due to max notifications", "subscriptionId", sub.ID.Hex(), "error", err)
			}
			globalNotifiedCount++
			continue
		}

		// Add update to bulkUpdates only if subscription is not deleted
		bulkUpdates = append(bulkUpdates, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": sub.ID}).
			SetUpdate(bson.M{
				"$set": bson.M{
					"lastNotifiedAt": now,
				},
				"$inc": bson.M{
					"notificationCount": 1,
				},
			}))
		globalNotifiedCount++
	}

	// Apply bulk updates only if there are valid updates
	if len(bulkUpdates) > 0 {
		if err := h.updateLastNotified(ctx, coll, bulkUpdates); err != nil {
			return globalNotifiedCount, fmt.Errorf("failed to update lastNotifiedAt: %v", err)
		}
	}

	if len(appointments) == 0 && len(fetchErrors) > 0 {
		return globalNotifiedCount, fmt.Errorf("failed to fetch appointments for all locations: %v", errors.Join(fetchErrors...))
	}

	return globalNotifiedCount, nil
}

// deleteExpiredSubscription deletes a single expired subscription
func (h *LambdaHandler) deleteExpiredSubscription(ctx context.Context, coll *mongo.Collection, sub Subscription) error {
	message := fmt.Sprintf("Your Global Entry appointment subscription for %s has expired (30 day limit) or your latest acceptable date (%s) has passed. Thanks for using Global Entry Alerts — resubscribe anytime at getglobalentryalerts.com.", sub.ShortName, sub.LatestDate.Format("2006-01-02"))
	title := "Global Entry Subscription Expired"
	h.notifySubscription(ctx, coll, &sub, title, message, "")

	_, err := coll.DeleteOne(ctx, bson.M{"_id": sub.ID})
	if err != nil {
		return fmt.Errorf("failed to delete subscription %s: %v", sub.ID, err)
	}
	slog.Info("Deleted expired subscription", "subscriptionId", sub.ID.Hex())
	return nil
}

// handleExpiringSubscriptions deletes subscriptions older than TTL or past latestDate
func (h *LambdaHandler) handleExpiringSubscriptions(ctx context.Context, coll *mongo.Collection) error {
	now := time.Now().UTC()
	ttlThreshold := now.Add(-h.Config.SubscriptionTTL)
	slog.Info("Checking for expiring subscriptions", "ttlThreshold", ttlThreshold, "currentDate", now)

	cursor, err := coll.Find(ctx, bson.M{
		"$or": []bson.M{
			{"createdAt": bson.M{"$lte": ttlThreshold}},
			{"latestDate": bson.M{"$lte": now}},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to find expired subscriptions: %v", err)
	}
	defer cursor.Close(ctx)

	var subscriptions []Subscription
	if err := cursor.All(ctx, &subscriptions); err != nil {
		return fmt.Errorf("failed to decode expired subscriptions: %v", err)
	}

	for _, sub := range subscriptions {
		if err := h.deleteExpiredSubscription(ctx, coll, sub); err != nil {
			slog.Error("Failed to process expired subscription", "subscriptionId", sub.ID.Hex(), "error", err)
		}
	}
	return nil
}

// validateSubscriptionRequest validates the subscription request
func (h *LambdaHandler) validateSubscriptionRequest(req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	hasNtfy, hasWeb := req.channels()

	if req.Location == "" {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "location is required"}`,
		}, nil
	}
	if req.Action == "subscribe" {
		if !hasNtfy && !hasWeb {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "choose ntfy topic and/or browser notifications (web push)"}`,
			}, nil
		}
		if hasWeb {
			if !h.webPushConfigured() {
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 503,
					Headers:    h.cors(),
					Body:       `{"error": "browser notifications are not configured on this server"}`,
				}, nil
			}
			if err := validateWebPushSubscription(req.WebPush); err != nil {
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    h.cors(),
					Body:       fmt.Sprintf(`{"error": %q}`, err.Error()),
				}, nil
			}
		}
	} else if req.Action == "unsubscribe" {
		if !hasNtfy && strings.TrimSpace(req.WebPushEndpoint) == "" {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "ntfyTopic or webPushEndpoint is required to unsubscribe"}`,
			}, nil
		}
		if strings.TrimSpace(req.WebPushEndpoint) != "" {
			u, err := url.Parse(req.WebPushEndpoint)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    h.cors(),
					Body:       `{"error": "invalid webPushEndpoint URL"}`,
				}, nil
			}
		}
	}
	if hasNtfy && !validNtfyPattern.MatchString(req.NtfyTopic) {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "Ntfy Topic must not contain spaces or special characters"}`,
		}, nil
	}

	if !validLocationPattern.MatchString(req.Location) {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "location must be a numeric enrollment center id"}`,
		}, nil
	}

	if req.ShortName == "" || req.Timezone == "" {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "shortName and timezone must be provided from location data"}`,
		}, nil
	}

	if req.Action == "subscribe" && req.LatestDate == "" {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "latestDate is required for subscribe"}`,
		}, nil
	}

	if req.LatestDate != "" {
		latestDate, err := time.Parse("2006-01-02", req.LatestDate)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "invalid latestDate format, use YYYY-MM-DD"}`,
			}, nil
		}
		if latestDate.Before(time.Now().UTC().Truncate(24 * time.Hour)) {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "latestDate cannot be in the past"}`,
			}, nil
		}
	}

	return events.APIGatewayV2HTTPResponse{}, nil
}

// subscribeSendConfirmations sends welcome notifications and increments notificationCount if any channel succeeds.
func (h *LambdaHandler) subscribeSendConfirmations(ctx context.Context, coll *mongo.Collection, docID bson.ObjectID, req SubscriptionRequest, latestDate time.Time, sendNtfy, sendWeb bool, webSubs []WebPushSubscription) {
	message := fmt.Sprintf("You're all set! We'll notify you when an appointment slot is available at %s before %s.", req.ShortName, latestDate.Format("2006-01-02"))
	title := "Global Entry Subscription Confirmation"
	confirmed := false
	if sendNtfy && strings.TrimSpace(req.NtfyTopic) != "" {
		if err := h.sendNtfyNotification(ctx, req.NtfyTopic, title, message, ""); err != nil {
			slog.Error("Failed to send confirmation notification", "subscriptionId", docID.Hex(), "topic", req.NtfyTopic, "error", err)
		} else {
			confirmed = true
		}
	}
	if sendWeb && len(webSubs) > 0 {
		if h.sendWebPushNotifications(ctx, coll, docID, title, message, "", webSubs) {
			confirmed = true
		}
	}
	if confirmed {
		slog.Debug("Updating notification count for subscription", "subscriptionId", docID.Hex(), "topic", req.NtfyTopic, "location", req.Location)
		if _, err := coll.UpdateOne(ctx,
			bson.M{"_id": docID},
			bson.M{"$inc": bson.M{"notificationCount": 1}}); err != nil {
			slog.Error("Failed to increment notification count", "subscriptionId", docID.Hex(), "topic", req.NtfyTopic, "error", err)
		}
	}
}

// handleSubscribe processes subscription requests
func (h *LambdaHandler) handleSubscribe(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	hasNtfy, hasWeb := req.channels()

	latestDate, err := time.Parse("2006-01-02", req.LatestDate)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "invalid latestDate format"}`,
		}, fmt.Errorf("failed to parse latestDate: %v", err)
	}

	var dupOr []bson.M
	if hasNtfy {
		dupOr = append(dupOr, bson.M{"ntfyTopic": req.NtfyTopic})
	}
	if hasWeb {
		dupOr = append(dupOr, bson.M{"webPushSubscriptions.endpoint": req.WebPush.Endpoint})
	}
	dupFilter := bson.M{"location": req.Location, "$or": dupOr}

	var existing Subscription
	err = coll.FindOne(ctx, dupFilter).Decode(&existing)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    h.cors(),
			Body:       `{"error": "failed to check existing subscription"}`,
		}, fmt.Errorf("failed to find subscription: %w", err)
	}

	now := time.Now().UTC()

	if errors.Is(err, mongo.ErrNoDocuments) {
		doc := bson.M{
			"_id":               bson.NewObjectID(),
			"location":          req.Location,
			"shortName":         req.ShortName,
			"timezone":          req.Timezone,
			"ntfyTopic":         req.NtfyTopic,
			"latestDate":        latestDate,
			"createdAt":         now,
			"lastNotifiedAt":    now.Add(-h.Config.NotificationCooldownTime),
			"notificationCount": 0,
		}
		if hasWeb {
			doc["webPushSubscriptions"] = []WebPushSubscription{*req.WebPush}
		}

		result, err := coll.InsertOne(ctx, doc)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 500,
				Headers:    h.cors(),
				Body:       `{"error": "failed to insert subscription"}`,
			}, fmt.Errorf("failed to insert subscription: %v", err)
		}
		insertedID, _ := result.InsertedID.(bson.ObjectID)
		slog.Info("Added subscription", "subscriptionId", insertedID.Hex(), "location", req.Location, "shortName", req.ShortName, "ntfyTopic", req.NtfyTopic, "latestDate", latestDate, "webPush", hasWeb)

		var webSubs []WebPushSubscription
		if hasWeb {
			webSubs = []WebPushSubscription{*req.WebPush}
		}
		h.subscribeSendConfirmations(ctx, coll, insertedID, req, latestDate, hasNtfy, hasWeb, webSubs)

		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    h.cors(),
			Body:       `{"message": "Subscribed successfully"}`,
		}, nil
	}

	// Same location + same ntfy topic and/or same push endpoint: merge instead of rejecting, so users can add
	// "This device" after an older ntfy-only subscription (previously returned "subscription already exists").
	mergedWeb := append([]WebPushSubscription(nil), existing.WebPushSubscriptions...)
	addedWeb := false
	if hasWeb {
		found := false
		for _, w := range mergedWeb {
			if w.Endpoint == req.WebPush.Endpoint {
				found = true
				break
			}
		}
		if !found {
			mergedWeb = append(mergedWeb, *req.WebPush)
			addedWeb = true
		}
	}

	newNtfy := strings.TrimSpace(existing.NtfyTopic)
	addedNtfy := false
	if hasNtfy {
		if newNtfy == "" {
			newNtfy = req.NtfyTopic
			addedNtfy = true
		} else if newNtfy != req.NtfyTopic {
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "subscription already exists for a different ntfy topic at this location"}`,
			}, nil
		}
	}

	if _, err := coll.UpdateOne(ctx, bson.M{"_id": existing.ID}, bson.M{"$set": bson.M{
		"latestDate":           latestDate,
		"shortName":            req.ShortName,
		"timezone":             req.Timezone,
		"webPushSubscriptions": mergedWeb,
		"ntfyTopic":            newNtfy,
	}}); err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    h.cors(),
			Body:       `{"error": "failed to update subscription"}`,
		}, fmt.Errorf("failed to update subscription: %w", err)
	}
	slog.Info("Updated subscription (merge)", "subscriptionId", existing.ID.Hex(), "location", req.Location, "addedWeb", addedWeb, "addedNtfy", addedNtfy)

	var newWebSubs []WebPushSubscription
	if addedWeb && hasWeb {
		newWebSubs = []WebPushSubscription{*req.WebPush}
	}
	if addedNtfy || addedWeb {
		h.subscribeSendConfirmations(ctx, coll, existing.ID, req, latestDate, addedNtfy, addedWeb, newWebSubs)
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    h.cors(),
		Body:       `{"message": "Subscribed successfully"}`,
	}, nil
}

// handleUnsubscribe processes unsubscription requests
func (h *LambdaHandler) handleUnsubscribe(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	var filter bson.M
	if strings.TrimSpace(req.NtfyTopic) != "" {
		filter = bson.M{"location": req.Location, "ntfyTopic": req.NtfyTopic}
	} else {
		filter = bson.M{"location": req.Location, "webPushSubscriptions.endpoint": req.WebPushEndpoint}
	}
	result, err := coll.DeleteOne(ctx, filter)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 500,
			Headers:    h.cors(),
			Body:       `{"error": "failed to delete subscription"}`,
		}, fmt.Errorf("failed to delete subscription: %v", err)
	}
	if result.DeletedCount == 0 {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 404,
			Headers:    h.cors(),
			Body:       `{"error": "subscription not found"}`,
		}, nil
	}
	slog.Info("Removed subscription", "location", req.Location, "ntfyTopic", req.NtfyTopic, "webPushEndpoint", req.WebPushEndpoint)
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    h.cors(),
		Body:       `{"message": "Unsubscribed successfully"}`,
	}, nil
}

// handleSubscription manages subscribe/unsubscribe requests
func (h *LambdaHandler) handleSubscription(ctx context.Context, coll *mongo.Collection, req SubscriptionRequest) (events.APIGatewayV2HTTPResponse, error) {
	if req.RecaptchaToken == "" {
		slog.Error("Missing reCAPTCHA token")
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "reCAPTCHA token is required"}`,
		}, nil
	}

	success, score, err := h.verifyRecaptchaToken(ctx, req.RecaptchaToken)
	if err != nil {
		slog.Error("Failed to verify reCAPTCHA token", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "reCAPTCHA verification failed"}`,
		}, nil
	}
	if !success || score < 0.5 {
		slog.Warn("reCAPTCHA verification failed", "success", success, "score", score)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    h.cors(),
			Body:       `{"error": "reCAPTCHA verification failed: low score or invalid token"}`,
		}, nil
	}

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
			Headers:    h.cors(),
			Body:       `{"error": "invalid action, use subscribe or unsubscribe"}`,
		}, nil
	}
}

// handleLocations proxies the CBP Global Entry enrollment centers list with an in-memory cache.
// The list changes rarely (new/removed centers); 1-hour TTL is a reasonable balance between
// freshness and avoiding hammering the upstream CBP API from browser clients.
func (h *LambdaHandler) handleLocations(ctx context.Context) (events.APIGatewayV2HTTPResponse, error) {
	locationsCacheMu.RLock()
	if len(locationsCache) > 0 && time.Since(locationsCachedAt) < locationsCacheTTL {
		cached := locationsCache
		locationsCacheMu.RUnlock()
		headers := h.cors()
		headers["Cache-Control"] = "public, max-age=3600"
		headers["Content-Type"] = "application/json"
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    headers,
			Body:       string(cached),
		}, nil
	}
	locationsCacheMu.RUnlock()

	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, cbpLocationsURL, nil)
	if err != nil {
		slog.Error("Failed to create CBP locations request", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 502,
			Headers:    h.cors(),
			Body:       `{"error": "could not build upstream request"}`,
		}, nil
	}
	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		slog.Error("Failed to fetch CBP locations", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 502,
			Headers:    h.cors(),
			Body:       `{"error": "could not reach enrollment center list"}`,
		}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("CBP locations returned non-200", "status", resp.StatusCode)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 502,
			Headers:    h.cors(),
			Body:       fmt.Sprintf(`{"error": "upstream returned %d"}`, resp.StatusCode),
		}, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 502,
			Headers:    h.cors(),
			Body:       `{"error": "could not read upstream response"}`,
		}, nil
	}

	locationsCacheMu.Lock()
	locationsCache = json.RawMessage(body)
	locationsCachedAt = time.Now()
	locationsCacheMu.Unlock()

	slog.Info("Refreshed locations cache", "bytes", len(body))
	headers := h.cors()
	headers["Cache-Control"] = "public, max-age=3600"
	headers["Content-Type"] = "application/json"
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    headers,
		Body:       string(body),
	}, nil
}

// HandleRequest handles Scheduled Events and API requests
func (h *LambdaHandler) HandleRequest(ctx context.Context, event json.RawMessage) (events.APIGatewayV2HTTPResponse, error) {
	// Lambda runs one request per execution environment at a time, so a fresh
	// reset here is sufficient to avoid leaking state across invocations.
	h.failedNtfyCount = 0
	h.requestCORS = nil
	coll := h.Client.Database("global-entry-appointment-db").Collection("subscriptions")

	var eventMap map[string]interface{}
	if err := json.Unmarshal(event, &eventMap); err != nil {
		slog.Error("Failed to parse event as JSON", "error", err)
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 400,
			Headers:    corsHeadersForOrigin(""),
			Body:       `{"error": "invalid event format"}`,
		}, nil
	}

	h.requestCORS = corsHeadersForOrigin(originFromEvent(eventMap))

	// Normalize API Gateway REST API (v1) to HTTP API (v2) shape so SAM local and both formats work
	if _, has := eventMap["rawPath"]; !has {
		if p, ok := eventMap["path"].(string); ok {
			eventMap["rawPath"] = p
		}
	}
	if reqCtx, ok := eventMap["requestContext"].(map[string]interface{}); ok {
		var httpInfo map[string]interface{}
		if h, ok := reqCtx["http"].(map[string]interface{}); ok {
			httpInfo = h
		} else {
			httpInfo = make(map[string]interface{})
			reqCtx["http"] = httpInfo
		}
		if _, has := httpInfo["method"]; !has {
			if m, ok := eventMap["httpMethod"].(string); ok {
				httpInfo["method"] = m
			}
		}
	}

	// Handle OPTIONS request
	if req, ok := eventMap["requestContext"].(map[string]interface{}); ok {
		if httpInfo, ok := req["http"].(map[string]interface{}); ok {
			if method, ok := httpInfo["method"].(string); ok && method == "OPTIONS" {
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 200,
					Headers:    h.cors(),
					Body:       "",
				}, nil
			}
		}
	}

	// Handle CloudWatch Events
	if source, ok := eventMap["source"].(string); ok {
		if source == "aws.events.availability" {
			// Minute-by-minute availability check
			cooldownThreshold := time.Now().UTC().Add(-h.Config.NotificationCooldownTime)
			slog.Debug("Cooldown threshold", "threshold", cooldownThreshold)
			pipeline := mongo.Pipeline{
				{{Key: "$match", Value: bson.M{
					"lastNotifiedAt": bson.M{"$lte": cooldownThreshold},
					"latestDate":     bson.M{"$gt": time.Now().UTC()},
				}}},
				{{Key: "$sort", Value: bson.M{
					"lastNotifiedAt": 1,
				}}},
			}
			cursor, err := coll.Aggregate(ctx, pipeline)
			if err != nil {
				slog.Error("Failed to execute aggregation", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    h.cors(),
					Body:       `{"error": "failed to execute aggregation"}`,
				}, nil
			}
			defer cursor.Close(ctx)

			var subscriptions []Subscription
			if err := cursor.All(ctx, &subscriptions); err != nil {
				slog.Error("Failed to decode aggregation results", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    h.cors(),
					Body:       `{"error": "failed to decode aggregation results"}`,
				}, nil
			}

			globalNotifiedCount := 0
			if len(subscriptions) > 0 {
				var err error
				globalNotifiedCount, err = h.checkAvailabilityAndNotify(ctx, subscriptions, globalNotifiedCount)
				if err != nil {
					if errors.Is(err, ErrMaxNtfyFailures) {
						slog.Error("Terminating due to maximum ntfy failures")
						return events.APIGatewayV2HTTPResponse{
							StatusCode: 500,
							Headers:    h.cors(),
							Body:       `{"error": "maximum notification failures reached"}`,
						}, ErrMaxNtfyFailures
					}
					slog.Warn("Failed to check availability", "error", err)
				}
			}

			return events.APIGatewayV2HTTPResponse{
				StatusCode: 200,
				Headers:    h.cors(),
				Body:       `{"message": "availability check processed"}`,
			}, nil
		} else if source == "aws.events.expiration" {
			// Daily expiration check
			if err := h.handleExpiringSubscriptions(ctx, coll); err != nil {
				slog.Error("Failed to handle expiring subscriptions", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    h.cors(),
					Body:       `{"error": "failed to handle expiring subscriptions"}`,
				}, nil
			}
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 200,
				Headers:    h.cors(),
				Body:       `{"message": "expiration check processed"}`,
			}, nil
		}
	}

	// Handle API Gateway V2 HTTP event
	if rawPath, ok := eventMap["rawPath"].(string); ok {
		requestContext, ok := eventMap["requestContext"].(map[string]interface{})
		if !ok {
			slog.Error("Missing requestContext in event")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		httpInfo, ok := requestContext["http"].(map[string]interface{})
		if !ok {
			slog.Error("Missing http info in requestContext")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		method, ok := httpInfo["method"].(string)
		if !ok {
			slog.Error("Missing method in http info")
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 400,
				Headers:    h.cors(),
				Body:       `{"error": "invalid event format"}`,
			}, nil
		}
		body, _ := eventMap["body"].(string)

		// GET /locations: cached CBP enrollment center list
		if method == "GET" && rawPath == "/locations" {
			return h.handleLocations(ctx)
		}

		// GET / or GET /subscriptions: API info, Web Push config, and subscriber count
		if method == "GET" && (rawPath == "/" || rawPath == "/subscriptions") {
			info := map[string]interface{}{
				"message":        "Global Entry appointment notification API",
				"subscriptions":  "POST /subscriptions",
				"webPushEnabled": h.webPushConfigured(),
			}
			if rawPath == "/subscriptions" {
				info["message"] = "Global Entry appointment notifications. Use POST /subscriptions with action, location, ntfyTopic and/or webPush, etc."
				// Include active subscriber count for social proof on the frontend
				if count, err := coll.CountDocuments(ctx, bson.M{}); err == nil {
					info["subscriberCount"] = count
				}
			}
			if h.webPushConfigured() {
				info["vapidPublicKey"] = h.Config.VAPIDPublicKey
			}
			msg, _ := json.Marshal(info)
			return events.APIGatewayV2HTTPResponse{
				StatusCode: 200,
				Headers:    h.cors(),
				Body:       string(msg),
			}, nil
		}

		if method == "POST" && strings.HasSuffix(rawPath, "/subscriptions") {
			if body == "" {
				slog.Error("Invalid request: missing body")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    h.cors(),
					Body:       `{"error": "missing request body"}`,
				}, nil
			}
			var subReq SubscriptionRequest
			if err := json.Unmarshal([]byte(body), &subReq); err != nil {
				slog.Error("Failed to parse request body", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    h.cors(),
					Body:       `{"error": "invalid request body"}`,
				}, nil
			}
			if subReq.Action == "" || subReq.Location == "" {
				slog.Error("Invalid subscription request: missing required fields")
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 400,
					Headers:    h.cors(),
					Body:       `{"error": "missing required fields"}`,
				}, nil
			}
			resp, err := h.handleSubscription(ctx, coll, subReq)
			if err != nil {
				slog.Error("Failed to handle subscription", "error", err)
				return events.APIGatewayV2HTTPResponse{
					StatusCode: 500,
					Headers:    h.cors(),
					Body:       `{"error": "internal server error"}`,
				}, fmt.Errorf("failed to handle subscription: %v", err)
			}
			slog.Debug("Returning response", "statusCode", resp.StatusCode, "body", resp.Body)
			return resp, nil
		}
	}

	slog.Error("Invalid event", "event", string(event))
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 400,
		Headers:    h.cors(),
		Body:       `{"error": "invalid event format"}`,
	}, nil
}

func main() {
	var config Config
	if err := envconfig.Process("", &config); err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	cbpURL := "https://ttp.cbp.dhs.gov/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1"
	dbURL := "mongodb+srv://arun0009:%s@global-entry-appointmen.fcwlj2v.mongodb.net/?retryWrites=true&w=majority&appName=global-entry-appointment-cluster"
	serverAPI := options.ServerAPI(options.ServerAPIVersion1)
	opts := options.Client().
		ApplyURI(fmt.Sprintf(dbURL, config.MongoDBPassword)).
		SetServerAPIOptions(serverAPI).
		SetConnectTimeout(config.MongoConnectTimeout).
		SetMinPoolSize(1). // keep one warm connection between invocations
		SetMaxPoolSize(5)  // Lambda is single-concurrent per instance; low pool keeps cost down
	client, err := mongo.Connect(opts)
	if err != nil {
		slog.Error("Failed to connect to MongoDB", "error", err)
		os.Exit(1)
	}

	// Force the lazy connection during init so the first request doesn't pay the handshake cost.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), config.MongoConnectTimeout)
	if err := client.Ping(pingCtx, nil); err != nil {
		slog.Warn("MongoDB ping failed during init; first request will retry", "error", err)
	}
	pingCancel()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	handler := NewLambdaHandler(config, cbpURL, client)
	lambda.Start(handler.HandleRequest)
}

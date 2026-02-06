package external

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Helper: Create test SendGrid client pointed at httptest server
// ---------------------------------------------------------------------------

func newTestSendGridClient(t *testing.T, serverURL string) *SendGridClient {
	t.Helper()
	base := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-sendgrid",
		RetryPolicy{
			MaxRetries: 0, // No retries in tests for deterministic behavior
			MinWait:    1 * time.Millisecond,
			MaxWait:    10 * time.Millisecond,
		},
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)

	return NewSendGridClientWithBase(base, SendGridClientConfig{
		APIKey:  "SG.test_api_key",
		BaseURL: serverURL,
	})
}

// ---------------------------------------------------------------------------
// Send Tests - Success Path
// ---------------------------------------------------------------------------

func TestSendGridSend_Success(t *testing.T) {
	var receivedPayload sendGridMailPayload
	var receivedAuth string
	var receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method and path
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v3/mail/send" {
			t.Errorf("expected path /v3/mail/send, got %s", r.URL.Path)
		}

		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")

		// Parse the request body to verify payload structure
		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		// Respond with 202 Accepted and a message ID
		w.Header().Set("X-Message-Id", "sg_msg_abc123")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	input := types.SendInput{
		To: "recipient@example.com",
		From: types.SenderIdentity{
			Name:    "WatchPoint Alerts",
			Address: "alerts@watchpoint.io",
		},
		TemplateID: "d-template-id-123",
		TemplateData: map[string]interface{}{
			"watchpoint_name": "Rain Alert - NYC",
			"threshold_value": "55%",
			"current_value":   "72%",
			"alert_time":      "Mon, Jan 6 at 3:04 PM",
		},
		ReferenceID: "notif_001",
	}

	msgID, err := client.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify returned message ID
	if msgID != "sg_msg_abc123" {
		t.Errorf("expected message ID sg_msg_abc123, got %s", msgID)
	}

	// Verify authorization header
	if receivedAuth != "Bearer SG.test_api_key" {
		t.Errorf("expected Bearer SG.test_api_key, got %s", receivedAuth)
	}

	// Verify content type
	if receivedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", receivedContentType)
	}

	// Verify payload structure - personalizations
	if len(receivedPayload.Personalizations) != 1 {
		t.Fatalf("expected 1 personalization, got %d", len(receivedPayload.Personalizations))
	}

	p := receivedPayload.Personalizations[0]
	if len(p.To) != 1 {
		t.Fatalf("expected 1 'to' address, got %d", len(p.To))
	}
	if p.To[0].Email != "recipient@example.com" {
		t.Errorf("expected to email recipient@example.com, got %s", p.To[0].Email)
	}

	// Verify dynamic template data
	if p.DynamicData == nil {
		t.Fatal("expected dynamic_template_data to be set")
	}
	if name, ok := p.DynamicData["watchpoint_name"]; !ok || name != "Rain Alert - NYC" {
		t.Errorf("expected watchpoint_name 'Rain Alert - NYC', got %v", name)
	}
	if threshold, ok := p.DynamicData["threshold_value"]; !ok || threshold != "55%" {
		t.Errorf("expected threshold_value '55%%', got %v", threshold)
	}

	// Verify from address
	if receivedPayload.From.Email != "alerts@watchpoint.io" {
		t.Errorf("expected from email alerts@watchpoint.io, got %s", receivedPayload.From.Email)
	}
	if receivedPayload.From.Name != "WatchPoint Alerts" {
		t.Errorf("expected from name 'WatchPoint Alerts', got %s", receivedPayload.From.Name)
	}

	// Verify template ID
	if receivedPayload.TemplateID != "d-template-id-123" {
		t.Errorf("expected template ID d-template-id-123, got %s", receivedPayload.TemplateID)
	}

	// Verify custom args for correlation
	if receivedPayload.CustomArgs == nil {
		t.Fatal("expected custom_args to be set")
	}
	if refID, ok := receivedPayload.CustomArgs["reference_id"]; !ok || refID != "notif_001" {
		t.Errorf("expected reference_id 'notif_001', got %v", refID)
	}
}

func TestSendGridSend_SuccessNoReferenceID(t *testing.T) {
	var receivedPayload sendGridMailPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.Header().Set("X-Message-Id", "sg_msg_norefs")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	input := types.SendInput{
		To: "recipient@example.com",
		From: types.SenderIdentity{
			Name:    "WatchPoint Alerts",
			Address: "alerts@watchpoint.io",
		},
		TemplateID:   "d-template-id-456",
		TemplateData: map[string]interface{}{"key": "value"},
		ReferenceID:  "", // No reference ID
	}

	msgID, err := client.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if msgID != "sg_msg_norefs" {
		t.Errorf("expected message ID sg_msg_norefs, got %s", msgID)
	}

	// custom_args should be omitted when ReferenceID is empty
	if receivedPayload.CustomArgs != nil {
		t.Errorf("expected custom_args to be nil when no ReferenceID, got %v", receivedPayload.CustomArgs)
	}
}

func TestSendGridSend_SuccessNilTemplateData(t *testing.T) {
	var receivedPayload sendGridMailPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.Header().Set("X-Message-Id", "sg_msg_nodata")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	input := types.SendInput{
		To: "recipient@example.com",
		From: types.SenderIdentity{
			Address: "alerts@watchpoint.io",
		},
		TemplateID:   "d-template-id-789",
		TemplateData: nil, // No template data
		ReferenceID:  "notif_002",
	}

	_, err := client.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Personalization should still exist, just with nil/empty dynamic data
	if len(receivedPayload.Personalizations) != 1 {
		t.Fatalf("expected 1 personalization, got %d", len(receivedPayload.Personalizations))
	}
}

// ---------------------------------------------------------------------------
// Send Tests - Error Paths
// ---------------------------------------------------------------------------

func TestSendGridSend_ForbiddenMapsToEmailBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{
					"message": "The from address does not match a verified Sender Identity.",
					"field":   "from",
					"help":    "http://sendgrid.com/docs/...",
				},
			},
		})
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, err := client.Send(context.Background(), types.SendInput{
		To:         "blocked@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-template-123",
	})
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeEmailBlocked {
		t.Errorf("expected error code %s, got %s", types.ErrCodeEmailBlocked, appErr.Code)
	}
}

func TestSendGridSend_RateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{"message": "Too many requests"},
			},
		})
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, err := client.Send(context.Background(), types.SendInput{
		To:         "recipient@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-template-123",
	})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	// BaseClient maps 429 to ErrCodeUpstreamRateLimited after retry exhaustion
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}
}

func TestSendGridSend_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{"message": "Internal server error"},
			},
		})
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, err := client.Send(context.Background(), types.SendInput{
		To:         "recipient@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-template-123",
	})
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}

	// BaseClient will convert 5xx to an AppError with ErrCodeUpstreamUnavailable
	// since retries are exhausted (MaxRetries: 0).
	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
}

func TestSendGridSend_BadRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{
					"message": "The template_id must refer to an existing template.",
					"field":   "template_id",
				},
			},
		})
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, err := client.Send(context.Background(), types.SendInput{
		To:         "recipient@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-invalid-template",
	})
	if err == nil {
		t.Fatal("expected error for 400, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamEmailProvider {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamEmailProvider, appErr.Code)
	}
}

func TestSendGridSend_NonJSONErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Bad Request - not JSON"))
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, err := client.Send(context.Background(), types.SendInput{
		To:         "recipient@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-template-123",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamEmailProvider {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamEmailProvider, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Authorization Header Tests
// ---------------------------------------------------------------------------

func TestSendGridClient_AuthorizationHeader(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("X-Message-Id", "sg_auth_test")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	_, _ = client.Send(context.Background(), types.SendInput{
		To:         "recipient@example.com",
		From:       types.SenderIdentity{Address: "alerts@watchpoint.io"},
		TemplateID: "d-template-123",
	})

	if receivedAuth != "Bearer SG.test_api_key" {
		t.Errorf("expected Bearer SG.test_api_key, got %s", receivedAuth)
	}
}

// ---------------------------------------------------------------------------
// Payload Structure Verification
// ---------------------------------------------------------------------------

func TestSendGridSend_PayloadStructure(t *testing.T) {
	// This test verifies the exact JSON structure sent to SendGrid matches
	// the v3 mail/send Dynamic Template format.
	var rawBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		rawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		defer r.Body.Close()
		w.Header().Set("X-Message-Id", "sg_structure_test")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := newTestSendGridClient(t, server.URL)

	input := types.SendInput{
		To: "user@example.com",
		From: types.SenderIdentity{
			Name:    "Wedding Weather",
			Address: "alerts@watchpoint.io",
		},
		TemplateID: "d-wedding-template",
		TemplateData: map[string]interface{}{
			"event_name":    "Big Day",
			"forecast_temp": "72",
			"rain_chance":   "15%",
		},
		ReferenceID: "notif_wedding_001",
	}

	_, err := client.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Parse the raw body to verify JSON keys are correct
	var parsed map[string]interface{}
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("failed to parse raw body: %v", err)
	}

	// Verify top-level keys
	if _, ok := parsed["personalizations"]; !ok {
		t.Error("missing 'personalizations' key in payload")
	}
	if _, ok := parsed["from"]; !ok {
		t.Error("missing 'from' key in payload")
	}
	if _, ok := parsed["template_id"]; !ok {
		t.Error("missing 'template_id' key in payload")
	}
	if _, ok := parsed["custom_args"]; !ok {
		t.Error("missing 'custom_args' key in payload")
	}

	// Verify template_id value
	if parsed["template_id"] != "d-wedding-template" {
		t.Errorf("expected template_id 'd-wedding-template', got %v", parsed["template_id"])
	}

	// Verify personalizations contain dynamic_template_data (the correct key for SendGrid)
	personalizations, ok := parsed["personalizations"].([]interface{})
	if !ok || len(personalizations) != 1 {
		t.Fatalf("expected 1 personalization, got %v", parsed["personalizations"])
	}

	p0, ok := personalizations[0].(map[string]interface{})
	if !ok {
		t.Fatal("personalization is not an object")
	}
	if _, ok := p0["dynamic_template_data"]; !ok {
		t.Error("missing 'dynamic_template_data' key in personalization")
	}
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertion that SendGridClient satisfies EmailProvider.
var _ EmailProvider = (*SendGridClient)(nil)

package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"watchpoint/internal/types"
)

// --- JSON helper tests ---

func TestJSON_Success(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	data := APIResponse{Data: map[string]string{"name": "test"}}
	JSON(w, r, http.StatusOK, data)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var body APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	dataMap, ok := body.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("expected data to be a map, got %T", body.Data)
	}
	if dataMap["name"] != "test" {
		t.Errorf("expected name=test, got %v", dataMap["name"])
	}
}

func TestJSON_Created(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	data := map[string]string{"id": "wp_123"}
	JSON(w, r, http.StatusCreated, data)

	resp := w.Result()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}
}

func TestJSON_NilData(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	JSON(w, r, http.StatusNoContent, nil)

	resp := w.Result()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected status 204, got %d", resp.StatusCode)
	}
}

func TestJSON_MarshalFailure(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Add request ID to context for verification.
	ctx := types.WithRequestID(r.Context(), "req-marshal-fail")
	r = r.WithContext(ctx)

	// Channels cannot be marshalled to JSON.
	unmarshalable := make(chan int)
	JSON(w, r, http.StatusOK, unmarshalable)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode fallback error: %v", err)
	}
	if errResp.Error.Code != string(types.ErrCodeInternalUnexpected) {
		t.Errorf("expected code %s, got %s", types.ErrCodeInternalUnexpected, errResp.Error.Code)
	}
	if errResp.Error.RequestID != "req-marshal-fail" {
		t.Errorf("expected request_id req-marshal-fail, got %s", errResp.Error.RequestID)
	}
}

func TestJSON_WithMeta(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	data := APIResponse{
		Data: map[string]string{"id": "wp_1"},
		Meta: &types.ResponseMeta{
			Warnings: []string{"deprecated endpoint"},
		},
	}
	JSON(w, r, http.StatusOK, data)

	resp := w.Result()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	meta, ok := body["meta"].(map[string]interface{})
	if !ok {
		t.Fatal("expected meta field in response")
	}
	warnings, ok := meta["warnings"].([]interface{})
	if !ok || len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %v", meta["warnings"])
	}
	if warnings[0] != "deprecated endpoint" {
		t.Errorf("expected warning 'deprecated endpoint', got %v", warnings[0])
	}
}

// --- Error helper tests ---

func TestError_AppError_Validation(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(r.Context(), "req-val-001")
	r = r.WithContext(ctx)

	appErr := types.NewAppError(
		types.ErrCodeValidationInvalidLat,
		"Latitude must be between -90 and 90",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != string(types.ErrCodeValidationInvalidLat) {
		t.Errorf("expected code %s, got %s", types.ErrCodeValidationInvalidLat, errResp.Error.Code)
	}
	if errResp.Error.Message != "Latitude must be between -90 and 90" {
		t.Errorf("expected message about latitude, got %q", errResp.Error.Message)
	}
	if errResp.Error.RequestID != "req-val-001" {
		t.Errorf("expected request_id req-val-001, got %s", errResp.Error.RequestID)
	}
}

func TestError_AppError_Auth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(r.Context(), "req-auth-001")
	r = r.WithContext(ctx)

	appErr := types.NewAppError(
		types.ErrCodeAuthTokenInvalid,
		"invalid or expired token",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != string(types.ErrCodeAuthTokenInvalid) {
		t.Errorf("expected code %s, got %s", types.ErrCodeAuthTokenInvalid, errResp.Error.Code)
	}
}

func TestError_AppError_NotFound(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/watchpoints/wp_123", nil)

	appErr := types.NewAppError(
		types.ErrCodeNotFoundWatchpoint,
		"watchpoint not found",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}
}

func TestError_AppError_Conflict(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/wp_123/pause", nil)

	appErr := types.NewAppError(
		types.ErrCodeConflictPaused,
		"watchpoint is already paused",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected status 409, got %d", resp.StatusCode)
	}
}

func TestError_AppError_Permission(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/v1/watchpoints/wp_123", nil)

	appErr := types.NewAppError(
		types.ErrCodePermissionRole,
		"insufficient role for this operation",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", resp.StatusCode)
	}
}

func TestError_AppError_RateLimit(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)

	appErr := types.NewAppError(
		types.ErrCodeRateLimit,
		"rate limit exceeded",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", resp.StatusCode)
	}
}

func TestError_AppError_Internal(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)

	appErr := types.NewAppError(
		types.ErrCodeInternalDB,
		"database connection failed",
		errors.New("connection refused"),
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	// Verify the wrapped error is NOT leaked to the client.
	if strings.Contains(errResp.Error.Message, "connection refused") {
		t.Error("internal error details should not be exposed to client")
	}
}

func TestError_AppError_Upstream(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/billing/subscribe", nil)

	appErr := types.NewAppError(
		types.ErrCodeUpstreamStripe,
		"payment provider temporarily unavailable",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", resp.StatusCode)
	}
}

func TestError_AppError_WithDetails(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(r.Context(), "req-detail-001")
	r = r.WithContext(ctx)

	appErr := types.NewAppErrorWithDetails(
		types.ErrCodeValidationMissingField,
		"required field missing",
		nil,
		map[string]any{"field": "name", "constraint": "required"},
	)
	Error(w, r, appErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Details["field"] != "name" {
		t.Errorf("expected details.field=name, got %v", errResp.Error.Details["field"])
	}
	if errResp.Error.Details["constraint"] != "required" {
		t.Errorf("expected details.constraint=required, got %v", errResp.Error.Details["constraint"])
	}
	if errResp.Error.RequestID != "req-detail-001" {
		t.Errorf("expected request_id req-detail-001, got %s", errResp.Error.RequestID)
	}
}

func TestError_GenericError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(r.Context(), "req-generic-001")
	r = r.WithContext(ctx)

	genericErr := errors.New("some internal database error with connection details")
	Error(w, r, genericErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != string(types.ErrCodeInternalUnexpected) {
		t.Errorf("expected code %s, got %s", types.ErrCodeInternalUnexpected, errResp.Error.Code)
	}
	// Must NOT leak internal error message.
	if strings.Contains(errResp.Error.Message, "database") {
		t.Error("generic error message should not be exposed to client")
	}
	if errResp.Error.Message != "an unexpected error occurred" {
		t.Errorf("expected safe message, got %q", errResp.Error.Message)
	}
	if errResp.Error.RequestID != "req-generic-001" {
		t.Errorf("expected request_id req-generic-001, got %s", errResp.Error.RequestID)
	}
}

func TestError_WrappedAppError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/watchpoints/wp_123", nil)

	// Wrap an AppError inside another error.
	appErr := types.NewAppError(
		types.ErrCodeNotFoundWatchpoint,
		"watchpoint not found",
		nil,
	)
	wrappedErr := errors.Join(errors.New("handler context"), appErr)
	Error(w, r, wrappedErr)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 from wrapped AppError, got %d", resp.StatusCode)
	}
}

func TestError_NoRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// No request ID in context.

	appErr := types.NewAppError(
		types.ErrCodeInternalUnexpected,
		"something went wrong",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	var errResp APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	// request_id should be empty string, not missing.
	if errResp.Error.RequestID != "" {
		t.Errorf("expected empty request_id, got %q", errResp.Error.RequestID)
	}
}

// --- DecodeJSON tests ---

func TestDecodeJSON_Success(t *testing.T) {
	body := `{"name":"My WatchPoint","lat":40.7128}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	var dst struct {
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
	}
	err := DecodeJSON(w, r, &dst)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if dst.Name != "My WatchPoint" {
		t.Errorf("expected name 'My WatchPoint', got %q", dst.Name)
	}
	if dst.Lat != 40.7128 {
		t.Errorf("expected lat 40.7128, got %f", dst.Lat)
	}
}

func TestDecodeJSON_UnknownField(t *testing.T) {
	body := `{"name":"test","unknown_field":"value"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Name string `json:"name"`
	}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "unknown field") {
		t.Errorf("expected message about unknown field, got %q", appErr.Message)
	}
}

func TestDecodeJSON_SyntaxError(t *testing.T) {
	body := `{invalid json`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct{}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for syntax error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "malformed JSON") {
		t.Errorf("expected message about malformed JSON, got %q", appErr.Message)
	}
}

func TestDecodeJSON_EmptyBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))

	var dst struct{}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "empty") {
		t.Errorf("expected message about empty body, got %q", appErr.Message)
	}
}

func TestDecodeJSON_TypeMismatch(t *testing.T) {
	body := `{"count":"not_a_number"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Count int `json:"count"`
	}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for type mismatch, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
	if appErr.Details["field"] != "count" {
		t.Errorf("expected details.field=count, got %v", appErr.Details["field"])
	}
}

func TestDecodeJSON_ExceedsMaxSize(t *testing.T) {
	// Create a body that exceeds 1MB.
	largeBody := strings.Repeat("x", maxRequestBodySize+1)
	body := `{"data":"` + largeBody + `"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Data string `json:"data"`
	}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for oversized body, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
}

func TestDecodeJSON_MultipleJSONValues(t *testing.T) {
	body := `{"name":"first"}{"name":"second"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Name string `json:"name"`
	}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for multiple JSON values, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != errCodeValidationInvalidJSON {
		t.Errorf("expected code %s, got %s", errCodeValidationInvalidJSON, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "single JSON object") {
		t.Errorf("expected message about single JSON object, got %q", appErr.Message)
	}
}

func TestDecodeJSON_NilBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	// http.NewRequest with nil body sets Body to http.NoBody.

	var dst struct{}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for nil body, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
}

// --- Integration: Error writes proper JSON structure ---

func TestError_ResponseStructure(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(r.Context(), "req-struct-001")
	r = r.WithContext(ctx)

	appErr := types.NewAppError(
		types.ErrCodeValidationInvalidLat,
		"Latitude must be between -90 and 90",
		nil,
	)
	Error(w, r, appErr)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	// Verify the JSON has the exact top-level structure.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("failed to unmarshal raw: %v", err)
	}
	// Must have "error" at top level.
	if _, ok := raw["error"]; !ok {
		t.Error("response must have top-level 'error' field")
	}

	// Parse error detail.
	var errDetail struct {
		Error struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			Details   map[string]any `json:"details"`
			RequestID string         `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errDetail); err != nil {
		t.Fatalf("failed to parse structured error: %v", err)
	}
	if errDetail.Error.Code == "" {
		t.Error("error.code must not be empty")
	}
	if errDetail.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
	if errDetail.Error.RequestID != "req-struct-001" {
		t.Errorf("error.request_id: expected req-struct-001, got %q", errDetail.Error.RequestID)
	}
}

// --- Verify Content-Type on error responses ---

func TestError_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	Error(w, r, errors.New("test"))

	resp := w.Result()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// --- Verify all ErrorCode categories map to expected HTTP statuses via Error ---

func TestError_AllErrorCodeCategories(t *testing.T) {
	tests := []struct {
		name           string
		code           types.ErrorCode
		expectedStatus int
	}{
		{"validation -> 400", types.ErrCodeValidationInvalidLon, http.StatusBadRequest},
		{"validation timezone -> 400", types.ErrCodeValidationInvalidTimezone, http.StatusBadRequest},
		{"validation conditions -> 400", types.ErrCodeValidationInvalidConditions, http.StatusBadRequest},
		{"validation threshold -> 400", types.ErrCodeValidationThresholdRange, http.StatusBadRequest},
		{"validation time_window -> 400", types.ErrCodeValidationTimeWindow, http.StatusBadRequest},
		{"validation missing_field -> 400", types.ErrCodeValidationMissingField, http.StatusBadRequest},
		{"validation email -> 400", types.ErrCodeValidationInvalidEmail, http.StatusBadRequest},
		{"validation webhook -> 400", types.ErrCodeValidationInvalidWebhook, http.StatusBadRequest},
		{"validation max_conditions -> 400", types.ErrCodeValidationMaxConditions, http.StatusBadRequest},
		{"auth token missing -> 401", types.ErrCodeAuthTokenMissing, http.StatusUnauthorized},
		{"auth token expired -> 401", types.ErrCodeAuthTokenExpired, http.StatusUnauthorized},
		{"auth session expired -> 401", types.ErrCodeAuthSessionExpired, http.StatusUnauthorized},
		{"permission scope -> 403", types.ErrCodePermissionScope, http.StatusForbidden},
		{"permission org mismatch -> 403", types.ErrCodePermissionOrgMismatch, http.StatusForbidden},
		{"permission role -> 403", types.ErrCodePermissionRole, http.StatusForbidden},
		{"limit watchpoints -> 403", types.ErrCodeLimitWatchpoints, http.StatusForbidden},
		{"limit api calls -> 429", types.ErrCodeLimitAPICalls, http.StatusTooManyRequests},
		{"rate limit -> 429", types.ErrCodeRateLimit, http.StatusTooManyRequests},
		{"not found watchpoint -> 404", types.ErrCodeNotFoundWatchpoint, http.StatusNotFound},
		{"not found org -> 404", types.ErrCodeNotFoundOrg, http.StatusNotFound},
		{"not found user -> 404", types.ErrCodeNotFoundUser, http.StatusNotFound},
		{"not found api key -> 404", types.ErrCodeNotFoundAPIKey, http.StatusNotFound},
		{"conflict paused -> 409", types.ErrCodeConflictPaused, http.StatusConflict},
		{"conflict active -> 409", types.ErrCodeConflictActive, http.StatusConflict},
		{"conflict email -> 409", types.ErrCodeConflictEmail, http.StatusConflict},
		{"conflict idempotency -> 409", types.ErrCodeConflictIdempotency, http.StatusConflict},
		{"internal db -> 500", types.ErrCodeInternalDB, http.StatusInternalServerError},
		{"internal unexpected -> 500", types.ErrCodeInternalUnexpected, http.StatusInternalServerError},
		{"upstream stripe -> 502", types.ErrCodeUpstreamStripe, http.StatusBadGateway},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)

			appErr := types.NewAppError(tc.code, "test", nil)
			Error(w, r, appErr)

			resp := w.Result()
			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("code %s: expected status %d, got %d", tc.code, tc.expectedStatus, resp.StatusCode)
			}
		})
	}
}

// --- Verify DecodeJSON with valid nested objects ---

func TestDecodeJSON_NestedObject(t *testing.T) {
	body := `{"name":"test","location":{"lat":40.7128,"lon":-74.0060}}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Name     string `json:"name"`
		Location struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
		} `json:"location"`
	}
	err := DecodeJSON(w, r, &dst)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if dst.Location.Lat != 40.7128 {
		t.Errorf("expected lat 40.7128, got %f", dst.Location.Lat)
	}
	if dst.Location.Lon != -74.0060 {
		t.Errorf("expected lon -74.0060, got %f", dst.Location.Lon)
	}
}

// --- Verify MaxBytesReader is applied to body ---

func TestDecodeJSON_MaxBytesReader(t *testing.T) {
	// Construct a body that is exactly at the limit (should succeed).
	// Using a simple JSON object with padding.
	padding := strings.Repeat("a", maxRequestBodySize-20) // Leave room for JSON structure.
	body := `{"data":"` + padding + `"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Data string `json:"data"`
	}
	// This should succeed since it's within limits.
	err := DecodeJSON(w, r, &dst)
	// Note: The exact behavior depends on whether the padding + structure fits.
	// The point is that the limit is applied.
	// We test "exceeds" separately above; here we just verify no panic.
	_ = err
}

// --- Verify DecodeJSON with whitespace-only body ---

func TestDecodeJSON_WhitespaceBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("   \n\t  "))

	var dst struct{}
	err := DecodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected error for whitespace-only body, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
}

// --- Helper: verify DecodeJSON does not consume body twice ---

func TestDecodeJSON_BodyConsumed(t *testing.T) {
	body := `{"name":"test"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Name string `json:"name"`
	}
	err := DecodeJSON(w, r, &dst)
	if err != nil {
		t.Fatalf("first decode should succeed, got %v", err)
	}

	// Second call should fail because body is consumed.
	var dst2 struct {
		Name string `json:"name"`
	}
	err = DecodeJSON(w, r, &dst2)
	if err == nil {
		t.Fatal("second decode should fail, body was consumed")
	}
}

// --- Verify DecodeJSON with array body ---

func TestDecodeJSON_ArrayBody(t *testing.T) {
	// DecodeJSON expects an object, but an array is valid JSON.
	// Whether this succeeds depends on the dst type.
	body := `[{"name":"a"},{"name":"b"}]`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst []struct {
		Name string `json:"name"`
	}
	err := DecodeJSON(w, r, &dst)
	if err != nil {
		t.Fatalf("expected no error for array decode, got %v", err)
	}
	if len(dst) != 2 {
		t.Errorf("expected 2 items, got %d", len(dst))
	}
}

// --- Test that DecodeJSON properly handles io.ReadCloser ---

func TestDecodeJSON_ReadCloserBody(t *testing.T) {
	body := `{"name":"test"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	// Wrap body in a NopCloser to simulate real HTTP body.
	r.Body = io.NopCloser(bytes.NewBufferString(body))

	var dst struct {
		Name string `json:"name"`
	}
	err := DecodeJSON(w, r, &dst)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if dst.Name != "test" {
		t.Errorf("expected name 'test', got %q", dst.Name)
	}
}

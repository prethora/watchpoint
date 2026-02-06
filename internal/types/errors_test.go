package types

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestAppErrorImplementsError verifies that *AppError satisfies the error interface.
func TestAppErrorImplementsError(t *testing.T) {
	var _ error = (*AppError)(nil)
}

// TestAppErrorErrorFormat verifies the Error() method produces the spec format: "code: message".
func TestAppErrorErrorFormat(t *testing.T) {
	appErr := &AppError{
		Code:    ErrCodeValidationInvalidLat,
		Message: "Latitude must be between -90 and 90",
	}

	expected := "validation_invalid_latitude: Latitude must be between -90 and 90"
	if appErr.Error() != expected {
		t.Errorf("Error() = %q, want %q", appErr.Error(), expected)
	}
}

// TestAppErrorUnwrap verifies the error chain support via Unwrap.
func TestAppErrorUnwrap(t *testing.T) {
	underlying := errors.New("database connection failed")
	appErr := &AppError{
		Code:    ErrCodeInternalDB,
		Message: "failed to query watchpoints",
		Err:     underlying,
	}

	if appErr.Unwrap() != underlying {
		t.Errorf("Unwrap() returned unexpected error: got %v, want %v", appErr.Unwrap(), underlying)
	}
}

// TestAppErrorUnwrapNil verifies Unwrap returns nil when no underlying error exists.
func TestAppErrorUnwrapNil(t *testing.T) {
	appErr := &AppError{
		Code:    ErrCodeNotFoundWatchpoint,
		Message: "watchpoint not found",
	}

	if appErr.Unwrap() != nil {
		t.Errorf("Unwrap() should return nil when Err is nil, got %v", appErr.Unwrap())
	}
}

// TestAppErrorErrorsAs verifies that errors.As can extract AppError from an error chain.
func TestAppErrorErrorsAs(t *testing.T) {
	appErr := &AppError{
		Code:    ErrCodeAuthTokenExpired,
		Message: "token has expired",
	}
	wrappedErr := fmt.Errorf("handler failed: %w", appErr)

	var target *AppError
	if !errors.As(wrappedErr, &target) {
		t.Fatal("errors.As should find AppError in the chain")
	}
	if target.Code != ErrCodeAuthTokenExpired {
		t.Errorf("extracted Code = %q, want %q", target.Code, ErrCodeAuthTokenExpired)
	}
}

// TestAppErrorErrorsIs verifies that errors.Is works through the AppError chain.
func TestAppErrorErrorsIs(t *testing.T) {
	sentinel := errors.New("sentinel")
	appErr := &AppError{
		Code:    ErrCodeInternalUnexpected,
		Message: "unexpected failure",
		Err:     sentinel,
	}

	if !errors.Is(appErr, sentinel) {
		t.Error("errors.Is should find the sentinel error through Unwrap")
	}
}

// TestNewAppError verifies the basic constructor.
func TestNewAppError(t *testing.T) {
	underlying := errors.New("connection refused")
	appErr := NewAppError(ErrCodeUpstreamStripe, "stripe unavailable", underlying)

	if appErr.Code != ErrCodeUpstreamStripe {
		t.Errorf("Code = %q, want %q", appErr.Code, ErrCodeUpstreamStripe)
	}
	if appErr.Message != "stripe unavailable" {
		t.Errorf("Message = %q, want %q", appErr.Message, "stripe unavailable")
	}
	if appErr.Err != underlying {
		t.Errorf("Err = %v, want %v", appErr.Err, underlying)
	}
	if appErr.Details != nil {
		t.Errorf("Details should be nil, got %v", appErr.Details)
	}
}

// TestNewAppErrorWithNilErr verifies constructor works with nil underlying error.
func TestNewAppErrorWithNilErr(t *testing.T) {
	appErr := NewAppError(ErrCodeNotFoundUser, "user not found", nil)

	if appErr.Err != nil {
		t.Errorf("Err should be nil, got %v", appErr.Err)
	}
	if appErr.Error() != "not_found_user: user not found" {
		t.Errorf("Error() = %q, unexpected format", appErr.Error())
	}
}

// TestNewAppErrorWithDetails verifies the detailed constructor.
func TestNewAppErrorWithDetails(t *testing.T) {
	details := map[string]any{
		"field": "latitude",
		"value": 95.0,
	}
	appErr := NewAppErrorWithDetails(
		ErrCodeValidationInvalidLat,
		"latitude out of range",
		nil,
		details,
	)

	if appErr.Code != ErrCodeValidationInvalidLat {
		t.Errorf("Code = %q, want %q", appErr.Code, ErrCodeValidationInvalidLat)
	}
	if appErr.Details == nil {
		t.Fatal("Details should not be nil")
	}
	if appErr.Details["field"] != "latitude" {
		t.Errorf("Details[\"field\"] = %v, want \"latitude\"", appErr.Details["field"])
	}
	if appErr.Details["value"] != 95.0 {
		t.Errorf("Details[\"value\"] = %v, want 95.0", appErr.Details["value"])
	}
}

// TestAppErrorWithDetails verifies the WithDetails method creates a copy with merged details.
func TestAppErrorWithDetails(t *testing.T) {
	original := NewAppErrorWithDetails(
		ErrCodeValidationMissingField,
		"field is required",
		nil,
		map[string]any{"field": "name"},
	)

	enhanced := original.WithDetails(map[string]any{
		"suggestion": "provide a non-empty name",
	})

	// Original should be unchanged.
	if _, ok := original.Details["suggestion"]; ok {
		t.Error("WithDetails should not mutate the original error")
	}

	// Enhanced should have both details.
	if enhanced.Details["field"] != "name" {
		t.Errorf("enhanced should retain original detail: field = %v", enhanced.Details["field"])
	}
	if enhanced.Details["suggestion"] != "provide a non-empty name" {
		t.Errorf("enhanced should have new detail: suggestion = %v", enhanced.Details["suggestion"])
	}

	// Code and Message should carry over.
	if enhanced.Code != original.Code {
		t.Errorf("Code should carry over: got %q, want %q", enhanced.Code, original.Code)
	}
	if enhanced.Message != original.Message {
		t.Errorf("Message should carry over: got %q, want %q", enhanced.Message, original.Message)
	}
}

// TestAppErrorWithDetailsOverwrite verifies that WithDetails overwrites existing keys.
func TestAppErrorWithDetailsOverwrite(t *testing.T) {
	original := NewAppErrorWithDetails(
		ErrCodeValidationInvalidLat,
		"invalid",
		nil,
		map[string]any{"field": "lat", "value": 95.0},
	)

	enhanced := original.WithDetails(map[string]any{"value": -100.0})

	if enhanced.Details["value"] != -100.0 {
		t.Errorf("WithDetails should overwrite existing key: value = %v, want -100.0", enhanced.Details["value"])
	}
	if enhanced.Details["field"] != "lat" {
		t.Errorf("WithDetails should retain non-overwritten keys: field = %v", enhanced.Details["field"])
	}
}

// TestAppErrorWithDetailsNilOriginal verifies WithDetails works when original has no details.
func TestAppErrorWithDetailsNilOriginal(t *testing.T) {
	original := NewAppError(ErrCodeNotFoundOrg, "not found", nil)
	enhanced := original.WithDetails(map[string]any{"id": "org_123"})

	if enhanced.Details["id"] != "org_123" {
		t.Errorf("WithDetails on nil original should work: id = %v", enhanced.Details["id"])
	}
}

// TestAppErrorHTTPStatus verifies the convenience method on AppError.
func TestAppErrorHTTPStatus(t *testing.T) {
	appErr := NewAppError(ErrCodeNotFoundWatchpoint, "not found", nil)
	if appErr.HTTPStatus() != http.StatusNotFound {
		t.Errorf("HTTPStatus() = %d, want %d", appErr.HTTPStatus(), http.StatusNotFound)
	}
}

// TestErrorCodeHTTPStatusMapping verifies the mapping from error codes to HTTP statuses.
// This is a comprehensive test covering every error code category.
func TestErrorCodeHTTPStatusMapping(t *testing.T) {
	tests := []struct {
		code       ErrorCode
		wantStatus int
	}{
		// Validation (400)
		{ErrCodeValidationInvalidLat, http.StatusBadRequest},
		{ErrCodeValidationInvalidLon, http.StatusBadRequest},
		{ErrCodeValidationInvalidTimezone, http.StatusBadRequest},
		{ErrCodeValidationInvalidConditions, http.StatusBadRequest},
		{ErrCodeValidationThresholdRange, http.StatusBadRequest},
		{ErrCodeValidationTimeWindow, http.StatusBadRequest},
		{ErrCodeValidationMissingField, http.StatusBadRequest},
		{ErrCodeValidationInvalidEmail, http.StatusBadRequest},
		{ErrCodeValidationInvalidWebhook, http.StatusBadRequest},
		{ErrCodeValidationMaxConditions, http.StatusBadRequest},

		// Auth (401)
		{ErrCodeAuthTokenMissing, http.StatusUnauthorized},
		{ErrCodeAuthTokenInvalid, http.StatusUnauthorized},
		{ErrCodeAuthTokenExpired, http.StatusUnauthorized},
		{ErrCodeAuthTokenRevoked, http.StatusUnauthorized},
		{ErrCodeAuthSessionExpired, http.StatusUnauthorized},
		{ErrCodeAuthInvalidCreds, http.StatusUnauthorized},
		{ErrCodeAuthUserNotFound, http.StatusUnauthorized},

		// Auth overrides (non-401)
		{ErrCodeAuthLocked, http.StatusTooManyRequests},
		{ErrCodeAuthAccountNotActive, http.StatusForbidden},
		{ErrCodeAuthEmailNotVerified, http.StatusForbidden},
		{ErrCodeAuthOrgDeleted, http.StatusForbidden},
		{ErrCodeAuthProviderMismatch, http.StatusConflict},

		// Permission (403)
		{ErrCodePermissionScope, http.StatusForbidden},
		{ErrCodePermissionOrgMismatch, http.StatusForbidden},
		{ErrCodePermissionRole, http.StatusForbidden},

		// Limits (403/429)
		{ErrCodeLimitWatchpoints, http.StatusForbidden},
		{ErrCodeLimitAPICalls, http.StatusTooManyRequests},
		{ErrCodeRateLimit, http.StatusTooManyRequests},

		// Not Found (404)
		{ErrCodeNotFoundWatchpoint, http.StatusNotFound},
		{ErrCodeNotFoundOrg, http.StatusNotFound},
		{ErrCodeNotFoundUser, http.StatusNotFound},
		{ErrCodeNotFoundAPIKey, http.StatusNotFound},
		{ErrCodeNotFoundNotification, http.StatusNotFound},

		// Conflict (409)
		{ErrCodeConflictPaused, http.StatusConflict},
		{ErrCodeConflictActive, http.StatusConflict},
		{ErrCodeConflictEmail, http.StatusConflict},
		{ErrCodeConflictConcurrent, http.StatusConflict},
		{ErrCodeConflictIdempotency, http.StatusConflict},

		// Internal (500)
		{ErrCodeInternalDB, http.StatusInternalServerError},
		{ErrCodeInternalUnexpected, http.StatusInternalServerError},

		// Upstream (502)
		{ErrCodeUpstreamStripe, http.StatusBadGateway},
		{ErrCodeUpstreamEmailProvider, http.StatusBadGateway},
		{ErrCodeUpstreamForecast, http.StatusBadGateway},
		{ErrCodeUpstreamRateLimited, http.StatusBadGateway},

		// Payment-specific
		{ErrCodePaymentDeclined, http.StatusPaymentRequired},
		{ErrCodeEmailBlocked, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			got := tt.code.HTTPStatus()
			if got != tt.wantStatus {
				t.Errorf("ErrorCode(%q).HTTPStatus() = %d, want %d", tt.code, got, tt.wantStatus)
			}
		})
	}
}

// TestErrorCodeHTTPStatusUnknown verifies that unrecognized codes default to 500.
func TestErrorCodeHTTPStatusUnknown(t *testing.T) {
	unknown := ErrorCode("totally_unknown_error")
	if unknown.HTTPStatus() != http.StatusInternalServerError {
		t.Errorf("unknown ErrorCode.HTTPStatus() = %d, want %d", unknown.HTTPStatus(), http.StatusInternalServerError)
	}
}

// TestAllErrorCodeStringValues verifies every error constant has the expected string value.
// This is a regression test to ensure nobody accidentally changes a constant's value.
func TestAllErrorCodeStringValues(t *testing.T) {
	tests := []struct {
		code     ErrorCode
		expected string
	}{
		// Validation
		{ErrCodeValidationInvalidLat, "validation_invalid_latitude"},
		{ErrCodeValidationInvalidLon, "validation_invalid_longitude"},
		{ErrCodeValidationInvalidTimezone, "validation_invalid_timezone"},
		{ErrCodeValidationInvalidConditions, "validation_invalid_conditions"},
		{ErrCodeValidationThresholdRange, "validation_threshold_out_of_range"},
		{ErrCodeValidationTimeWindow, "validation_time_window_invalid"},
		{ErrCodeValidationMissingField, "validation_missing_required_field"},
		{ErrCodeValidationInvalidEmail, "validation_invalid_email"},
		{ErrCodeValidationInvalidWebhook, "validation_invalid_webhook_url"},
		{ErrCodeValidationMaxConditions, "validation_too_many_conditions"},

		// Auth
		{ErrCodeAuthTokenMissing, "auth_token_missing"},
		{ErrCodeAuthTokenInvalid, "auth_token_invalid"},
		{ErrCodeAuthTokenExpired, "auth_token_expired"},
		{ErrCodeAuthTokenRevoked, "auth_token_revoked"},
		{ErrCodeAuthSessionExpired, "auth_session_expired"},
		{ErrCodeAuthInvalidCreds, "auth_invalid_credentials"},
		{ErrCodeAuthUserNotFound, "auth_user_not_found"},
		{ErrCodeAuthLocked, "auth_account_locked"},
		{ErrCodeAuthAccountNotActive, "auth_account_not_active"},
		{ErrCodeAuthEmailNotVerified, "auth_email_not_verified"},
		{ErrCodeAuthOrgDeleted, "auth_organization_deleted"},
		{ErrCodeAuthProviderMismatch, "auth_provider_mismatch"},

		// Permission
		{ErrCodePermissionScope, "permission_scope_insufficient"},
		{ErrCodePermissionOrgMismatch, "permission_organization_mismatch"},
		{ErrCodePermissionRole, "permission_role_insufficient"},

		// Limits
		{ErrCodeLimitWatchpoints, "limit_watchpoints_exceeded"},
		{ErrCodeLimitAPICalls, "limit_api_rate_exceeded"},
		{ErrCodeRateLimit, "rate_limit_exceeded"},

		// Not Found
		{ErrCodeNotFoundWatchpoint, "not_found_watchpoint"},
		{ErrCodeNotFoundOrg, "not_found_organization"},
		{ErrCodeNotFoundUser, "not_found_user"},
		{ErrCodeNotFoundAPIKey, "not_found_api_key"},
		{ErrCodeNotFoundNotification, "not_found_notification"},

		// Conflict
		{ErrCodeConflictPaused, "conflict_already_paused"},
		{ErrCodeConflictActive, "conflict_already_active"},
		{ErrCodeConflictEmail, "conflict_email_exists"},
		{ErrCodeConflictConcurrent, "conflict_concurrent_modification"},
		{ErrCodeConflictIdempotency, "conflict_idempotency_mismatch"},

		// Internal/Upstream
		{ErrCodeInternalDB, "internal_database_error"},
		{ErrCodeInternalUnexpected, "internal_unexpected_error"},
		{ErrCodeUpstreamStripe, "upstream_stripe_unavailable"},
		{ErrCodeUpstreamEmailProvider, "upstream_email_provider_unavailable"},
		{ErrCodeUpstreamForecast, "upstream_forecast_unavailable"},
		{ErrCodeUpstreamRateLimited, "upstream_rate_limited"},

		// Payment-specific
		{ErrCodePaymentDeclined, "payment_declined"},
		{ErrCodeEmailBlocked, "email_blocked"},
	}

	for _, tt := range tests {
		if string(tt.code) != tt.expected {
			t.Errorf("ErrorCode constant %q has value %q, want %q", tt.code, string(tt.code), tt.expected)
		}
	}
}

// TestAppErrorFmtStringer verifies that AppError produces readable output in fmt functions.
func TestAppErrorFmtStringer(t *testing.T) {
	appErr := NewAppError(ErrCodeConflictEmail, "email already in use", nil)
	result := fmt.Sprintf("got error: %v", appErr)
	expected := "got error: conflict_email_exists: email already in use"
	if result != expected {
		t.Errorf("fmt.Sprintf(\"%%v\") = %q, want %q", result, expected)
	}
}

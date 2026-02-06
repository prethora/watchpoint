package types

import (
	"fmt"
	"net/http"
	"strings"
)

// ErrorCode is a typed string for categorizing application errors.
type ErrorCode string

// Complete error code constants.
// All handlers MUST use these constants instead of hardcoded strings.
const (
	// Validation (400)
	ErrCodeValidationInvalidLat        ErrorCode = "validation_invalid_latitude"
	ErrCodeValidationInvalidLon        ErrorCode = "validation_invalid_longitude"
	ErrCodeValidationInvalidTimezone   ErrorCode = "validation_invalid_timezone"
	ErrCodeValidationInvalidConditions ErrorCode = "validation_invalid_conditions"
	ErrCodeValidationThresholdRange    ErrorCode = "validation_threshold_out_of_range"
	ErrCodeValidationTimeWindow        ErrorCode = "validation_time_window_invalid"
	ErrCodeValidationMissingField      ErrorCode = "validation_missing_required_field"
	ErrCodeValidationInvalidEmail      ErrorCode = "validation_invalid_email"
	ErrCodeValidationInvalidWebhook    ErrorCode = "validation_invalid_webhook_url"
	ErrCodeValidationMaxConditions     ErrorCode = "validation_too_many_conditions"
	ErrCodeValidationBatchSize         ErrorCode = "validation_batch_size_exceeded"
	ErrCodeValidationInvalidVariable   ErrorCode = "validation_invalid_variable"
	ErrCodeBulkPartialFailure          ErrorCode = "bulk_partial_failure"

	// Auth (401)
	ErrCodeAuthTokenMissing      ErrorCode = "auth_token_missing"
	ErrCodeAuthTokenInvalid      ErrorCode = "auth_token_invalid"
	ErrCodeAuthTokenExpired      ErrorCode = "auth_token_expired"
	ErrCodeAuthTokenRevoked      ErrorCode = "auth_token_revoked"
	ErrCodeAuthSessionExpired    ErrorCode = "auth_session_expired"
	ErrCodeAuthInvalidCreds      ErrorCode = "auth_invalid_credentials"
	ErrCodeAuthUserNotFound      ErrorCode = "auth_user_not_found"
	ErrCodeAuthLocked            ErrorCode = "auth_account_locked"
	ErrCodeAuthAccountNotActive  ErrorCode = "auth_account_not_active"
	ErrCodeAuthEmailNotVerified  ErrorCode = "auth_email_not_verified"
	ErrCodeAuthOrgDeleted        ErrorCode = "auth_organization_deleted"
	ErrCodeAuthProviderMismatch  ErrorCode = "auth_provider_mismatch"

	// Permission (403)
	ErrCodePermissionScope      ErrorCode = "permission_scope_insufficient"
	ErrCodePermissionOrgMismatch ErrorCode = "permission_organization_mismatch"
	ErrCodePermissionRole       ErrorCode = "permission_role_insufficient"

	// Limits (403/429)
	ErrCodeLimitWatchpoints ErrorCode = "limit_watchpoints_exceeded"
	ErrCodeLimitAPICalls    ErrorCode = "limit_api_rate_exceeded"
	ErrCodeRateLimit        ErrorCode = "rate_limit_exceeded"

	// Not Found (404)
	ErrCodeNotFoundWatchpoint   ErrorCode = "not_found_watchpoint"
	ErrCodeNotFoundOrg          ErrorCode = "not_found_organization"
	ErrCodeNotFoundUser         ErrorCode = "not_found_user"
	ErrCodeNotFoundAPIKey       ErrorCode = "not_found_api_key"
	ErrCodeNotFoundNotification ErrorCode = "not_found_notification"

	// Conflict (409)
	ErrCodeConflictPaused      ErrorCode = "conflict_already_paused"
	ErrCodeConflictActive      ErrorCode = "conflict_already_active"
	ErrCodeConflictEmail       ErrorCode = "conflict_email_exists"
	ErrCodeConflictConcurrent  ErrorCode = "conflict_concurrent_modification"
	ErrCodeConflictIdempotency ErrorCode = "conflict_idempotency_mismatch"

	// Internal/Upstream (500/502)
	ErrCodeInternalDB                  ErrorCode = "internal_database_error"
	ErrCodeInternalUnexpected          ErrorCode = "internal_unexpected_error"
	ErrCodeInternalForecastCorruption  ErrorCode = "internal_forecast_corruption"
	ErrCodeUpstreamStripe        ErrorCode = "upstream_stripe_unavailable"
	ErrCodeUpstreamEmailProvider ErrorCode = "upstream_email_provider_unavailable"
	ErrCodeUpstreamForecast      ErrorCode = "upstream_forecast_unavailable"
	ErrCodeUpstreamUnavailable   ErrorCode = "upstream_unavailable"
	ErrCodeUpstreamRateLimited   ErrorCode = "upstream_rate_limited"

	// Payment-specific
	ErrCodePaymentDeclined ErrorCode = "payment_declined"
	ErrCodeEmailBlocked    ErrorCode = "email_blocked"
)

// HTTPStatus maps an ErrorCode to its corresponding HTTP status code.
// Used by the API layer to translate AppErrors into HTTP responses.
// Returns 500 for unrecognized error codes as a safe default.
func (c ErrorCode) HTTPStatus() int {
	s := string(c)
	switch {
	case strings.HasPrefix(s, "validation_"):
		return http.StatusBadRequest // 400
	case s == string(ErrCodeAuthLocked):
		return http.StatusTooManyRequests // 429
	case s == string(ErrCodeAuthAccountNotActive),
		s == string(ErrCodeAuthEmailNotVerified),
		s == string(ErrCodeAuthOrgDeleted):
		return http.StatusForbidden // 403
	case s == string(ErrCodeAuthProviderMismatch):
		return http.StatusConflict // 409
	case strings.HasPrefix(s, "auth_"):
		return http.StatusUnauthorized // 401
	case strings.HasPrefix(s, "permission_"):
		return http.StatusForbidden // 403
	case s == string(ErrCodeLimitWatchpoints):
		return http.StatusForbidden // 403
	case s == string(ErrCodeLimitAPICalls), s == string(ErrCodeRateLimit):
		return http.StatusTooManyRequests // 429
	case strings.HasPrefix(s, "not_found_"):
		return http.StatusNotFound // 404
	case strings.HasPrefix(s, "conflict_"):
		return http.StatusConflict // 409
	case s == string(ErrCodePaymentDeclined):
		return http.StatusPaymentRequired // 402
	case s == string(ErrCodeEmailBlocked):
		return http.StatusForbidden // 403
	case strings.HasPrefix(s, "upstream_"):
		return http.StatusBadGateway // 502
	case strings.HasPrefix(s, "internal_"):
		return http.StatusInternalServerError // 500
	default:
		return http.StatusInternalServerError // 500
	}
}

// AppError is the standard application error type used throughout the platform.
// All domain and handler errors should be expressed as AppError to enable
// consistent error formatting, HTTP status mapping, and error chain support.
type AppError struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Err     error          `json:"-"`
	Details map[string]any `json:"details,omitempty"`
}

// Error implements the error interface.
func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying error for errors.Is/errors.As support.
func (e *AppError) Unwrap() error {
	return e.Err
}

// HTTPStatus returns the HTTP status code corresponding to this error's code.
func (e *AppError) HTTPStatus() int {
	return e.Code.HTTPStatus()
}

// WithDetails returns a copy of the error with the provided details merged in.
// This is useful for adding context without mutating the original error.
func (e *AppError) WithDetails(details map[string]any) *AppError {
	merged := make(map[string]any, len(e.Details)+len(details))
	for k, v := range e.Details {
		merged[k] = v
	}
	for k, v := range details {
		merged[k] = v
	}
	return &AppError{
		Code:    e.Code,
		Message: e.Message,
		Err:     e.Err,
		Details: merged,
	}
}

// NewAppError creates a new AppError with the given code, message, and optional
// underlying error. This is the standard constructor for domain errors.
func NewAppError(code ErrorCode, message string, err error) *AppError {
	return &AppError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}

// NewAppErrorWithDetails creates a new AppError with the given code, message,
// underlying error, and structured details.
func NewAppErrorWithDetails(code ErrorCode, message string, err error, details map[string]any) *AppError {
	return &AppError{
		Code:    code,
		Message: message,
		Err:     err,
		Details: details,
	}
}

package core

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"watchpoint/internal/types"
)

// maxRequestBodySize is the maximum allowed size of a request body (1 MB).
const maxRequestBodySize = 1 << 20 // 1 MB

// APIResponse is the standard envelope for all successful API responses.
// Uses types.ResponseMeta to convey non-blocking warnings (e.g., deprecation notices).
type APIResponse struct {
	Data interface{}         `json:"data,omitempty"`
	Meta *types.ResponseMeta `json:"meta,omitempty"`
}

// APIErrorResponse is the standard envelope for all error API responses.
type APIErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains the structured error information returned to clients.
type ErrorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
}

// JSON writes a JSON response with the given status code and data.
// It sets the Content-Type header, marshals the data, and writes the response.
// If marshalling fails, it falls back to a 500 error response.
func JSON(w http.ResponseWriter, r *http.Request, status int, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		// Fall back to a plain error if marshalling fails.
		// Log at the call site is not available; use the raw writer.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fallback := APIErrorResponse{
			Error: ErrorDetail{
				Code:      string(types.ErrCodeInternalUnexpected),
				Message:   "failed to marshal response",
				RequestID: types.GetRequestID(r.Context()),
			},
		}
		// Best-effort write; if this also fails, there is nothing more we can do.
		_ = json.NewEncoder(w).Encode(fallback)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Error writes an error response to the client. It inspects the error chain:
//   - If the error is (or wraps) a *types.AppError, it uses its Code to determine
//     the HTTP status and writes a structured APIErrorResponse.
//   - If the error is a generic (non-AppError) error, it returns a 500 Internal
//     Server Error with the code "internal_unexpected_error".
//
// Internal error details (wrapped errors) are never exposed to the client.
// The original error message from generic errors is also not exposed to prevent
// information leakage; a safe default message is used instead.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	requestID := types.GetRequestID(r.Context())

	var appErr *types.AppError
	if errors.As(err, &appErr) {
		status := appErr.HTTPStatus()
		resp := APIErrorResponse{
			Error: ErrorDetail{
				Code:      string(appErr.Code),
				Message:   appErr.Message,
				Details:   appErr.Details,
				RequestID: requestID,
			},
		}
		JSON(w, r, status, resp)
		return
	}

	// Generic error: return 500 without leaking internal details.
	resp := APIErrorResponse{
		Error: ErrorDetail{
			Code:      string(types.ErrCodeInternalUnexpected),
			Message:   "an unexpected error occurred",
			RequestID: requestID,
		},
	}
	JSON(w, r, http.StatusInternalServerError, resp)
}

// DecodeJSON reads the request body into dst, enforcing:
//   - A maximum body size of 1 MB to prevent abuse.
//   - DisallowUnknownFields to enforce strict JSON contracts.
//
// It returns a *types.AppError with code "validation_invalid_json" (400) on:
//   - JSON syntax errors
//   - Unknown fields in the request body
//   - Body exceeding the size limit
//   - Empty body
//   - Body containing more than one JSON value
//
// On success, it returns nil.
//
// The w parameter is accepted for signature consistency with the other response
// utilities but is not used directly; callers handle writing error responses.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	// Enforce max body size. Pass w to MaxBytesReader so that further writes
	// to the body after the limit is hit trigger the appropriate error.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return mapDecodeError(err)
	}

	// Ensure the body contains only a single JSON value.
	// A second Decode call should return io.EOF if the body is well-formed.
	if dec.More() {
		return types.NewAppError(
			errCodeValidationInvalidJSON,
			"request body must contain a single JSON object",
			nil,
		)
	}

	return nil
}

// errCodeValidationInvalidJSON is the error code for malformed JSON input.
// This is a local constant because it's specific to the API chassis layer
// and the architecture spec refers to it as "validation_invalid_json".
const errCodeValidationInvalidJSON types.ErrorCode = "validation_invalid_json"

// mapDecodeError translates a json.Decoder error into a structured AppError.
func mapDecodeError(err error) *types.AppError {
	// Check for max bytes exceeded (request too large).
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return types.NewAppError(
			errCodeValidationInvalidJSON,
			"request body must not exceed 1MB",
			err,
		)
	}

	// Check for JSON syntax errors.
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return types.NewAppError(
			errCodeValidationInvalidJSON,
			"malformed JSON in request body",
			err,
		)
	}

	// Check for type mismatch errors.
	var unmarshalTypeErr *json.UnmarshalTypeError
	if errors.As(err, &unmarshalTypeErr) {
		return types.NewAppErrorWithDetails(
			errCodeValidationInvalidJSON,
			"invalid value for field",
			err,
			map[string]any{
				"field":    unmarshalTypeErr.Field,
				"expected": unmarshalTypeErr.Type.String(),
			},
		)
	}

	// Check for unknown field errors (DisallowUnknownFields).
	if strings.HasPrefix(err.Error(), "json: unknown field") {
		return types.NewAppError(
			errCodeValidationInvalidJSON,
			"unknown field in request body: "+strings.TrimPrefix(err.Error(), "json: unknown field "),
			err,
		)
	}

	// Check for empty body.
	if errors.Is(err, io.EOF) {
		return types.NewAppError(
			errCodeValidationInvalidJSON,
			"request body must not be empty",
			err,
		)
	}

	// Fallback for any other decode error.
	return types.NewAppError(
		errCodeValidationInvalidJSON,
		"invalid JSON in request body",
		err,
	)
}


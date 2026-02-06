package core

import (
	"context"
	"time"

	"watchpoint/internal/types"
)

// Authenticator decouples the HTTP layer from specific auth mechanisms
// (DB lookups), allowing for easy mocking in tests.
type Authenticator interface {
	// ResolveToken inspects a token prefix ("sk_", "sess_") and returns the Actor.
	//
	// Resolution Strategy:
	// 1. Query the key record with WHERE clause: revoked_at IS NULL.
	// 2. If found (not revoked), compare expires_at vs NOW() to determine expiration.
	// 3. For API Key authentication, fetch the source column and populate Actor.Source.
	//    If the key has no source (NULL or empty), default to "default".
	//
	// Distinct Error Codes:
	// - Return ErrAuthTokenInvalid if the token is malformed, not found, or revoked.
	// - Return ErrAuthTokenExpired if the token exists and is not revoked, but expired.
	ResolveToken(ctx context.Context, token string) (*types.Actor, error)
}

// RateLimitStore abstracts the backing store for rate limiting.
// Production uses PostgreSQL atomic updates; dev/test uses in-memory.
type RateLimitStore interface {
	// IncrementAndCheck atomically increments the rate limit counter for the
	// given key and checks if the limit has been exceeded within the window.
	IncrementAndCheck(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error)
}

// RateLimitResult contains the outcome of a rate limit check.
type RateLimitResult struct {
	// Allowed indicates whether the request is within the rate limit.
	Allowed bool
	// Remaining is the number of requests remaining in the current window.
	Remaining int
	// ResetAt is the time when the current rate limit window resets.
	ResetAt time.Time
}

// IdempotencyRecord represents a stored idempotency key entry.
type IdempotencyRecord struct {
	// Key is the client-provided idempotency key.
	Key string
	// OrganizationID scopes the key to a specific organization.
	OrganizationID string
	// Status is the current processing state: "processing", "completed", or "failed".
	Status string
	// ResponseCode is the HTTP status code of the completed response.
	ResponseCode int
	// ResponseBody is the complete response body as raw JSON bytes.
	ResponseBody []byte
}

// Idempotency status constants.
const (
	IdempotencyStatusProcessing = "processing"
	IdempotencyStatusCompleted  = "completed"
	IdempotencyStatusFailed     = "failed"
)

// IdempotencyStore abstracts the backing store for idempotency key management.
// Production uses PostgreSQL; dev/test uses in-memory.
type IdempotencyStore interface {
	// Get retrieves an existing idempotency record by key and organization ID.
	// Returns nil and no error if the key does not exist.
	Get(ctx context.Context, key string, orgID string) (*IdempotencyRecord, error)

	// Create inserts a new idempotency record with status "processing".
	// Returns an error if the key already exists for the organization.
	Create(ctx context.Context, key string, orgID string, requestPath string) error

	// Complete marks an idempotency record as completed and stores the response.
	Complete(ctx context.Context, key string, orgID string, statusCode int, body []byte) error

	// Fail marks an idempotency record as failed. This allows the key to be
	// retried on subsequent requests.
	Fail(ctx context.Context, key string, orgID string) error
}

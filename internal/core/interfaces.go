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

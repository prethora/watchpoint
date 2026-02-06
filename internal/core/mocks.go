package core

import (
	"context"
	"sync"
	"time"

	"watchpoint/internal/types"
)

// --- MockAuthenticator ---

// MockAuthenticator implements the Authenticator interface for testing.
// It allows injecting a predefined Actor for a given token, or returning
// a fixed error to simulate authentication failures.
//
// Usage:
//
//	mock := &MockAuthenticator{
//	    Actor: &types.Actor{
//	        ID:             "user_test123",
//	        Type:           types.ActorTypeUser,
//	        OrganizationID: "org_test456",
//	    },
//	}
//	actor, err := mock.ResolveToken(ctx, "sess_abc123")
//
// To simulate an error:
//
//	mock := &MockAuthenticator{
//	    Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid token", nil),
//	}
type MockAuthenticator struct {
	// Actor is the predefined Actor returned on successful token resolution.
	// If nil and Err is also nil, ResolveToken returns (nil, nil).
	Actor *types.Actor

	// Err is the error returned by ResolveToken. When set, Actor is ignored.
	Err error

	// ResolveTokenFunc is an optional function that overrides the default behavior.
	// When set, it takes precedence over Actor and Err fields. This allows tests
	// to implement dynamic behavior based on the token value.
	ResolveTokenFunc func(ctx context.Context, token string) (*types.Actor, error)

	// mu protects Calls for concurrent access.
	mu sync.Mutex

	// Calls records every token passed to ResolveToken for assertion purposes.
	Calls []string
}

// ResolveToken implements the Authenticator interface.
// It records the call, then delegates to ResolveTokenFunc if set,
// otherwise returns Err (if set) or Actor.
func (m *MockAuthenticator) ResolveToken(ctx context.Context, token string) (*types.Actor, error) {
	m.mu.Lock()
	m.Calls = append(m.Calls, token)
	m.mu.Unlock()

	if m.ResolveTokenFunc != nil {
		return m.ResolveTokenFunc(ctx, token)
	}
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Actor, nil
}

// --- MockRateLimitStore ---

// MockRateLimitStore implements the RateLimitStore interface for testing.
// It allows injecting a predefined result or error to simulate rate limiting.
//
// Usage:
//
//	mock := &MockRateLimitStore{
//	    Result: RateLimitResult{Allowed: true, Remaining: 99, ResetAt: time.Now().Add(time.Hour)},
//	}
//	result, err := mock.IncrementAndCheck(ctx, "org_123", 100, time.Hour)
//
// To simulate rate limit exceeded:
//
//	mock := &MockRateLimitStore{
//	    Result: RateLimitResult{Allowed: false, Remaining: 0, ResetAt: time.Now().Add(30 * time.Minute)},
//	}
type MockRateLimitStore struct {
	// Result is the predefined RateLimitResult returned by IncrementAndCheck.
	Result RateLimitResult

	// Err is the error returned by IncrementAndCheck. When set, Result is still
	// returned alongside the error (consistent with typical Go patterns where
	// partial results may accompany errors).
	Err error

	// IncrementAndCheckFunc is an optional function that overrides the default behavior.
	// When set, it takes precedence over Result and Err fields.
	IncrementAndCheckFunc func(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error)

	// mu protects Calls for concurrent access.
	mu sync.Mutex

	// Calls records every invocation for assertion purposes.
	Calls []RateLimitCall
}

// RateLimitCall records the arguments of a single IncrementAndCheck invocation.
type RateLimitCall struct {
	Key    string
	Limit  int
	Window time.Duration
}

// IncrementAndCheck implements the RateLimitStore interface.
// It records the call, then delegates to IncrementAndCheckFunc if set,
// otherwise returns Result and Err.
func (m *MockRateLimitStore) IncrementAndCheck(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error) {
	m.mu.Lock()
	m.Calls = append(m.Calls, RateLimitCall{Key: key, Limit: limit, Window: window})
	m.mu.Unlock()

	if m.IncrementAndCheckFunc != nil {
		return m.IncrementAndCheckFunc(ctx, key, limit, window)
	}
	return m.Result, m.Err
}

// --- MockSecurityService ---

// MockSecurityService implements the types.SecurityService interface for testing.
// It allows injecting predefined responses for IP and identifier blocking checks,
// and records all calls for assertion.
//
// Usage:
//
//	mock := &MockSecurityService{
//	    BlockedIPs: map[string]bool{"192.168.1.100": true},
//	}
//	blocked := mock.IsIPBlocked(ctx, "192.168.1.100") // returns true
//	blocked = mock.IsIPBlocked(ctx, "10.0.0.1")       // returns false
type MockSecurityService struct {
	// BlockedIPs maps IP addresses to their blocked status. If an IP is not
	// present in the map, IsIPBlocked returns false.
	BlockedIPs map[string]bool

	// BlockedIdentifiers maps identifiers (e.g., emails) to their blocked status.
	// If an identifier is not present in the map, IsIdentifierBlocked returns false.
	BlockedIdentifiers map[string]bool

	// RecordAttemptErr is the error returned by RecordAttempt. Defaults to nil.
	RecordAttemptErr error

	// RecordAttemptFunc is an optional function that overrides the default behavior
	// of RecordAttempt. When set, it takes precedence over RecordAttemptErr.
	RecordAttemptFunc func(ctx context.Context, eventType, identifier, ip string, success bool, reason string) error

	// mu protects RecordedAttempts for concurrent access.
	mu sync.Mutex

	// RecordedAttempts stores all calls to RecordAttempt for assertion purposes.
	RecordedAttempts []SecurityAttemptCall
}

// SecurityAttemptCall records the arguments of a single RecordAttempt invocation.
type SecurityAttemptCall struct {
	EventType  string
	Identifier string
	IP         string
	Success    bool
	Reason     string
}

// RecordAttempt implements types.SecurityService.
// It records the call and returns RecordAttemptErr (or nil).
func (m *MockSecurityService) RecordAttempt(ctx context.Context, eventType, identifier, ip string, success bool, reason string) error {
	m.mu.Lock()
	m.RecordedAttempts = append(m.RecordedAttempts, SecurityAttemptCall{
		EventType:  eventType,
		Identifier: identifier,
		IP:         ip,
		Success:    success,
		Reason:     reason,
	})
	m.mu.Unlock()

	if m.RecordAttemptFunc != nil {
		return m.RecordAttemptFunc(ctx, eventType, identifier, ip, success, reason)
	}
	return m.RecordAttemptErr
}

// IsIPBlocked implements types.SecurityService.
// It returns true if the IP address is present in BlockedIPs and mapped to true.
func (m *MockSecurityService) IsIPBlocked(_ context.Context, ip string) bool {
	if m.BlockedIPs == nil {
		return false
	}
	return m.BlockedIPs[ip]
}

// IsIdentifierBlocked implements types.SecurityService.
// It returns true if the identifier is present in BlockedIdentifiers and mapped to true.
func (m *MockSecurityService) IsIdentifierBlocked(_ context.Context, identifier string) bool {
	if m.BlockedIdentifiers == nil {
		return false
	}
	return m.BlockedIdentifiers[identifier]
}

// Compile-time interface assertions.
var (
	_ Authenticator         = (*MockAuthenticator)(nil)
	_ RateLimitStore        = (*MockRateLimitStore)(nil)
	_ types.SecurityService = (*MockSecurityService)(nil)
)

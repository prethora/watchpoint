package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- MockAuthenticator Tests ---

func TestMockAuthenticator_ReturnsActor(t *testing.T) {
	expected := &types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
		Source:         "dashboard",
	}
	mock := &MockAuthenticator{Actor: expected}

	actor, err := mock.ResolveToken(context.Background(), "sess_test123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor == nil {
		t.Fatal("expected non-nil actor")
	}
	if actor.ID != expected.ID {
		t.Errorf("got actor ID %q, want %q", actor.ID, expected.ID)
	}
	if actor.Type != expected.Type {
		t.Errorf("got actor Type %q, want %q", actor.Type, expected.Type)
	}
	if actor.OrganizationID != expected.OrganizationID {
		t.Errorf("got OrganizationID %q, want %q", actor.OrganizationID, expected.OrganizationID)
	}
	if actor.Source != expected.Source {
		t.Errorf("got Source %q, want %q", actor.Source, expected.Source)
	}
}

func TestMockAuthenticator_ReturnsError(t *testing.T) {
	expectedErr := types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid token", nil)
	mock := &MockAuthenticator{Err: expectedErr}

	actor, err := mock.ResolveToken(context.Background(), "bad_token")
	if actor != nil {
		t.Errorf("expected nil actor when error is set, got %+v", actor)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("got error code %q, want %q", appErr.Code, types.ErrCodeAuthTokenInvalid)
	}
}

func TestMockAuthenticator_RecordsCalls(t *testing.T) {
	mock := &MockAuthenticator{
		Actor: &types.Actor{ID: "user_1", Type: types.ActorTypeUser},
	}

	tokens := []string{"token_a", "token_b", "token_c"}
	for _, tok := range tokens {
		_, _ = mock.ResolveToken(context.Background(), tok)
	}

	if len(mock.Calls) != len(tokens) {
		t.Fatalf("expected %d calls, got %d", len(tokens), len(mock.Calls))
	}
	for i, tok := range tokens {
		if mock.Calls[i] != tok {
			t.Errorf("call[%d]: got %q, want %q", i, mock.Calls[i], tok)
		}
	}
}

func TestMockAuthenticator_ResolveTokenFunc(t *testing.T) {
	mock := &MockAuthenticator{
		// These should be ignored when ResolveTokenFunc is set.
		Actor: &types.Actor{ID: "ignored"},
		Err:   errors.New("ignored"),
		ResolveTokenFunc: func(ctx context.Context, token string) (*types.Actor, error) {
			if token == "valid_token" {
				return &types.Actor{ID: "user_dynamic", Type: types.ActorTypeAPIKey}, nil
			}
			return nil, types.NewAppError(types.ErrCodeAuthTokenExpired, "expired", nil)
		},
	}

	// Test valid path
	actor, err := mock.ResolveToken(context.Background(), "valid_token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor.ID != "user_dynamic" {
		t.Errorf("got actor ID %q, want %q", actor.ID, "user_dynamic")
	}

	// Test error path
	actor, err = mock.ResolveToken(context.Background(), "expired_token")
	if actor != nil {
		t.Errorf("expected nil actor, got %+v", actor)
	}
	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeAuthTokenExpired {
		t.Errorf("got error code %q, want %q", appErr.Code, types.ErrCodeAuthTokenExpired)
	}

	// Verify calls were still recorded
	if len(mock.Calls) != 2 {
		t.Errorf("expected 2 calls recorded, got %d", len(mock.Calls))
	}
}

func TestMockAuthenticator_NilActorNilErr(t *testing.T) {
	mock := &MockAuthenticator{}

	actor, err := mock.ResolveToken(context.Background(), "some_token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor != nil {
		t.Errorf("expected nil actor, got %+v", actor)
	}
}

// --- MockRateLimitStore Tests ---

func TestMockRateLimitStore_Allowed(t *testing.T) {
	resetTime := time.Now().Add(time.Hour)
	mock := &MockRateLimitStore{
		Result: RateLimitResult{
			Allowed:   true,
			Remaining: 99,
			ResetAt:   resetTime,
		},
	}

	result, err := mock.IncrementAndCheck(context.Background(), "org_123", 100, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected Allowed to be true")
	}
	if result.Remaining != 99 {
		t.Errorf("got Remaining %d, want 99", result.Remaining)
	}
	if !result.ResetAt.Equal(resetTime) {
		t.Errorf("got ResetAt %v, want %v", result.ResetAt, resetTime)
	}
}

func TestMockRateLimitStore_Denied(t *testing.T) {
	mock := &MockRateLimitStore{
		Result: RateLimitResult{
			Allowed:   false,
			Remaining: 0,
			ResetAt:   time.Now().Add(30 * time.Minute),
		},
	}

	result, err := mock.IncrementAndCheck(context.Background(), "org_123", 100, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected Allowed to be false")
	}
	if result.Remaining != 0 {
		t.Errorf("got Remaining %d, want 0", result.Remaining)
	}
}

func TestMockRateLimitStore_Error(t *testing.T) {
	expectedErr := errors.New("store unavailable")
	mock := &MockRateLimitStore{
		Err: expectedErr,
	}

	_, err := mock.IncrementAndCheck(context.Background(), "org_123", 100, time.Hour)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}
}

func TestMockRateLimitStore_RecordsCalls(t *testing.T) {
	mock := &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 50},
	}

	_, _ = mock.IncrementAndCheck(context.Background(), "org_a", 100, time.Hour)
	_, _ = mock.IncrementAndCheck(context.Background(), "org_b", 200, 2*time.Hour)

	if len(mock.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.Calls))
	}
	if mock.Calls[0].Key != "org_a" {
		t.Errorf("call[0] key: got %q, want %q", mock.Calls[0].Key, "org_a")
	}
	if mock.Calls[0].Limit != 100 {
		t.Errorf("call[0] limit: got %d, want 100", mock.Calls[0].Limit)
	}
	if mock.Calls[1].Key != "org_b" {
		t.Errorf("call[1] key: got %q, want %q", mock.Calls[1].Key, "org_b")
	}
	if mock.Calls[1].Limit != 200 {
		t.Errorf("call[1] limit: got %d, want 200", mock.Calls[1].Limit)
	}
}

func TestMockRateLimitStore_IncrementAndCheckFunc(t *testing.T) {
	callCount := 0
	mock := &MockRateLimitStore{
		// Should be ignored when func is set.
		Result: RateLimitResult{Allowed: false},
		IncrementAndCheckFunc: func(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error) {
			callCount++
			if callCount > 2 {
				return RateLimitResult{Allowed: false, Remaining: 0}, nil
			}
			return RateLimitResult{Allowed: true, Remaining: limit - callCount}, nil
		},
	}

	r1, _ := mock.IncrementAndCheck(context.Background(), "k", 10, time.Hour)
	if !r1.Allowed {
		t.Error("first call should be allowed")
	}
	r2, _ := mock.IncrementAndCheck(context.Background(), "k", 10, time.Hour)
	if !r2.Allowed {
		t.Error("second call should be allowed")
	}
	r3, _ := mock.IncrementAndCheck(context.Background(), "k", 10, time.Hour)
	if r3.Allowed {
		t.Error("third call should be denied")
	}
}

// --- MockSecurityService Tests ---

func TestMockSecurityService_IsIPBlocked(t *testing.T) {
	mock := &MockSecurityService{
		BlockedIPs: map[string]bool{
			"192.168.1.100": true,
			"10.0.0.5":      true,
		},
	}

	tests := []struct {
		ip      string
		blocked bool
	}{
		{"192.168.1.100", true},
		{"10.0.0.5", true},
		{"172.16.0.1", false},
		{"8.8.8.8", false},
	}

	for _, tc := range tests {
		got := mock.IsIPBlocked(context.Background(), tc.ip)
		if got != tc.blocked {
			t.Errorf("IsIPBlocked(%q): got %v, want %v", tc.ip, got, tc.blocked)
		}
	}
}

func TestMockSecurityService_IsIPBlocked_NilMap(t *testing.T) {
	mock := &MockSecurityService{}

	if mock.IsIPBlocked(context.Background(), "1.2.3.4") {
		t.Error("IsIPBlocked should return false when BlockedIPs is nil")
	}
}

func TestMockSecurityService_IsIdentifierBlocked(t *testing.T) {
	mock := &MockSecurityService{
		BlockedIdentifiers: map[string]bool{
			"attacker@evil.com": true,
		},
	}

	if !mock.IsIdentifierBlocked(context.Background(), "attacker@evil.com") {
		t.Error("expected attacker@evil.com to be blocked")
	}
	if mock.IsIdentifierBlocked(context.Background(), "user@good.com") {
		t.Error("expected user@good.com to not be blocked")
	}
}

func TestMockSecurityService_IsIdentifierBlocked_NilMap(t *testing.T) {
	mock := &MockSecurityService{}

	if mock.IsIdentifierBlocked(context.Background(), "test@example.com") {
		t.Error("IsIdentifierBlocked should return false when BlockedIdentifiers is nil")
	}
}

func TestMockSecurityService_RecordAttempt(t *testing.T) {
	mock := &MockSecurityService{}

	err := mock.RecordAttempt(context.Background(), "login", "user@test.com", "192.168.1.1", true, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = mock.RecordAttempt(context.Background(), "api_auth", "", "10.0.0.1", false, "invalid_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.RecordedAttempts) != 2 {
		t.Fatalf("expected 2 recorded attempts, got %d", len(mock.RecordedAttempts))
	}

	first := mock.RecordedAttempts[0]
	if first.EventType != "login" {
		t.Errorf("attempt[0] EventType: got %q, want %q", first.EventType, "login")
	}
	if first.Identifier != "user@test.com" {
		t.Errorf("attempt[0] Identifier: got %q, want %q", first.Identifier, "user@test.com")
	}
	if first.IP != "192.168.1.1" {
		t.Errorf("attempt[0] IP: got %q, want %q", first.IP, "192.168.1.1")
	}
	if !first.Success {
		t.Error("attempt[0] Success: expected true")
	}

	second := mock.RecordedAttempts[1]
	if second.EventType != "api_auth" {
		t.Errorf("attempt[1] EventType: got %q, want %q", second.EventType, "api_auth")
	}
	if second.Success {
		t.Error("attempt[1] Success: expected false")
	}
	if second.Reason != "invalid_key" {
		t.Errorf("attempt[1] Reason: got %q, want %q", second.Reason, "invalid_key")
	}
}

func TestMockSecurityService_RecordAttemptErr(t *testing.T) {
	expectedErr := errors.New("db connection failed")
	mock := &MockSecurityService{
		RecordAttemptErr: expectedErr,
	}

	err := mock.RecordAttempt(context.Background(), "login", "user@test.com", "1.2.3.4", false, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}

	// Should still record the attempt
	if len(mock.RecordedAttempts) != 1 {
		t.Errorf("expected 1 recorded attempt, got %d", len(mock.RecordedAttempts))
	}
}

func TestMockSecurityService_RecordAttemptFunc(t *testing.T) {
	callCount := 0
	mock := &MockSecurityService{
		RecordAttemptFunc: func(ctx context.Context, eventType, identifier, ip string, success bool, reason string) error {
			callCount++
			if callCount > 2 {
				return errors.New("rate limited")
			}
			return nil
		},
	}

	if err := mock.RecordAttempt(context.Background(), "login", "", "1.1.1.1", true, ""); err != nil {
		t.Errorf("call 1: unexpected error: %v", err)
	}
	if err := mock.RecordAttempt(context.Background(), "login", "", "2.2.2.2", true, ""); err != nil {
		t.Errorf("call 2: unexpected error: %v", err)
	}
	if err := mock.RecordAttempt(context.Background(), "login", "", "3.3.3.3", true, ""); err == nil {
		t.Error("call 3: expected error, got nil")
	}
}

// --- Interface Satisfaction Tests ---

func TestMockAuthenticator_ImplementsAuthenticator(t *testing.T) {
	var _ Authenticator = (*MockAuthenticator)(nil)
}

func TestMockRateLimitStore_ImplementsRateLimitStore(t *testing.T) {
	var _ RateLimitStore = (*MockRateLimitStore)(nil)
}

func TestMockSecurityService_ImplementsSecurityService(t *testing.T) {
	var _ types.SecurityService = (*MockSecurityService)(nil)
}

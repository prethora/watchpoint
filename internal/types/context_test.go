package types

import (
	"context"
	"testing"
)

// mockLogger implements the Logger interface for testing purposes.
type mockLogger struct {
	messages []string
}

func (m *mockLogger) Info(msg string, args ...any)  { m.messages = append(m.messages, "info:"+msg) }
func (m *mockLogger) Error(msg string, args ...any) { m.messages = append(m.messages, "error:"+msg) }
func (m *mockLogger) Warn(msg string, args ...any)  { m.messages = append(m.messages, "warn:"+msg) }
func (m *mockLogger) With(args ...any) Logger        { return m }

func TestWithActor_GetActor(t *testing.T) {
	t.Run("round-trip stores and retrieves actor", func(t *testing.T) {
		actor := Actor{
			ID:             "user-123",
			Type:           ActorTypeUser,
			OrganizationID: "org-456",
			IsTestMode:     false,
			Source:         "dashboard",
		}
		ctx := WithActor(context.Background(), actor)
		got, ok := GetActor(ctx)
		if !ok {
			t.Fatal("expected ok to be true, got false")
		}
		if got.ID != actor.ID {
			t.Errorf("ID: got %q, want %q", got.ID, actor.ID)
		}
		if got.Type != actor.Type {
			t.Errorf("Type: got %q, want %q", got.Type, actor.Type)
		}
		if got.OrganizationID != actor.OrganizationID {
			t.Errorf("OrganizationID: got %q, want %q", got.OrganizationID, actor.OrganizationID)
		}
		if got.IsTestMode != actor.IsTestMode {
			t.Errorf("IsTestMode: got %v, want %v", got.IsTestMode, actor.IsTestMode)
		}
		if got.Source != actor.Source {
			t.Errorf("Source: got %q, want %q", got.Source, actor.Source)
		}
	})

	t.Run("test mode actor round-trip", func(t *testing.T) {
		actor := Actor{
			ID:             "key-789",
			Type:           ActorTypeAPIKey,
			OrganizationID: "org-111",
			IsTestMode:     true,
			Source:         "wedding_app",
		}
		ctx := WithActor(context.Background(), actor)
		got, ok := GetActor(ctx)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if !got.IsTestMode {
			t.Error("expected IsTestMode to be true")
		}
		if got.Type != ActorTypeAPIKey {
			t.Errorf("Type: got %q, want %q", got.Type, ActorTypeAPIKey)
		}
	})

	t.Run("system actor round-trip", func(t *testing.T) {
		actor := Actor{
			ID:   "system",
			Type: ActorTypeSystem,
		}
		ctx := WithActor(context.Background(), actor)
		got, ok := GetActor(ctx)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if got.Type != ActorTypeSystem {
			t.Errorf("Type: got %q, want %q", got.Type, ActorTypeSystem)
		}
	})

	t.Run("returns false when no actor in context", func(t *testing.T) {
		_, ok := GetActor(context.Background())
		if ok {
			t.Error("expected ok to be false for empty context")
		}
	})

	t.Run("returns zero-value actor when missing", func(t *testing.T) {
		actor, ok := GetActor(context.Background())
		if ok {
			t.Error("expected ok to be false")
		}
		if actor.ID != "" {
			t.Errorf("expected empty ID, got %q", actor.ID)
		}
		if actor.Type != "" {
			t.Errorf("expected empty Type, got %q", actor.Type)
		}
		if actor.OrganizationID != "" {
			t.Errorf("expected empty OrganizationID, got %q", actor.OrganizationID)
		}
	})
}

func TestWithRequestID_GetRequestID(t *testing.T) {
	t.Run("round-trip stores and retrieves request ID", func(t *testing.T) {
		id := "req-abc-123-def-456"
		ctx := WithRequestID(context.Background(), id)
		got := GetRequestID(ctx)
		if got != id {
			t.Errorf("got %q, want %q", got, id)
		}
	})

	t.Run("returns empty string when no request ID in context", func(t *testing.T) {
		got := GetRequestID(context.Background())
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("handles empty request ID", func(t *testing.T) {
		ctx := WithRequestID(context.Background(), "")
		got := GetRequestID(ctx)
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
}

func TestWithLogger_LoggerFromContext(t *testing.T) {
	t.Run("round-trip stores and retrieves logger", func(t *testing.T) {
		logger := &mockLogger{}
		ctx := WithLogger(context.Background(), logger)
		got := LoggerFromContext(ctx)
		if got == nil {
			t.Fatal("expected non-nil logger")
		}
		// Verify it is the same logger by calling a method and checking side-effects.
		got.Info("test message")
		if len(logger.messages) != 1 || logger.messages[0] != "info:test message" {
			t.Errorf("unexpected messages: %v", logger.messages)
		}
	})

	t.Run("returns nil when no logger in context", func(t *testing.T) {
		got := LoggerFromContext(context.Background())
		if got != nil {
			t.Error("expected nil logger for empty context")
		}
	})
}

func TestContextKeys_ArePrivate(t *testing.T) {
	// Verify that using a plain string key does not collide with the typed contextKey.
	// This ensures the unexported contextKey type provides collision protection.
	ctx := context.WithValue(context.Background(), "actor", "not-an-actor")
	_, ok := GetActor(ctx)
	if ok {
		t.Error("expected typed context key to prevent collision with plain string key")
	}

	ctx = context.WithValue(context.Background(), "request_id", "should-not-match")
	got := GetRequestID(ctx)
	if got != "" {
		t.Errorf("expected empty string due to key type mismatch, got %q", got)
	}

	ctx = context.WithValue(context.Background(), "logger", &mockLogger{})
	l := LoggerFromContext(ctx)
	if l != nil {
		t.Error("expected nil logger due to key type mismatch")
	}
}

func TestContextValues_DoNotInterfere(t *testing.T) {
	// Verify that setting multiple context values does not interfere with each other.
	actor := Actor{
		ID:             "user-1",
		Type:           ActorTypeUser,
		OrganizationID: "org-1",
		Source:         "test",
	}
	logger := &mockLogger{}
	reqID := "req-xyz"

	ctx := context.Background()
	ctx = WithActor(ctx, actor)
	ctx = WithRequestID(ctx, reqID)
	ctx = WithLogger(ctx, logger)

	// All three values should be independently retrievable.
	gotActor, ok := GetActor(ctx)
	if !ok {
		t.Fatal("expected actor to be present")
	}
	if gotActor.ID != "user-1" {
		t.Errorf("actor ID: got %q, want %q", gotActor.ID, "user-1")
	}

	gotReqID := GetRequestID(ctx)
	if gotReqID != reqID {
		t.Errorf("request ID: got %q, want %q", gotReqID, reqID)
	}

	gotLogger := LoggerFromContext(ctx)
	if gotLogger == nil {
		t.Fatal("expected logger to be present")
	}
}

func TestIsTestKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "test key", key: "sk_test_abc123", want: true},
		{name: "live key", key: "sk_live_abc123", want: false},
		{name: "empty string", key: "", want: false},
		{name: "partial prefix", key: "sk_test", want: false},
		{name: "exact prefix only", key: "sk_test_", want: true},
		{name: "session token", key: "sess_abc123", want: false},
		{name: "random string", key: "not_a_key", want: false},
		{name: "prefix in middle", key: "abc_sk_test_def", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsTestKey(tt.key)
			if got != tt.want {
				t.Errorf("IsTestKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestWithSessionCSRFToken_GetSessionCSRFToken(t *testing.T) {
	t.Run("round-trip stores and retrieves CSRF token", func(t *testing.T) {
		token := "csrf_abc123def456ghi789jkl012mno"
		ctx := WithSessionCSRFToken(context.Background(), token)
		got, ok := GetSessionCSRFToken(ctx)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if got != token {
			t.Errorf("got %q, want %q", got, token)
		}
	})

	t.Run("returns false when no CSRF token in context", func(t *testing.T) {
		_, ok := GetSessionCSRFToken(context.Background())
		if ok {
			t.Error("expected ok to be false for empty context")
		}
	})

	t.Run("returns false for empty string token", func(t *testing.T) {
		ctx := WithSessionCSRFToken(context.Background(), "")
		_, ok := GetSessionCSRFToken(ctx)
		if ok {
			t.Error("expected ok to be false for empty CSRF token")
		}
	})

	t.Run("does not interfere with other context values", func(t *testing.T) {
		actor := Actor{
			ID:   "user-1",
			Type: ActorTypeUser,
		}
		token := "csrf_token_xyz"

		ctx := context.Background()
		ctx = WithActor(ctx, actor)
		ctx = WithSessionCSRFToken(ctx, token)
		ctx = WithRequestID(ctx, "req-123")

		gotActor, ok := GetActor(ctx)
		if !ok || gotActor.ID != "user-1" {
			t.Errorf("actor not preserved: ok=%v, ID=%q", ok, gotActor.ID)
		}

		gotToken, ok := GetSessionCSRFToken(ctx)
		if !ok || gotToken != token {
			t.Errorf("CSRF token not preserved: ok=%v, token=%q", ok, gotToken)
		}

		gotReqID := GetRequestID(ctx)
		if gotReqID != "req-123" {
			t.Errorf("request ID not preserved: %q", gotReqID)
		}
	})
}

func TestActorType_Constants(t *testing.T) {
	// Verify the exact string values match the specification.
	if ActorTypeUser != "user" {
		t.Errorf("ActorTypeUser: got %q, want %q", ActorTypeUser, "user")
	}
	if ActorTypeAPIKey != "api_key" {
		t.Errorf("ActorTypeAPIKey: got %q, want %q", ActorTypeAPIKey, "api_key")
	}
	if ActorTypeSystem != "system" {
		t.Errorf("ActorTypeSystem: got %q, want %q", ActorTypeSystem, "system")
	}
}

func TestGetOrgID(t *testing.T) {
	t.Run("returns OrgID from actor in context", func(t *testing.T) {
		actor := Actor{
			ID:             "user_1",
			Type:           ActorTypeUser,
			OrganizationID: "org_abc123",
		}
		ctx := WithActor(context.Background(), actor)
		orgID, ok := GetOrgID(ctx)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if orgID != "org_abc123" {
			t.Errorf("got %q, want %q", orgID, "org_abc123")
		}
	})

	t.Run("returns false when no actor in context", func(t *testing.T) {
		_, ok := GetOrgID(context.Background())
		if ok {
			t.Error("expected ok to be false for empty context")
		}
	})

	t.Run("returns false when actor has empty OrgID", func(t *testing.T) {
		actor := Actor{
			ID:   "system",
			Type: ActorTypeSystem,
			// OrganizationID is empty.
		}
		ctx := WithActor(context.Background(), actor)
		_, ok := GetOrgID(ctx)
		if ok {
			t.Error("expected ok to be false for actor with empty OrgID")
		}
	})
}

func TestActor_HasScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		check  string
		want   bool
	}{
		{"has exact scope", []string{"watchpoints:read", "watchpoints:write"}, "watchpoints:write", true},
		{"missing scope", []string{"watchpoints:read"}, "watchpoints:write", false},
		{"empty scopes", []string{}, "watchpoints:read", false},
		{"nil scopes", nil, "watchpoints:read", false},
		{"single match", []string{"account:read"}, "account:read", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actor := Actor{Scopes: tc.scopes}
			got := actor.HasScope(tc.check)
			if got != tc.want {
				t.Errorf("HasScope(%q) = %v, want %v", tc.check, got, tc.want)
			}
		})
	}
}

func TestActor_RoleHasAtLeast(t *testing.T) {
	tests := []struct {
		name     string
		role     UserRole
		required UserRole
		want     bool
	}{
		{"owner >= owner", RoleOwner, RoleOwner, true},
		{"owner >= admin", RoleOwner, RoleAdmin, true},
		{"owner >= member", RoleOwner, RoleMember, true},
		{"admin >= admin", RoleAdmin, RoleAdmin, true},
		{"admin >= member", RoleAdmin, RoleMember, true},
		{"admin < owner", RoleAdmin, RoleOwner, false},
		{"member >= member", RoleMember, RoleMember, true},
		{"member < admin", RoleMember, RoleAdmin, false},
		{"member < owner", RoleMember, RoleOwner, false},
		{"empty role < member", "", RoleMember, false},
		{"empty role < admin", "", RoleAdmin, false},
		{"empty role < owner", "", RoleOwner, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actor := Actor{Role: tc.role}
			got := actor.RoleHasAtLeast(tc.required)
			if got != tc.want {
				t.Errorf("RoleHasAtLeast(%q) = %v, want %v (actor role: %q)", tc.required, got, tc.want, tc.role)
			}
		})
	}
}

func TestRoleScopeMap_Coverage(t *testing.T) {
	// Verify all roles have entries.
	for _, role := range []UserRole{RoleOwner, RoleAdmin, RoleMember} {
		scopes, ok := RoleScopeMap[role]
		if !ok {
			t.Errorf("RoleScopeMap missing entry for role %q", role)
		}
		if len(scopes) == 0 {
			t.Errorf("RoleScopeMap[%q] has empty scopes", role)
		}
	}

	// Verify Owner and Admin have account:write; Member does not.
	ownerScopes := RoleScopeMap[RoleOwner]
	hasAccountWrite := false
	for _, s := range ownerScopes {
		if s == "account:write" {
			hasAccountWrite = true
		}
	}
	if !hasAccountWrite {
		t.Error("Owner should have account:write scope")
	}

	memberScopes := RoleScopeMap[RoleMember]
	for _, s := range memberScopes {
		if s == "account:write" {
			t.Error("Member should NOT have account:write scope")
		}
	}
}

func TestActor_RoleAndScopes_RoundTrip(t *testing.T) {
	actor := Actor{
		ID:             "user_full",
		Type:           ActorTypeUser,
		OrganizationID: "org_123",
		Role:           RoleAdmin,
		Scopes:         RoleScopeMap[RoleAdmin],
		IsTestMode:     false,
		Source:         "dashboard",
	}
	ctx := WithActor(context.Background(), actor)
	got, ok := GetActor(ctx)
	if !ok {
		t.Fatal("expected actor in context")
	}
	if got.Role != RoleAdmin {
		t.Errorf("Role: got %q, want %q", got.Role, RoleAdmin)
	}
	if len(got.Scopes) != len(RoleScopeMap[RoleAdmin]) {
		t.Errorf("Scopes: got %d, want %d", len(got.Scopes), len(RoleScopeMap[RoleAdmin]))
	}
}

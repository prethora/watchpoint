package types

import (
	"context"
	"strings"
)

// ActorType identifies the kind of authenticated entity making a request.
type ActorType string

const (
	ActorTypeUser   ActorType = "user"
	ActorTypeAPIKey ActorType = "api_key"
	ActorTypeSystem ActorType = "system"
)

// Actor represents the authenticated entity performing an operation.
type Actor struct {
	ID             string
	Type           ActorType
	OrganizationID string
	IsTestMode     bool
	Source         string // Origin of the request (e.g., "wedding_app", "dashboard").
}

// Context Keys
type contextKey string

const (
	actorKey        contextKey = "actor"
	requestIDKey    contextKey = "request_id"
	loggerKey       contextKey = "logger"
	sessionCSRFKey  contextKey = "session_csrf_token"
)

// WithActor stores the Actor in the context.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// GetActor retrieves the Actor from the context.
func GetActor(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorKey).(Actor)
	return actor, ok
}

// WithRequestID stores the request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// GetRequestID retrieves the request ID from the context.
func GetRequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger stores a Logger in the context.
func WithLogger(ctx context.Context, logger Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// LoggerFromContext retrieves the Logger from the context.
// The returned logger is expected to have been pre-enriched with request-scoped
// fields (e.g., RequestID, ActorID) by middleware before storage.
// Returns nil if no logger has been set.
func LoggerFromContext(ctx context.Context) Logger {
	if l, ok := ctx.Value(loggerKey).(Logger); ok {
		return l
	}
	return nil
}

// WithSessionCSRFToken stores the session's CSRF token in the context.
// This is set by AuthMiddleware for session-based authentication so that
// CSRFMiddleware can validate the X-CSRF-Token header against it.
func WithSessionCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, sessionCSRFKey, token)
}

// GetSessionCSRFToken retrieves the session's CSRF token from the context.
// Returns the token and true if present, or empty string and false if not set.
func GetSessionCSRFToken(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(sessionCSRFKey).(string)
	return token, ok && token != ""
}

// IsTestKey returns true if the API key is a test key.
func IsTestKey(key string) bool {
	return strings.HasPrefix(key, "sk_test_")
}

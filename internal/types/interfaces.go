package types

import (
	"context"
	"time"
)

// Validator is implemented by entities to self-validate.
type Validator interface {
	Validate() error
}

// SSRFValidator checks if a webhook URL is safe to call.
type SSRFValidator func(url string) error

// RateLimitInfo contains the current state of a rate limit.
type RateLimitInfo struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// RateLimiter provides rate limiting for API requests.
type RateLimiter interface {
	// Allow checks if the actor can perform the action.
	Allow(ctx context.Context, actorID string, action string) (RateLimitInfo, bool, error)
}

// RepositoryRegistry provides access to all repository instances.
type RepositoryRegistry interface {
	WatchPoints() WatchPointRepository
	Organizations() OrganizationRepository
	Users() UserRepository
	Notifications() NotificationRepository
}

// WatchPointRepository defines the data access interface for WatchPoints.
// Stub interface - full method signatures defined in 02-foundation-db.md.
type WatchPointRepository interface{}

// OrganizationRepository defines the data access interface for Organizations.
// Stub interface - full method signatures defined in 02-foundation-db.md.
type OrganizationRepository interface{}

// UserRepository defines the data access interface for Users.
// Stub interface - full method signatures defined in 02-foundation-db.md.
type UserRepository interface{}

// NotificationRepository defines the data access interface for Notifications.
// Stub interface - full method signatures defined in 02-foundation-db.md.
type NotificationRepository interface{}

// TransactionManager provides transactional execution across repositories.
type TransactionManager interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context, repos RepositoryRegistry) error) error
}

// NotificationChannel defines the interface for a notification delivery channel.
// Implemented by email and webhook workers (08b, 08c).
type NotificationChannel interface {
	// Type returns the channel type (e.g., "email", "webhook").
	Type() ChannelType

	// ValidateConfig checks if the channel config is valid (used in Test Mode/API).
	ValidateConfig(config map[string]any) error

	// Format transforms the generic Notification into a channel-specific payload.
	// For Webhooks, this handles platform detection (Slack vs Generic).
	Format(ctx context.Context, n *Notification, config map[string]any) ([]byte, error)

	// Deliver executes the transmission.
	Deliver(ctx context.Context, payload []byte, destination string) (*DeliveryResult, error)

	// ShouldRetry inspects an error to determine if it is transient.
	ShouldRetry(err error) bool
}

// ForecastSource defines how we retrieve forecast data (abstracts Zarr/S3).
type ForecastSource interface {
	GetPoint(ctx context.Context, lat, lon float64, t time.Time) (*ForecastSnapshot, error)
	GetTile(ctx context.Context, tileID string, t time.Time) (map[string]*ForecastSnapshot, error)
}

// Evaluator defines the contract for checking conditions against forecasts.
type Evaluator interface {
	Evaluate(wp *WatchPoint, forecast *ForecastSnapshot) (EvaluationResult, error)
}

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
}

// RealClock implements Clock using the real system time (always UTC).
type RealClock struct{}

// Now returns the current time in UTC.
func (RealClock) Now() time.Time { return time.Now().UTC() }

// SecurityService provides unified security event tracking and IP-based blocking.
type SecurityService interface {
	// RecordAttempt logs a security event (login, api_auth, etc.) for tracking.
	RecordAttempt(ctx context.Context, eventType string, identifier string, ip string, success bool, reason string) error

	// IsIPBlocked checks if an IP address should be blocked based on recent failed attempts.
	IsIPBlocked(ctx context.Context, ip string) bool

	// IsIdentifierBlocked checks if a specific identifier (e.g., email) should be blocked.
	IsIdentifierBlocked(ctx context.Context, identifier string) bool
}

// Logger defines the structured logging interface used throughout the platform.
type Logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	With(args ...any) Logger
}

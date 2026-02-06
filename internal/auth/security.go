// Package auth implements authentication, session management, and security
// services for the WatchPoint platform.
package auth

import (
	"context"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// SecurityConfig holds the tunable thresholds for brute force protection.
type SecurityConfig struct {
	// IPBlockThreshold is the number of failed attempts from an IP within
	// the window before the IP is blocked. Default: 100 (per SEC-002).
	IPBlockThreshold int

	// IdentifierBlockThreshold is the number of failed login attempts for
	// a specific identifier within the window before the identifier is
	// blocked. Default: 5 (per SEC-001).
	IdentifierBlockThreshold int

	// WindowDuration is the time window for counting recent failures.
	// Default: 15 minutes.
	WindowDuration time.Duration
}

// DefaultSecurityConfig returns the default security configuration matching
// the architecture specification thresholds.
func DefaultSecurityConfig() SecurityConfig {
	return SecurityConfig{
		IPBlockThreshold:         100,
		IdentifierBlockThreshold: 5,
		WindowDuration:           15 * time.Minute,
	}
}

// SecurityRepo defines the data access methods needed by the SecurityService.
// This interface matches the SecurityRepository defined in 02-foundation-db.md.
type SecurityRepo interface {
	LogAttempt(ctx context.Context, event *types.SecurityEvent) error
	CountRecentFailuresByIP(ctx context.Context, ip string, since time.Time) (int, error)
	CountRecentFailuresByIdentifier(ctx context.Context, identifier string, since time.Time) (int, error)
}

// securityService implements types.SecurityService using the SecurityRepository
// for persistence and configurable thresholds for blocking decisions.
type securityService struct {
	repo   SecurityRepo
	config SecurityConfig
	clock  types.Clock
	logger *slog.Logger
}

// NewSecurityService creates a new SecurityService implementation.
// The service uses the provided SecurityRepo for database access and the
// SecurityConfig for threshold values.
func NewSecurityService(repo SecurityRepo, config SecurityConfig, clock types.Clock, logger *slog.Logger) types.SecurityService {
	if clock == nil {
		clock = types.RealClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &securityService{
		repo:   repo,
		config: config,
		clock:  clock,
		logger: logger,
	}
}

// RecordAttempt logs a security event for tracking. Called by auth handlers
// on both successful and failed authentication attempts.
func (s *securityService) RecordAttempt(ctx context.Context, eventType string, identifier string, ip string, success bool, reason string) error {
	event := &types.SecurityEvent{
		EventType:     eventType,
		Identifier:    identifier,
		IPAddress:     ip,
		AttemptedAt:   s.clock.Now(),
		Success:       success,
		FailureReason: reason,
	}

	if err := s.repo.LogAttempt(ctx, event); err != nil {
		s.logger.Error("failed to record security attempt",
			"event_type", eventType,
			"identifier", identifier,
			"ip", ip,
			"error", err,
		)
		return err
	}
	return nil
}

// IsIPBlocked checks if an IP address should be blocked based on recent
// failed attempts across all event types. Returns true if the IP has exceeded
// the configured threshold (default: 100 failures in 15 minutes).
func (s *securityService) IsIPBlocked(ctx context.Context, ip string) bool {
	since := s.clock.Now().Add(-s.config.WindowDuration)
	count, err := s.repo.CountRecentFailuresByIP(ctx, ip, since)
	if err != nil {
		// On error, log and fail open to avoid blocking legitimate users
		// due to a database issue. This is a deliberate choice: security
		// should not cause availability failures.
		s.logger.Error("failed to check IP block status",
			"ip", ip,
			"error", err,
		)
		return false
	}
	return count >= s.config.IPBlockThreshold
}

// IsIdentifierBlocked checks if a specific identifier (e.g., email) should
// be blocked based on recent failed attempts. Returns true if the identifier
// has exceeded the configured threshold (default: 5 failures in 15 minutes).
func (s *securityService) IsIdentifierBlocked(ctx context.Context, identifier string) bool {
	since := s.clock.Now().Add(-s.config.WindowDuration)
	count, err := s.repo.CountRecentFailuresByIdentifier(ctx, identifier, since)
	if err != nil {
		// Fail open on error - same rationale as IsIPBlocked.
		s.logger.Error("failed to check identifier block status",
			"identifier", identifier,
			"error", err,
		)
		return false
	}
	return count >= s.config.IdentifierBlockThreshold
}

// BruteForceProtector encapsulates lockout logic as defined in
// 05f-api-auth.md Section 4.2. It composes the SecurityService to provide
// a higher-level API for auth handlers.
type BruteForceProtector struct {
	security types.SecurityService
}

// NewBruteForceProtector creates a new BruteForceProtector wrapping the
// given SecurityService.
func NewBruteForceProtector(security types.SecurityService) *BruteForceProtector {
	return &BruteForceProtector{security: security}
}

// CheckLoginAllowed verifies that both the email and IP are not currently
// blocked by brute force protection. Returns true if login is allowed.
// Per SEC-001: checks identifier-level blocking (email) and IP-level
// blocking independently.
func (b *BruteForceProtector) CheckLoginAllowed(ctx context.Context, email, ip string) (bool, error) {
	if b.security.IsIdentifierBlocked(ctx, email) {
		return false, nil
	}
	if b.security.IsIPBlocked(ctx, ip) {
		return false, nil
	}
	return true, nil
}

// RecordAttempt delegates to the underlying SecurityService to record a
// login attempt for brute force tracking.
func (b *BruteForceProtector) RecordAttempt(ctx context.Context, email, ip string, success bool) error {
	reason := ""
	if !success {
		reason = "invalid_creds"
	}
	return b.security.RecordAttempt(ctx, "login", email, ip, success, reason)
}

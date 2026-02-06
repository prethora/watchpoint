package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// SessionConfig holds configuration for session management.
type SessionConfig struct {
	// SessionDuration is the lifetime of a new session.
	// Default: 7 days (per architecture spec).
	SessionDuration time.Duration

	// SessionIDPrefix is the prefix for session IDs ("sess_").
	SessionIDPrefix string
}

// DefaultSessionConfig returns the default session configuration.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		SessionDuration: 7 * 24 * time.Hour, // 7 days
		SessionIDPrefix: "sess_",
	}
}

// SessionRepo defines the data access methods needed by the SessionService.
type SessionRepo interface {
	Create(ctx context.Context, session *types.Session) error
	GetByID(ctx context.Context, sessionID string) (*types.Session, error)
	DeleteByID(ctx context.Context, sessionID string) error
	DeleteByUser(ctx context.Context, userID string) error
	DeleteExpiredByUser(ctx context.Context, userID string) error
}

// TokenGenerator abstracts entropy sources for testability.
// Defined in 05f-api-auth.md Section 4.2.
type TokenGenerator interface {
	GenerateSessionID() (string, error)
	GenerateCSRF() (string, error)
	GenerateSecureToken() (string, error) // For Resets/Invites
	GenerateOAuthState() (string, error)  // Signed state token
}

// sessionService implements the SessionService interface defined in
// 05f-api-auth.md Section 4.2.
type sessionService struct {
	repo      SessionRepo
	tokenGen  TokenGenerator
	config    SessionConfig
	clock     types.Clock
	logger    *slog.Logger
}

// NewSessionService creates a new SessionService implementation.
func NewSessionService(
	repo SessionRepo,
	tokenGen TokenGenerator,
	config SessionConfig,
	clock types.Clock,
	logger *slog.Logger,
) *sessionService {
	if clock == nil {
		clock = types.RealClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionService{
		repo:     repo,
		tokenGen: tokenGen,
		config:   config,
		clock:    clock,
		logger:   logger,
	}
}

// CreateSession creates a new session for the given user and returns the
// Session object and the raw session ID (for cookie setting).
// Per DASH-001: generates SessionID and CSRFToken, inserts into DB.
func (s *sessionService) CreateSession(ctx context.Context, userID, orgID, ip, userAgent string) (*types.Session, string, error) {
	sessionID, err := s.tokenGen.GenerateSessionID()
	if err != nil {
		return nil, "", types.NewAppError(types.ErrCodeInternalUnexpected, "failed to generate session ID", err)
	}

	csrfToken, err := s.tokenGen.GenerateCSRF()
	if err != nil {
		return nil, "", types.NewAppError(types.ErrCodeInternalUnexpected, "failed to generate CSRF token", err)
	}

	now := s.clock.Now()
	session := &types.Session{
		ID:             sessionID,
		UserID:         userID,
		OrganizationID: orgID,
		CSRFToken:      csrfToken,
		IPAddress:      ip,
		UserAgent:      userAgent,
		ExpiresAt:      now.Add(s.config.SessionDuration),
		LastActivityAt: now,
		CreatedAt:      now,
	}

	if err := s.repo.Create(ctx, session); err != nil {
		return nil, "", err
	}

	s.logger.Info("session created",
		"session_id", sessionID,
		"user_id", userID,
		"org_id", orgID,
	)

	return session, sessionID, nil
}

// ValidateSession validates a session ID against the database.
// Returns the Session if valid, or an error if not found or expired.
func (s *sessionService) ValidateSession(ctx context.Context, sessionID string) (*types.Session, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Check expiry
	if s.clock.Now().After(session.ExpiresAt) {
		// Session exists but has expired. Clean it up asynchronously
		// (best effort - the scheduled cleanup will catch it otherwise).
		s.logger.Info("session expired",
			"session_id", sessionID,
			"expired_at", session.ExpiresAt,
		)
		return nil, types.NewAppError(types.ErrCodeAuthSessionExpired, "session has expired", nil)
	}

	return session, nil
}

// ValidateCSRF checks that the provided CSRF token matches the session's
// token using constant-time comparison to prevent timing attacks.
func (s *sessionService) ValidateCSRF(session *types.Session, token string) error {
	if session == nil {
		return types.NewAppError(types.ErrCodeAuthSessionExpired, "no session provided", nil)
	}
	if subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) != 1 {
		return types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid CSRF token", nil)
	}
	return nil
}

// InvalidateSession performs a hard delete of a single session.
// Per Section 7.2: "DELETE FROM sessions WHERE id = $1" to ensure
// immediate invalidation on logout.
func (s *sessionService) InvalidateSession(ctx context.Context, sessionID string) error {
	if err := s.repo.DeleteByID(ctx, sessionID); err != nil {
		return err
	}
	s.logger.Info("session invalidated", "session_id", sessionID)
	return nil
}

// InvalidateAllUserSessions removes all sessions for a user.
// Per Section 7.2: Used during CompletePasswordReset to revoke
// all access immediately.
func (s *sessionService) InvalidateAllUserSessions(ctx context.Context, userID string) error {
	if err := s.repo.DeleteByUser(ctx, userID); err != nil {
		return err
	}
	s.logger.Info("all sessions invalidated for user", "user_id", userID)
	return nil
}

// CleanExpiredSessions removes expired sessions for a user.
// This is the lazy cleanup called during login transactions (DASH-001).
func (s *sessionService) CleanExpiredSessions(ctx context.Context, userID string) error {
	return s.repo.DeleteExpiredByUser(ctx, userID)
}

// withRepo returns a copy of the sessionService that uses the given
// SessionRepo for database operations. This enables the AuthService to
// create sessions within a transaction by providing a transaction-scoped
// session repository while reusing the same token generator and config.
func (s *sessionService) withRepo(repo SessionRepo) *sessionService {
	return &sessionService{
		repo:     repo,
		tokenGen: s.tokenGen,
		config:   s.config,
		clock:    s.clock,
		logger:   s.logger,
	}
}

// CryptoTokenGenerator is the production implementation of TokenGenerator
// using crypto/rand for secure random generation.
type CryptoTokenGenerator struct {
	// SessionIDPrefix is prepended to generated session IDs.
	SessionIDPrefix string
}

// NewCryptoTokenGenerator creates a new CryptoTokenGenerator with the
// standard "sess_" prefix.
func NewCryptoTokenGenerator() *CryptoTokenGenerator {
	return &CryptoTokenGenerator{
		SessionIDPrefix: "sess_",
	}
}

// GenerateSessionID generates a cryptographically secure session ID.
// Format: "sess_" + 32 random hex bytes (64 hex chars).
func (g *CryptoTokenGenerator) GenerateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return g.SessionIDPrefix + hex.EncodeToString(b), nil
}

// GenerateCSRF generates a cryptographically secure CSRF token.
// Format: 32 random hex bytes (64 hex chars).
func (g *CryptoTokenGenerator) GenerateCSRF() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate CSRF token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateSecureToken generates a cryptographically secure token for
// password resets and invite links.
// Format: 32 random hex bytes (64 hex chars).
func (g *CryptoTokenGenerator) GenerateSecureToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secure token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateOAuthState generates a cryptographically secure state parameter
// for OAuth CSRF protection.
// Format: 32 random hex bytes (64 hex chars).
func (g *CryptoTokenGenerator) GenerateOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CanonicalizeEmail normalizes email addresses for consistent DB lookups.
// Per Section 7.3: strings.ToLower(strings.TrimSpace(email))
func CanonicalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

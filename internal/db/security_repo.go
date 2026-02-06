package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"watchpoint/internal/types"
)

// SecurityRepository provides data access for the security_events table.
// It implements the SecurityRepository interface defined in 02-foundation-db.md
// Section 9, operating on the unified security_events table (Section 5.2).
type SecurityRepository struct {
	db DBTX
}

// NewSecurityRepository creates a new SecurityRepository backed by the given
// database connection (pool or transaction).
func NewSecurityRepository(db DBTX) *SecurityRepository {
	return &SecurityRepository{db: db}
}

// LogAttempt records a security event (login attempt, API auth attempt, etc.)
// into the security_events table. This is the write path for all security
// event tracking.
func (r *SecurityRepository) LogAttempt(ctx context.Context, event *types.SecurityEvent) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO security_events (event_type, identifier, ip_address, attempted_at, success, failure_reason)
		 VALUES ($1, $2, $3, COALESCE($4, NOW()), $5, $6)`,
		event.EventType,
		nilIfEmpty(event.Identifier),
		event.IPAddress,
		nilIfZeroTime(event.AttemptedAt),
		event.Success,
		nilIfEmpty(event.FailureReason),
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to log security event", err)
	}
	return nil
}

// CountRecentFailuresByIP returns the count of failed attempts from an IP
// address within the specified time window. Used for IP-based blocking
// (SEC-002 flow simulation).
//
// Query: SELECT count(*) FROM security_events WHERE ip_address=$1
//        AND success=false AND attempted_at > $2
func (r *SecurityRepository) CountRecentFailuresByIP(ctx context.Context, ip string, since time.Time) (int, error) {
	var count int
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_events
		 WHERE ip_address = $1 AND success = false AND attempted_at > $2`,
		ip,
		since,
	).Scan(&count)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to count IP failures", err)
	}
	return count, nil
}

// CountRecentFailuresByIdentifier returns the count of failed attempts for
// a specific identifier (e.g., email) within the specified time window.
// Used for account-level lockouts (SEC-001 flow simulation).
//
// Query: SELECT count(*) FROM security_events WHERE identifier=$1
//        AND event_type='login' AND success=false AND attempted_at > $2
func (r *SecurityRepository) CountRecentFailuresByIdentifier(ctx context.Context, identifier string, since time.Time) (int, error) {
	var count int
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_events
		 WHERE identifier = $1 AND event_type = 'login' AND success = false AND attempted_at > $2`,
		identifier,
		since,
	).Scan(&count)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to count identifier failures", err)
	}
	return count, nil
}

// nilIfEmpty returns nil if the string is empty, otherwise returns a pointer
// to the string. Used for nullable text columns.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nilIfZeroTime returns nil if the time is zero, otherwise returns a pointer
// to the time. Used to let the DB default (NOW()) apply when no time is set.
func nilIfZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// isUniqueViolation checks if the error is a PostgreSQL unique constraint
// violation (error code 23505). Used by repositories to detect duplicate
// key conflicts and return appropriate application-level errors.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

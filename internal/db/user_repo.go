package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// UserRepository provides data access for the users table.
// It implements the UserRepository interfaces defined in both
// 02-foundation-db.md Section 9.2 and 05f-api-auth.md Section 4.1.
type UserRepository struct {
	db DBTX
}

// NewUserRepository creates a new UserRepository backed by the given
// database connection (pool or transaction).
func NewUserRepository(db DBTX) *UserRepository {
	return &UserRepository{db: db}
}

// userColumns defines the standard set of columns selected for user queries.
// Used consistently across all query methods to avoid column drift.
const userColumns = `u.id, u.organization_id, u.email, u.name, u.password_hash, u.role, u.status,
	u.auth_provider, u.auth_provider_id, u.invite_token_hash, u.invite_expires_at,
	u.created_at, u.last_login_at, u.deleted_at`

// scanUser scans a single user row into a types.User struct.
// The columns must match the order defined in userColumns.
// Uses nullable scan targets for columns that may be NULL in the database
// (password_hash, name, auth_provider, auth_provider_id, invite_token_hash).
func scanUser(row pgx.Row) (*types.User, error) {
	var u types.User
	var (
		name            *string
		passwordHash    *string
		authProvider    *string
		authProviderID  *string
		inviteTokenHash *string
	)
	err := row.Scan(
		&u.ID,
		&u.OrganizationID,
		&u.Email,
		&name,
		&passwordHash,
		&u.Role,
		&u.Status,
		&authProvider,
		&authProviderID,
		&inviteTokenHash,
		&u.InviteExpiresAt,
		&u.CreatedAt,
		&u.LastLoginAt,
		&u.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	if name != nil {
		u.Name = *name
	}
	if passwordHash != nil {
		u.PasswordHash = *passwordHash
	}
	if authProvider != nil {
		u.AuthProvider = *authProvider
	}
	if authProviderID != nil {
		u.AuthProviderID = *authProviderID
	}
	if inviteTokenHash != nil {
		u.InviteTokenHash = *inviteTokenHash
	}
	return &u, nil
}

// GetByID retrieves a user by their ID scoped to an organization.
// Returns ErrNotFoundUser if no active user is found.
func (r *UserRepository) GetByID(ctx context.Context, id string, orgID string) (*types.User, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+userColumns+`
		 FROM users u
		 WHERE u.id = $1 AND u.organization_id = $2 AND u.deleted_at IS NULL`,
		id,
		orgID,
	)

	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeNotFoundUser, "user not found", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve user", err)
	}
	return u, nil
}

// GetByEmail retrieves a user by their email address.
// As specified in 05f-api-auth.md Section 4.1:
//   - Returns ErrNotFoundUser (mapped to "auth_user_not_found") if no user exists.
//   - Returns ErrOrgDeleted (mapped to "auth_organization_deleted") if the user's
//     associated organization has been soft-deleted.
//
// The query joins organizations to check for soft-deletion in a single round trip.
func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*types.User, error) {
	// Use a single query with LEFT JOIN on organizations to detect org deletion.
	// The org.deleted_at column tells us if the org was soft-deleted.
	row := r.db.QueryRow(ctx,
		`SELECT `+userColumns+`, o.deleted_at
		 FROM users u
		 LEFT JOIN organizations o ON o.id = u.organization_id
		 WHERE u.email = $1 AND u.deleted_at IS NULL`,
		email,
	)

	var u types.User
	var (
		name            *string
		passwordHash    *string
		authProvider    *string
		authProviderID  *string
		inviteTokenHash *string
		orgDeletedAt    *time.Time
	)
	err := row.Scan(
		&u.ID,
		&u.OrganizationID,
		&u.Email,
		&name,
		&passwordHash,
		&u.Role,
		&u.Status,
		&authProvider,
		&authProviderID,
		&inviteTokenHash,
		&u.InviteExpiresAt,
		&u.CreatedAt,
		&u.LastLoginAt,
		&u.DeletedAt,
		&orgDeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeAuthUserNotFound, "user not found", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve user by email", err)
	}
	if name != nil {
		u.Name = *name
	}
	if passwordHash != nil {
		u.PasswordHash = *passwordHash
	}
	if authProvider != nil {
		u.AuthProvider = *authProvider
	}
	if authProviderID != nil {
		u.AuthProviderID = *authProviderID
	}
	if inviteTokenHash != nil {
		u.InviteTokenHash = *inviteTokenHash
	}

	// Check if the associated organization is soft-deleted
	if orgDeletedAt != nil {
		return nil, types.NewAppError(types.ErrCodeAuthOrgDeleted, "organization has been deleted", nil)
	}

	return &u, nil
}

// GetByInviteTokenHash retrieves a user by their SHA-256 invite token hash.
// Used by AcceptInvite to find the invited user from the raw token.
// Returns ErrAuthTokenInvalid if no matching invited user is found.
func (r *UserRepository) GetByInviteTokenHash(ctx context.Context, tokenHash string) (*types.User, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+userColumns+`
		 FROM users u
		 WHERE u.invite_token_hash = $1 AND u.status = 'invited' AND u.deleted_at IS NULL`,
		tokenHash,
	)

	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid or expired invite token", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve user by invite token", err)
	}
	return u, nil
}

// GetOwnerEmail returns the email address of an Owner-role user for the given
// organization. Used for system-level alerts (e.g., notifying the owner of
// email channel failures).
func (r *UserRepository) GetOwnerEmail(ctx context.Context, orgID string) (string, error) {
	var email string
	err := r.db.QueryRow(ctx,
		`SELECT email FROM users
		 WHERE organization_id = $1 AND role = 'owner' AND deleted_at IS NULL
		 LIMIT 1`,
		orgID,
	).Scan(&email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", types.NewAppError(types.ErrCodeNotFoundUser, "no owner found for organization", nil)
		}
		return "", types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve owner email", err)
	}
	return email, nil
}

// UpdatePassword updates the user's password hash. As specified in
// 05f-api-auth.md Section 4.1: "Updates the hash and clears reset tokens."
// The clearing of reset tokens is handled by the caller (AuthService) within
// the same transaction; this method focuses on the users table update.
func (r *UserRepository) UpdatePassword(ctx context.Context, userID string, newHash string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2 AND deleted_at IS NULL`,
		newHash,
		userID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update password", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundUser, "user not found", nil)
	}
	return nil
}

// CreateWithProvider creates a new user via OAuth (password_hash is nil).
// As specified in 05f-api-auth.md Section 4.1.
func (r *UserRepository) CreateWithProvider(ctx context.Context, user *types.User) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO users (id, organization_id, email, name, password_hash, role, status,
		 auth_provider, auth_provider_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		user.ID,
		user.OrganizationID,
		user.Email,
		user.Name,
		nilIfEmpty(user.PasswordHash),
		user.Role,
		user.Status,
		nilIfEmpty(user.AuthProvider),
		nilIfEmpty(user.AuthProviderID),
		user.CreatedAt,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create user with provider", err)
	}
	return nil
}

// UpdateStatus updates a user's status and optionally clears invite-related
// fields. Used by AcceptInvite to transition from 'invited' to 'active'.
// Also sets password_hash and name for the accept-invite flow.
func (r *UserRepository) UpdateStatus(ctx context.Context, userID string, status types.UserStatus, name string, passwordHash string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET status = $1, name = $2, password_hash = $3,
		 invite_token_hash = NULL, invite_expires_at = NULL
		 WHERE id = $4 AND deleted_at IS NULL`,
		status,
		name,
		passwordHash,
		userID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update user status", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundUser, "user not found", nil)
	}
	return nil
}

// UpdateLastLogin updates the last_login_at timestamp for a user.
// Called during the login transaction (USER-006 flow simulation step 7).
func (r *UserRepository) UpdateLastLogin(ctx context.Context, userID string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET last_login_at = NOW() WHERE id = $1 AND deleted_at IS NULL`,
		userID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update last login", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundUser, "user not found", nil)
	}
	return nil
}

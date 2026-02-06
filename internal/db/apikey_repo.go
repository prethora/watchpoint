package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// APIKeyRepository provides data access for the api_keys table.
// It implements the APIKeyRepository interface defined in 05d-api-organization.md
// Section 2.2. API keys use bcrypt hashing; plaintext secrets are never stored.
type APIKeyRepository struct {
	db DBTX
}

// NewAPIKeyRepository creates a new APIKeyRepository backed by the given
// database connection (pool or transaction).
func NewAPIKeyRepository(db DBTX) *APIKeyRepository {
	return &APIKeyRepository{db: db}
}

// apiKeyColumns defines the standard set of columns selected for API key queries.
// key_hash is intentionally included for internal operations but MUST NOT be
// exposed in API responses.
const apiKeyColumns = `id, organization_id, created_by_user_id, key_hash,
	key_prefix, scopes, test_mode, source, name, last_used_at, expires_at,
	revoked_at, created_at`

// ListAPIKeysParams defines filtering options for listing API keys.
// Defined here to match the architecture spec in 05d-api-organization.md Section 5.2.
type ListAPIKeysParams struct {
	ActiveOnly bool
	Prefix     string
	Limit      int
	Cursor     string
}

// List retrieves API keys for an organization with optional filtering.
// Supports prefix filtering for compromise recovery (finding keys by leaked prefix).
// When Prefix is non-empty, filters by key_prefix LIKE $prefix%.
// Per 05d-api-organization.md Section 2.2.
func (r *APIKeyRepository) List(ctx context.Context, orgID string, params ListAPIKeysParams) ([]*types.APIKey, error) {
	var conditions []string
	var args []any
	argIdx := 1

	// Organization ID is always required.
	conditions = append(conditions, fmt.Sprintf("organization_id = $%d", argIdx))
	args = append(args, orgID)
	argIdx++

	// ActiveOnly filter: exclude revoked and expired keys.
	if params.ActiveOnly {
		conditions = append(conditions, "revoked_at IS NULL")
		conditions = append(conditions, fmt.Sprintf("(expires_at IS NULL OR expires_at > $%d)", argIdx))
		args = append(args, time.Now().UTC())
		argIdx++
	}

	// Prefix filter for compromise recovery.
	if params.Prefix != "" {
		conditions = append(conditions, fmt.Sprintf("key_prefix LIKE $%d", argIdx))
		args = append(args, params.Prefix+"%")
		argIdx++
	}

	// Cursor-based pagination using created_at.
	if params.Cursor != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, params.Cursor)
		if err != nil {
			return nil, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"invalid cursor format; expected RFC3339 timestamp",
				err,
			)
		}
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", argIdx))
		args = append(args, cursorTime)
		argIdx++
	}

	whereClause := "WHERE " + strings.Join(conditions, " AND ")

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}

	query := fmt.Sprintf(
		`SELECT %s FROM api_keys %s ORDER BY created_at DESC LIMIT $%d`,
		apiKeyColumns,
		whereClause,
		argIdx,
	)
	args = append(args, limit+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to query API keys", err)
	}
	defer rows.Close()

	var results []*types.APIKey
	for rows.Next() {
		key, scanErr := scanAPIKey(rows)
		if scanErr != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan API key row", scanErr)
		}
		results = append(results, key)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating API key rows", err)
	}

	// Return all results including the potential extra row (limit+1).
	// The handler is responsible for detecting pagination (if len > limit,
	// HasMore=true) and trimming the extra row before building the response.
	return results, nil
}

// Create inserts a new API key record. The key_hash MUST be the bcrypt hash
// of the plaintext secret; the plaintext MUST NOT be passed to this method.
// Per USER-011 flow simulation step 5.
func (r *APIKeyRepository) Create(ctx context.Context, key *types.APIKey) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO api_keys (id, organization_id, created_by_user_id, key_hash,
		 key_prefix, scopes, test_mode, source, name, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, NOW()))`,
		key.ID,
		key.OrganizationID,
		key.CreatedByUserID,
		key.KeyHash,
		key.KeyPrefix,
		key.Scopes,
		key.TestMode,
		nilIfEmptyString(key.Source),
		key.Name,
		key.ExpiresAt,
		nilIfZeroTime(key.CreatedAt),
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create API key", err)
	}
	return nil
}

// GetByID retrieves an API key by ID and organization, verifying ownership.
// Returns ErrCodeNotFoundAPIKey if the key does not exist or belongs to
// a different organization.
func (r *APIKeyRepository) GetByID(ctx context.Context, id string, orgID string) (*types.APIKey, error) {
	row := r.db.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM api_keys WHERE id = $1 AND organization_id = $2`, apiKeyColumns),
		id,
		orgID,
	)

	key, err := scanAPIKeyRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeNotFoundAPIKey, "API key not found", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve API key", err)
	}
	return key, nil
}

// Delete performs a soft revocation of an API key by setting revoked_at.
// Per USER-013 flow simulation: UPDATE api_keys SET revoked_at = NOW()
// WHERE id = $1 AND organization_id = $2.
func (r *APIKeyRepository) Delete(ctx context.Context, id string, orgID string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND organization_id = $2 AND revoked_at IS NULL`,
		id,
		orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to revoke API key", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundAPIKey, "API key not found or already revoked", nil)
	}
	return nil
}

// Rotate implements dual-validity key rotation. Within a single transaction-like
// execution it:
//  1. Updates the old key's expires_at to the grace end time.
//  2. Inserts a new key record with the new hash.
//
// Per USER-012 flow simulation: the old key remains valid until graceEnd,
// allowing clients to transition to the new key without downtime.
//
// Note: The newKey parameter must have all fields populated EXCEPT that the
// handler is responsible for generating the ID, hash, prefix, etc.
func (r *APIKeyRepository) Rotate(ctx context.Context, oldKeyID string, orgID string, newKey *types.APIKey, graceEnd time.Time) error {
	// Step 1: Set expiry on the old key.
	tag, err := r.db.Exec(ctx,
		`UPDATE api_keys SET expires_at = $1
		 WHERE id = $2 AND organization_id = $3 AND revoked_at IS NULL`,
		graceEnd,
		oldKeyID,
		orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update old key expiry during rotation", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundAPIKey, "API key not found or already revoked", nil)
	}

	// Step 2: Insert the new key.
	if err := r.Create(ctx, newKey); err != nil {
		return err
	}

	return nil
}

// RevokeByUser bulk revokes all API keys created by a specific user.
// Used when a user is deleted to revoke their keys.
// Per 05d-api-organization.md Section 2.2.
func (r *APIKeyRepository) RevokeByUser(ctx context.Context, userID string, orgID string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE api_keys SET revoked_at = NOW()
		 WHERE created_by_user_id = $1 AND organization_id = $2 AND revoked_at IS NULL`,
		userID,
		orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to revoke API keys by user", err)
	}
	return nil
}

// TouchLastUsed updates the last_used_at timestamp for an API key.
// This is a fire-and-forget optimization; errors are logged but not propagated.
// Per 05d-api-organization.md Section 2.2.
func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`,
		id,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update API key last_used_at", err)
	}
	return nil
}

// CountRecentByUser returns the count of API keys created by a user within
// the given time window. Used for proactive rate limiting to prevent rapid
// key cycling during compromise scenarios.
// Per 05d-api-organization.md Section 5.4 item 3.
func (r *APIKeyRepository) CountRecentByUser(ctx context.Context, userID string, since time.Time) (int, error) {
	var count int
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE created_by_user_id = $1 AND created_at >= $2`,
		userID,
		since,
	).Scan(&count)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to count recent API keys", err)
	}
	return count, nil
}

// scanAPIKey scans an API key from pgx.Rows. Column order must match apiKeyColumns.
func scanAPIKey(rows pgx.Rows) (*types.APIKey, error) {
	var key types.APIKey
	err := rows.Scan(
		&key.ID,
		&key.OrganizationID,
		&key.CreatedByUserID,
		&key.KeyHash,
		&key.KeyPrefix,
		&key.Scopes,
		&key.TestMode,
		&key.Source,
		&key.Name,
		&key.LastUsedAt,
		&key.ExpiresAt,
		&key.RevokedAt,
		&key.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// scanAPIKeyRow scans an API key from a single pgx.Row (for QueryRow).
// Column order must match apiKeyColumns.
func scanAPIKeyRow(row pgx.Row) (*types.APIKey, error) {
	var key types.APIKey
	err := row.Scan(
		&key.ID,
		&key.OrganizationID,
		&key.CreatedByUserID,
		&key.KeyHash,
		&key.KeyPrefix,
		&key.Scopes,
		&key.TestMode,
		&key.Source,
		&key.Name,
		&key.LastUsedAt,
		&key.ExpiresAt,
		&key.RevokedAt,
		&key.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// nilIfEmptyString returns nil if the string is empty, otherwise returns a
// pointer to the string. Used for nullable VARCHAR columns.
func nilIfEmptyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

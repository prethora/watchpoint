package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// OrganizationRepository provides data access for the organizations table.
// It implements the OrganizationRepository interface defined in
// 02-foundation-db.md Section 9.2 and extended in 05d-api-organization.md Section 2.2.
type OrganizationRepository struct {
	db DBTX
}

// NewOrganizationRepository creates a new OrganizationRepository backed by the
// given database connection (pool or transaction).
func NewOrganizationRepository(db DBTX) *OrganizationRepository {
	return &OrganizationRepository{db: db}
}

// orgColumns defines the standard set of columns selected for organization queries.
// Used consistently across all query methods to avoid column drift.
const orgColumns = `o.id, o.name, o.billing_email, o.plan, o.plan_limits,
	o.stripe_customer_id, o.notification_preferences,
	o.created_at, o.updated_at, o.deleted_at`

// scanOrg scans a single organization row into a types.Organization struct.
// The columns must match the order defined in orgColumns.
func scanOrg(row pgx.Row) (*types.Organization, error) {
	var org types.Organization
	var stripeCustomerID *string

	err := row.Scan(
		&org.ID,
		&org.Name,
		&org.BillingEmail,
		&org.Plan,
		&org.PlanLimits,
		&stripeCustomerID,
		&org.NotificationPreferences,
		&org.CreatedAt,
		&org.UpdatedAt,
		&org.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	if stripeCustomerID != nil {
		org.StripeCustomerID = *stripeCustomerID
	}
	return &org, nil
}

// Create inserts a new organization record. The caller must set the ID
// (prefixed UUID, e.g. "org_...") and required fields before calling.
// Used during the signup flow (USER-001).
func (r *OrganizationRepository) Create(ctx context.Context, org *types.Organization) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO organizations (id, name, billing_email, plan, plan_limits,
		 stripe_customer_id, notification_preferences, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, NOW()), COALESCE($9, NOW()))`,
		org.ID,
		org.Name,
		org.BillingEmail,
		org.Plan,
		org.PlanLimits,
		nilIfEmpty(org.StripeCustomerID),
		org.NotificationPreferences,
		nilIfZeroTime(org.CreatedAt),
		nilIfZeroTime(org.UpdatedAt),
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create organization", err)
	}
	return nil
}

// GetByID retrieves an organization by its ID. Excludes soft-deleted organizations.
// Returns ErrNotFoundOrg if no active organization is found.
func (r *OrganizationRepository) GetByID(ctx context.Context, id string) (*types.Organization, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+orgColumns+`
		 FROM organizations o
		 WHERE o.id = $1 AND o.deleted_at IS NULL`,
		id,
	)

	org, err := scanOrg(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve organization", err)
	}
	return org, nil
}

// Update applies partial changes to an organization record. The caller passes
// the full Organization struct; only mutable fields (name, billing_email,
// notification_preferences) are written. The updated_at timestamp is set by
// the database.
//
// Used by PATCH /v1/organization (USER-002 flow).
func (r *OrganizationRepository) Update(ctx context.Context, org *types.Organization) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE organizations
		 SET name = $1,
		     billing_email = $2,
		     notification_preferences = $3,
		     updated_at = NOW()
		 WHERE id = $4 AND deleted_at IS NULL`,
		org.Name,
		org.BillingEmail,
		org.NotificationPreferences,
		org.ID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update organization", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
	}
	return nil
}

// Delete performs a soft delete by setting deleted_at = NOW().
// This is part of the Organization Deletion flow (USER-003).
// The caller must cancel the billing subscription and pause all WatchPoints
// before calling Delete.
func (r *OrganizationRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE organizations SET deleted_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to delete organization", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found or already deleted", nil)
	}
	return nil
}

// UpdatePlan updates the organization's plan tier and corresponding plan limits.
// Used by the billing integration to apply plan changes from Stripe webhooks.
func (r *OrganizationRepository) UpdatePlan(ctx context.Context, id string, plan types.PlanTier, limits types.PlanLimits) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE organizations
		 SET plan = $1,
		     plan_limits = $2,
		     updated_at = NOW()
		 WHERE id = $3 AND deleted_at IS NULL`,
		plan,
		limits,
		id,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update organization plan", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
	}
	return nil
}

// IncrementRateLimit atomically increments the API call counter for the current
// billing period and returns the new count. Used by middleware for rate limiting.
// The UPSERT ensures the rate_limits row exists for the current period.
func (r *OrganizationRepository) IncrementRateLimit(ctx context.Context, id string) (int, error) {
	var newCount int
	err := r.db.QueryRow(ctx,
		`INSERT INTO rate_limits (organization_id, api_calls_count, period_start, period_end)
		 VALUES ($1, 1, NOW(), date_trunc('day', NOW()) + INTERVAL '1 day')
		 ON CONFLICT (organization_id, source)
		 DO UPDATE SET api_calls_count = rate_limits.api_calls_count + 1
		 RETURNING api_calls_count`,
		id,
	).Scan(&newCount)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to increment rate limit", err)
	}
	return newCount, nil
}

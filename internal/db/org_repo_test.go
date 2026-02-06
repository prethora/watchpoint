package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// Note: mockDBTX, mockRow, and mockRows are defined in session_repo_test.go
// and usage_repo_test.go and reused here.

// ============================================================
// Create Tests
// ============================================================

func TestOrganizationRepository_Create_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	org := &types.Organization{
		ID:           "org_123",
		Name:         "Acme Corp",
		BillingEmail: "billing@acme.com",
		Plan:         types.PlanFree,
		PlanLimits: types.PlanLimits{
			MaxWatchPoints:   10,
			MaxAPICallsDaily: 1000,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, org)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestOrganizationRepository_Create_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	org := &types.Organization{
		ID:           "org_dup",
		Name:         "Duplicate Corp",
		BillingEmail: "dup@acme.com",
		Plan:         types.PlanFree,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("unique_violation"))

	err := repo.Create(ctx, org)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// GetByID Tests
// ============================================================

func TestOrganizationRepository_GetByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "org_123"                                          // id
			*dest[1].(*string) = "Acme Corp"                                        // name
			*dest[2].(*string) = "billing@acme.com"                                 // billing_email
			*dest[3].(*types.PlanTier) = types.PlanFree                             // plan
			dest[4].(*types.PlanLimits).Scan([]byte(`{"watchpoints_max":10}`))      // plan_limits
			s := "cus_stripe123"                                                    // stripe_customer_id
			*dest[5].(**string) = &s
			dest[6].(*types.NotificationPreferences).Scan([]byte(`{}`))             // notification_preferences
			*dest[7].(*time.Time) = now                                             // created_at
			*dest[8].(*time.Time) = now                                             // updated_at
			*dest[9].(**time.Time) = nil                                            // deleted_at
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_123"}).Return(row)

	org, err := repo.GetByID(ctx, "org_123")
	require.NoError(t, err)
	assert.Equal(t, "org_123", org.ID)
	assert.Equal(t, "Acme Corp", org.Name)
	assert.Equal(t, "billing@acme.com", org.BillingEmail)
	assert.Equal(t, types.PlanFree, org.Plan)
	assert.Equal(t, "cus_stripe123", org.StripeCustomerID)
	assert.Nil(t, org.DeletedAt)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_GetByID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_missing"}).Return(row)

	_, err := repo.GetByID(ctx, "org_missing")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_GetByID_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: errors.New("connection refused")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_123"}).Return(row)

	_, err := repo.GetByID(ctx, "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Update Tests
// ============================================================

func TestOrganizationRepository_Update_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	org := &types.Organization{
		ID:           "org_123",
		Name:         "Acme Corp Updated",
		BillingEmail: "new-billing@acme.com",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Update(ctx, org)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestOrganizationRepository_Update_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	org := &types.Organization{
		ID:           "org_missing",
		Name:         "Missing Corp",
		BillingEmail: "missing@example.com",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Update(ctx, org)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_Update_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	org := &types.Organization{
		ID:           "org_123",
		Name:         "Acme",
		BillingEmail: "billing@acme.com",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Update(ctx, org)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Delete Tests (Soft Delete)
// ============================================================

func TestOrganizationRepository_Delete_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"org_123"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Delete(ctx, "org_123")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestOrganizationRepository_Delete_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"org_missing"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Delete(ctx, "org_missing")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_Delete_AlreadyDeleted(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	// If already soft-deleted, WHERE deleted_at IS NULL won't match
	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"org_deleted"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Delete(ctx, "org_deleted")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_Delete_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"org_123"}).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Delete(ctx, "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdatePlan Tests
// ============================================================

func TestOrganizationRepository_UpdatePlan_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	limits := types.PlanLimits{
		MaxWatchPoints:   100,
		MaxAPICallsDaily: 10000,
		AllowNowcast:     true,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdatePlan(ctx, "org_123", types.PlanPro, limits)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestOrganizationRepository_UpdatePlan_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	limits := types.PlanLimits{}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdatePlan(ctx, "org_missing", types.PlanPro, limits)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// IncrementRateLimit Tests
// ============================================================

func TestOrganizationRepository_IncrementRateLimit_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 42
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_123"}).Return(row)

	count, err := repo.IncrementRateLimit(ctx, "org_123")
	require.NoError(t, err)
	assert.Equal(t, 42, count)

	db.AssertExpectations(t)
}

func TestOrganizationRepository_IncrementRateLimit_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewOrganizationRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: errors.New("db error")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_123"}).Return(row)

	_, err := repo.IncrementRateLimit(ctx, "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

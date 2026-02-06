package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// --- SubscriptionStateRepo Tests ---

func TestSubscriptionStateRepo_UpdateSubscriptionStatus_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	// Mock zombie check: org is not deleted
	db.On("QueryRow", mock.Anything,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		mock.Anything,
	).Return(&mockRow{
		scanFn: func(dest ...any) error {
			// Set deleted_at to nil (not deleted)
			p := dest[0].(**time.Time)
			*p = nil
			return nil
		},
	})

	// Mock the UPDATE
	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	eventTime := time.Now().UTC()
	err := repo.UpdateSubscriptionStatus(
		context.Background(),
		"org_1",
		types.PlanPro,
		types.SubStatusActive,
		eventTime,
	)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSubscriptionStateRepo_UpdateSubscriptionStatus_ZombieBilling(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	deletedTime := time.Now().Add(-24 * time.Hour)

	// Mock zombie check: org IS deleted
	db.On("QueryRow", mock.Anything,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		mock.Anything,
	).Return(&mockRow{
		scanFn: func(dest ...any) error {
			p := dest[0].(**time.Time)
			*p = &deletedTime
			return nil
		},
	})

	eventTime := time.Now().UTC()
	err := repo.UpdateSubscriptionStatus(
		context.Background(),
		"org_1",
		types.PlanPro,
		types.SubStatusActive,
		eventTime,
	)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeConflictConcurrent, appErr.Code)
	assert.Contains(t, appErr.Message, "ZC_BILLING_ALERT")
}

func TestSubscriptionStateRepo_UpdateSubscriptionStatus_StaleEvent(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	// Mock zombie check: org not deleted
	db.On("QueryRow", mock.Anything,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		mock.Anything,
	).Return(&mockRow{
		scanFn: func(dest ...any) error {
			p := dest[0].(**time.Time)
			*p = nil
			return nil
		},
	})

	// Mock the UPDATE returning 0 rows (stale event - optimistic lock rejected)
	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	staleEventTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	err := repo.UpdateSubscriptionStatus(
		context.Background(),
		"org_1",
		types.PlanStarter,
		types.SubStatusActive,
		staleEventTime,
	)
	// Stale events are silently ignored (idempotent no-op)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSubscriptionStateRepo_UpdateSubscriptionStatus_DBErrorOnZombieCheck(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	// Mock zombie check fails
	db.On("QueryRow", mock.Anything,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		mock.Anything,
	).Return(&mockRow{
		scanErr: errors.New("connection refused"),
	})

	err := repo.UpdateSubscriptionStatus(
		context.Background(),
		"org_1",
		types.PlanPro,
		types.SubStatusActive,
		time.Now(),
	)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestSubscriptionStateRepo_UpdateSubscriptionStatus_DBErrorOnUpdate(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	// Mock zombie check: org not deleted
	db.On("QueryRow", mock.Anything,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		mock.Anything,
	).Return(&mockRow{
		scanFn: func(dest ...any) error {
			p := dest[0].(**time.Time)
			*p = nil
			return nil
		},
	})

	// Mock the UPDATE fails
	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("timeout"))

	err := repo.UpdateSubscriptionStatus(
		context.Background(),
		"org_1",
		types.PlanPro,
		types.SubStatusActive,
		time.Now(),
	)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestSubscriptionStateRepo_UpdatePaymentFailure_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	failedAt := time.Now().UTC()

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdatePaymentFailure(context.Background(), "org_1", failedAt)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSubscriptionStateRepo_UpdatePaymentFailure_OrgNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdatePaymentFailure(context.Background(), "org_nonexistent", time.Now())
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)
}

func TestSubscriptionStateRepo_UpdatePaymentFailure_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSubscriptionStateRepo(db, nil)

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.UpdatePaymentFailure(context.Background(), "org_1", time.Now())
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

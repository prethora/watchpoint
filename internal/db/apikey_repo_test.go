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

// Note: mockDBTX and mockRow are defined in session_repo_test.go.

// ============================================================
// Create Tests
// ============================================================

func TestAPIKeyRepository_Create_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	userID := "user_abc"
	key := &types.APIKey{
		ID:              "key_test1",
		OrganizationID:  "org_abc",
		CreatedByUserID: &userID,
		KeyHash:         "$2a$12$hashedvaluehere",
		KeyPrefix:       "sk_live_abcdefgh",
		Scopes:          []string{"watchpoints:read"},
		TestMode:        false,
		Name:            "My Test Key",
		CreatedAt:       time.Now().UTC(),
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, key)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAPIKeyRepository_Create_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	userID := "user_abc"
	key := &types.APIKey{
		ID:              "key_test1",
		OrganizationID:  "org_abc",
		CreatedByUserID: &userID,
		KeyHash:         "$2a$12$hashedvalue",
		KeyPrefix:       "sk_live_abcdefgh",
		Scopes:          []string{"watchpoints:read"},
		Name:            "Test Key",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.Create(ctx, key)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// ============================================================
// GetByID Tests
// ============================================================

func TestAPIKeyRepository_GetByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	now := time.Now().UTC()
	userID := "user_1"
	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "key_found"
			*dest[1].(*string) = "org_1"
			*dest[2].(**string) = &userID
			*dest[3].(*string) = "$2a$12$hash"
			*dest[4].(*string) = "sk_live_abc"
			*dest[5].(*[]string) = []string{"watchpoints:read"}
			*dest[6].(*bool) = false
			*dest[7].(*string) = ""
			*dest[8].(*string) = "My Key"
			*dest[9].(**time.Time) = nil
			*dest[10].(**time.Time) = nil
			*dest[11].(**time.Time) = nil
			*dest[12].(*time.Time) = now
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).Return(row)

	key, err := repo.GetByID(ctx, "key_found", "org_1")
	require.NoError(t, err)
	assert.Equal(t, "key_found", key.ID)
	assert.Equal(t, "org_1", key.OrganizationID)
	assert.Equal(t, "$2a$12$hash", key.KeyHash)
	assert.Equal(t, "sk_live_abc", key.KeyPrefix)
	assert.Equal(t, "My Key", key.Name)
	assert.Nil(t, key.RevokedAt)
}

func TestAPIKeyRepository_GetByID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).Return(row)

	_, err := repo.GetByID(ctx, "key_nonexistent", "org_1")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundAPIKey, appErr.Code)
}

// ============================================================
// Delete (Revoke) Tests
// ============================================================

func TestAPIKeyRepository_Delete_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Delete(ctx, "key_123", "org_abc")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAPIKeyRepository_Delete_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Delete(ctx, "key_nonexistent", "org_abc")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundAPIKey, appErr.Code)
}

func TestAPIKeyRepository_Delete_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Delete(ctx, "key_123", "org_abc")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// ============================================================
// RevokeByUser Tests
// ============================================================

func TestAPIKeyRepository_RevokeByUser_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	err := repo.RevokeByUser(ctx, "user_1", "org_abc")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAPIKeyRepository_RevokeByUser_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.RevokeByUser(ctx, "user_1", "org_abc")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// ============================================================
// TouchLastUsed Tests
// ============================================================

func TestAPIKeyRepository_TouchLastUsed_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.TouchLastUsed(ctx, "key_abc")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

// ============================================================
// CountRecentByUser Tests
// ============================================================

func TestAPIKeyRepository_CountRecentByUser_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	since := time.Now().UTC().Add(-1 * time.Hour)

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 3
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).Return(row)

	count, err := repo.CountRecentByUser(ctx, "user_abc", since)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestAPIKeyRepository_CountRecentByUser_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAPIKeyRepository(db)
	ctx := context.Background()

	since := time.Now().UTC().Add(-1 * time.Hour)

	row := &mockRow{scanErr: errors.New("db error")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).Return(row)

	_, err := repo.CountRecentByUser(ctx, "user_abc", since)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// ============================================================
// Helper Tests
// ============================================================

func TestNilIfEmptyString(t *testing.T) {
	assert.Nil(t, nilIfEmptyString(""))

	result := nilIfEmptyString("hello")
	require.NotNil(t, result)
	assert.Equal(t, "hello", *result)
}

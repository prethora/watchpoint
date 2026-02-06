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

// --- SecurityRepository Tests ---

func TestSecurityRepository_LogAttempt_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	event := &types.SecurityEvent{
		EventType:     "login",
		Identifier:    "user@example.com",
		IPAddress:     "192.168.1.1",
		AttemptedAt:   time.Now().UTC(),
		Success:       false,
		FailureReason: "invalid_creds",
	}

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.LogAttempt(context.Background(), event)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSecurityRepository_LogAttempt_NullableFields(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	// IP-only event with no identifier and zero time
	event := &types.SecurityEvent{
		EventType: "api_auth",
		IPAddress: "10.0.0.1",
		Success:   false,
	}

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.MatchedBy(func(args []any) bool {
		// Verify nullable fields are typed nil pointers
		identifierPtr, _ := args[1].(*string)
		attemptedAtPtr, _ := args[3].(*time.Time)
		return identifierPtr == nil && attemptedAtPtr == nil
	})).Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.LogAttempt(context.Background(), event)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSecurityRepository_LogAttempt_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	event := &types.SecurityEvent{
		EventType: "login",
		IPAddress: "192.168.1.1",
		Success:   false,
	}

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.LogAttempt(context.Background(), event)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestSecurityRepository_CountRecentFailuresByIP_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	since := time.Now().Add(-15 * time.Minute)
	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 42
			return nil
		},
	}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	count, err := repo.CountRecentFailuresByIP(context.Background(), "192.168.1.1", since)
	require.NoError(t, err)
	assert.Equal(t, 42, count)
}

func TestSecurityRepository_CountRecentFailuresByIP_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	since := time.Now().Add(-15 * time.Minute)
	row := &mockRow{scanErr: errors.New("db error")}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	_, err := repo.CountRecentFailuresByIP(context.Background(), "192.168.1.1", since)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestSecurityRepository_CountRecentFailuresByIdentifier_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	since := time.Now().Add(-15 * time.Minute)
	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 3
			return nil
		},
	}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	count, err := repo.CountRecentFailuresByIdentifier(context.Background(), "user@example.com", since)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestSecurityRepository_CountRecentFailuresByIdentifier_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSecurityRepository(db)

	since := time.Now().Add(-15 * time.Minute)
	row := &mockRow{scanErr: errors.New("query failed")}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	_, err := repo.CountRecentFailuresByIdentifier(context.Background(), "user@example.com", since)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// --- Helper function tests ---

func TestNilIfEmpty(t *testing.T) {
	assert.Nil(t, nilIfEmpty(""))
	result := nilIfEmpty("hello")
	require.NotNil(t, result)
	assert.Equal(t, "hello", *result)
}

func TestNilIfZeroTime(t *testing.T) {
	assert.Nil(t, nilIfZeroTime(time.Time{}))
	now := time.Now()
	result := nilIfZeroTime(now)
	require.NotNil(t, result)
	assert.Equal(t, now, *result)
}

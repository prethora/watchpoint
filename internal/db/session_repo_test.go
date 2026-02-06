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

// --- Mock DBTX ---

type mockDBTX struct {
	mock.Mock
}

func (m *mockDBTX) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	args := m.Called(ctx, sql, arguments)
	return args.Get(0).(pgconn.CommandTag), args.Error(1)
}

func (m *mockDBTX) Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	args := m.Called(ctx, sql, arguments)
	if r := args.Get(0); r != nil {
		return r.(pgx.Rows), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *mockDBTX) QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row {
	args := m.Called(ctx, sql, arguments)
	return args.Get(0).(pgx.Row)
}

// --- Mock Row ---

type mockRow struct {
	scanErr error
	scanFn  func(dest ...any) error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return r.scanErr
}

// --- SessionRepository Tests ---

func TestSessionRepository_Create_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	session := &types.Session{
		ID:             "sess_test123",
		UserID:         "user_1",
		OrganizationID: "org_1",
		CSRFToken:      "csrf_abc",
		IPAddress:      "192.168.1.1",
		UserAgent:      "TestBrowser/1.0",
		ExpiresAt:      time.Now().Add(7 * 24 * time.Hour),
		LastActivityAt: time.Now(),
		CreatedAt:      time.Now(),
	}

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(context.Background(), session)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSessionRepository_Create_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	session := &types.Session{
		ID:     "sess_test123",
		UserID: "user_1",
	}

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.Create(context.Background(), session)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestSessionRepository_GetByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	now := time.Now().UTC()
	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "sess_found"
			*dest[1].(*string) = "user_1"
			*dest[2].(*string) = "org_1"
			*dest[3].(*string) = "csrf_token"
			*dest[4].(*string) = "192.168.1.1"
			*dest[5].(*string) = "TestBrowser/1.0"
			*dest[6].(*time.Time) = now.Add(7 * 24 * time.Hour)
			*dest[7].(*time.Time) = now
			*dest[8].(*time.Time) = now
			return nil
		},
	}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	session, err := repo.GetByID(context.Background(), "sess_found")
	require.NoError(t, err)
	assert.Equal(t, "sess_found", session.ID)
	assert.Equal(t, "user_1", session.UserID)
	assert.Equal(t, "org_1", session.OrganizationID)
	assert.Equal(t, "csrf_token", session.CSRFToken)
}

func TestSessionRepository_GetByID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).Return(row)

	_, err := repo.GetByID(context.Background(), "sess_nonexistent")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthSessionExpired, appErr.Code)
}

func TestSessionRepository_DeleteByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("DELETE 1"), nil)

	err := repo.DeleteByID(context.Background(), "sess_abc")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSessionRepository_DeleteByUser_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("DELETE 3"), nil)

	err := repo.DeleteByUser(context.Background(), "user_1")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestSessionRepository_DeleteExpiredByUser_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewSessionRepository(db)

	db.On("Exec", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("DELETE 2"), nil)

	err := repo.DeleteExpiredByUser(context.Background(), "user_1")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

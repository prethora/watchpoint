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

// Note: mockDBTX and mockRow are defined in session_repo_test.go and reused here.

// ============================================================
// GetByID Tests
// ============================================================

func TestUserRepository_GetByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	lastLogin := time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC)

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "user_123"                        // id
			*dest[1].(*string) = "org_456"                         // organization_id
			*dest[2].(*string) = "test@example.com"                // email
			s := "John Doe"                                        // name (nullable)
			*dest[3].(**string) = &s
			h := "$2a$12$hash"                                     // password_hash (nullable)
			*dest[4].(**string) = &h
			*dest[5].(*types.UserRole) = types.RoleOwner           // role
			*dest[6].(*types.UserStatus) = types.UserStatusActive  // status
			*dest[7].(**string) = nil                              // auth_provider
			*dest[8].(**string) = nil                              // auth_provider_id
			*dest[9].(**string) = nil                              // invite_token_hash
			*dest[10].(**time.Time) = nil                          // invite_expires_at
			*dest[11].(*time.Time) = now                           // created_at
			*dest[12].(**time.Time) = &lastLogin                   // last_login_at
			*dest[13].(**time.Time) = nil                          // deleted_at
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"user_123", "org_456"}).Return(row)

	user, err := repo.GetByID(ctx, "user_123", "org_456")
	require.NoError(t, err)
	assert.Equal(t, "user_123", user.ID)
	assert.Equal(t, "org_456", user.OrganizationID)
	assert.Equal(t, "test@example.com", user.Email)
	assert.Equal(t, "John Doe", user.Name)
	assert.Equal(t, "$2a$12$hash", user.PasswordHash)
	assert.Equal(t, types.RoleOwner, user.Role)
	assert.Equal(t, types.UserStatusActive, user.Status)
	assert.Empty(t, user.AuthProvider)
	assert.Empty(t, user.AuthProviderID)

	db.AssertExpectations(t)
}

func TestUserRepository_GetByID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"user_missing", "org_456"}).Return(row)

	_, err := repo.GetByID(ctx, "user_missing", "org_456")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// GetByEmail Tests
// ============================================================

func TestUserRepository_GetByEmail_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// GetByEmail scans userColumns + o.deleted_at (15 values)
	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "user_123"
			*dest[1].(*string) = "org_456"
			*dest[2].(*string) = "test@example.com"
			n := "Jane Doe"
			*dest[3].(**string) = &n
			h := "$2a$12$hash"
			*dest[4].(**string) = &h
			*dest[5].(*types.UserRole) = types.RoleAdmin
			*dest[6].(*types.UserStatus) = types.UserStatusActive
			g := "google"
			*dest[7].(**string) = &g
			gid := "google_id_123"
			*dest[8].(**string) = &gid
			*dest[9].(**string) = nil                  // invite_token_hash
			*dest[10].(**time.Time) = nil              // invite_expires_at
			*dest[11].(*time.Time) = now               // created_at
			*dest[12].(**time.Time) = &now             // last_login_at
			*dest[13].(**time.Time) = nil              // deleted_at
			*dest[14].(**time.Time) = nil              // o.deleted_at (org not deleted)
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"test@example.com"}).Return(row)

	user, err := repo.GetByEmail(ctx, "test@example.com")
	require.NoError(t, err)
	assert.Equal(t, "user_123", user.ID)
	assert.Equal(t, "test@example.com", user.Email)
	assert.Equal(t, "google", user.AuthProvider)
	assert.Equal(t, "google_id_123", user.AuthProviderID)

	db.AssertExpectations(t)
}

func TestUserRepository_GetByEmail_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"missing@example.com"}).Return(row)

	_, err := repo.GetByEmail(ctx, "missing@example.com")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthUserNotFound, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_GetByEmail_OrgDeleted(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	orgDeletedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "user_123"
			*dest[1].(*string) = "org_deleted"
			*dest[2].(*string) = "test@example.com"
			*dest[3].(**string) = nil                  // name
			h := "$2a$12$hash"
			*dest[4].(**string) = &h
			*dest[5].(*types.UserRole) = types.RoleOwner
			*dest[6].(*types.UserStatus) = types.UserStatusActive
			*dest[7].(**string) = nil                  // auth_provider
			*dest[8].(**string) = nil                  // auth_provider_id
			*dest[9].(**string) = nil                  // invite_token_hash
			*dest[10].(**time.Time) = nil              // invite_expires_at
			*dest[11].(*time.Time) = now               // created_at
			*dest[12].(**time.Time) = nil              // last_login_at
			*dest[13].(**time.Time) = nil              // deleted_at
			*dest[14].(**time.Time) = &orgDeletedAt    // o.deleted_at (org IS deleted)
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"test@example.com"}).Return(row)

	_, err := repo.GetByEmail(ctx, "test@example.com")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthOrgDeleted, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// GetByInviteTokenHash Tests
// ============================================================

func TestUserRepository_GetByInviteTokenHash_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "user_invite123"
			*dest[1].(*string) = "org_456"
			*dest[2].(*string) = "invited@example.com"
			*dest[3].(**string) = nil                             // name
			*dest[4].(**string) = nil                             // password_hash
			*dest[5].(*types.UserRole) = types.RoleMember
			*dest[6].(*types.UserStatus) = types.UserStatusInvited
			*dest[7].(**string) = nil                             // auth_provider
			*dest[8].(**string) = nil                             // auth_provider_id
			th := "sha256_hash_value"
			*dest[9].(**string) = &th                             // invite_token_hash
			*dest[10].(**time.Time) = &expiresAt                  // invite_expires_at
			*dest[11].(*time.Time) = now                          // created_at
			*dest[12].(**time.Time) = nil                         // last_login_at
			*dest[13].(**time.Time) = nil                         // deleted_at
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"sha256_hash_value"}).Return(row)

	user, err := repo.GetByInviteTokenHash(ctx, "sha256_hash_value")
	require.NoError(t, err)
	assert.Equal(t, "user_invite123", user.ID)
	assert.Equal(t, types.UserStatusInvited, user.Status)
	assert.Equal(t, "sha256_hash_value", user.InviteTokenHash)
	assert.NotNil(t, user.InviteExpiresAt)

	db.AssertExpectations(t)
}

func TestUserRepository_GetByInviteTokenHash_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"bad_hash"}).Return(row)

	_, err := repo.GetByInviteTokenHash(ctx, "bad_hash")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthTokenInvalid, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// GetOwnerEmail Tests
// ============================================================

func TestUserRepository_GetOwnerEmail_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "owner@example.com"
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_456"}).Return(row)

	email, err := repo.GetOwnerEmail(ctx, "org_456")
	require.NoError(t, err)
	assert.Equal(t, "owner@example.com", email)

	db.AssertExpectations(t)
}

func TestUserRepository_GetOwnerEmail_NoOwner(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_no_owner"}).Return(row)

	_, err := repo.GetOwnerEmail(ctx, "org_no_owner")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdatePassword Tests
// ============================================================

func TestUserRepository_UpdatePassword_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"$2a$12$new_hash", "user_123"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdatePassword(ctx, "user_123", "$2a$12$new_hash")
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdatePassword_UserNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"$2a$12$new_hash", "user_missing"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdatePassword(ctx, "user_missing", "$2a$12$new_hash")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdatePassword_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"$2a$12$new_hash", "user_123"}).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.UpdatePassword(ctx, "user_123", "$2a$12$new_hash")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateLastLogin Tests
// ============================================================

func TestUserRepository_UpdateLastLogin_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"user_123"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateLastLogin(ctx, "user_123")
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdateLastLogin_UserNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"user_missing"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdateLastLogin(ctx, "user_missing")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateStatus Tests
// ============================================================

func TestUserRepository_UpdateStatus_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{types.UserStatusActive, "John Doe", "$2a$12$hash", "user_123"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateStatus(ctx, "user_123", types.UserStatusActive, "John Doe", "$2a$12$hash")
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdateStatus_UserNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{types.UserStatusActive, "John", "$2a$12$hash", "user_missing"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdateStatus(ctx, "user_missing", types.UserStatusActive, "John", "$2a$12$hash")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// CreateWithProvider Tests
// ============================================================

func TestUserRepository_CreateWithProvider_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	user := &types.User{
		ID:             "user_oauth123",
		OrganizationID: "org_456",
		Email:          "oauth@example.com",
		Name:           "OAuth User",
		Role:           types.RoleOwner,
		Status:         types.UserStatusActive,
		AuthProvider:   "google",
		AuthProviderID: "google_sub_123",
		CreatedAt:      now,
	}

	gp := "google"
	gid := "google_sub_123"

	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{
			user.ID, user.OrganizationID, user.Email, user.Name,
			(*string)(nil), // password_hash is nil (OAuth user)
			user.Role, user.Status,
			&gp,
			&gid,
			user.CreatedAt,
		}).Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.CreateWithProvider(ctx, user)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_CreateWithProvider_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_dup",
		OrganizationID: "org_456",
		Email:          "existing@example.com",
		Role:           types.RoleOwner,
		Status:         types.UserStatusActive,
		CreatedAt:      time.Now(),
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("unique_violation"))

	err := repo.CreateWithProvider(ctx, user)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// CountOwners Tests
// ============================================================

func TestUserRepository_CountOwners_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 2
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_456"}).Return(row)

	count, err := repo.CountOwners(ctx, "org_456")
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	db.AssertExpectations(t)
}

func TestUserRepository_CountOwners_Zero(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int) = 0
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_empty"}).Return(row)

	count, err := repo.CountOwners(ctx, "org_empty")
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	db.AssertExpectations(t)
}

func TestUserRepository_CountOwners_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: errors.New("db error")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"org_456"}).Return(row)

	_, err := repo.CountOwners(ctx, "org_456")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// CreateInvited Tests
// ============================================================

func TestUserRepository_CreateInvited_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(72 * time.Hour)

	user := &types.User{
		ID:             "user_invite1",
		OrganizationID: "org_456",
		Email:          "invited@example.com",
		Role:           types.RoleMember,
		CreatedAt:      now,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.CreateInvited(ctx, user, "bcrypt_hash_123", expiresAt)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_CreateInvited_DuplicateEmail(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_dup_invite",
		OrganizationID: "org_456",
		Email:          "existing@example.com",
		Role:           types.RoleMember,
	}

	// Simulate unique constraint violation (PG error code 23505)
	pgErr := &pgconn.PgError{Code: "23505"}
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, pgErr)

	expiresAt := time.Now().Add(72 * time.Hour)
	err := repo.CreateInvited(ctx, user, "hash", expiresAt)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeConflictEmail, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_CreateInvited_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_invite_err",
		OrganizationID: "org_456",
		Email:          "error@example.com",
		Role:           types.RoleMember,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	expiresAt := time.Now().Add(72 * time.Hour)
	err := repo.CreateInvited(ctx, user, "hash", expiresAt)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Update Tests
// ============================================================

func TestUserRepository_Update_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_123",
		OrganizationID: "org_456",
		Email:          "updated@example.com",
		Name:           "Updated Name",
		Role:           types.RoleAdmin,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Update(ctx, user)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_Update_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_missing",
		OrganizationID: "org_456",
		Email:          "missing@example.com",
		Role:           types.RoleMember,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Update(ctx, user)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_Update_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &types.User{
		ID:             "user_123",
		OrganizationID: "org_456",
		Email:          "test@example.com",
		Role:           types.RoleMember,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Update(ctx, user)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Delete Tests (Soft Delete)
// ============================================================

func TestUserRepository_Delete_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"user_123", "org_456"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Delete(ctx, "user_123", "org_456")
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_Delete_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"user_missing", "org_456"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Delete(ctx, "user_missing", "org_456")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_Delete_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{"user_123", "org_456"}).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Delete(ctx, "user_123", "org_456")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateInviteToken Tests
// ============================================================

func TestUserRepository_UpdateInviteToken_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{"new_token_hash", expiresAt, "user_invite1"}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateInviteToken(ctx, "user_invite1", "new_token_hash", expiresAt)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdateInviteToken_NotInvited(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// User is active (not invited), so WHERE status='invited' won't match
	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{"new_hash", expiresAt, "user_active"}).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdateInviteToken(ctx, "user_active", "new_hash", expiresAt)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundUser, appErr.Code)

	db.AssertExpectations(t)
}

func TestUserRepository_UpdateInviteToken_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUserRepository(db)
	ctx := context.Background()

	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	db.On("Exec", ctx, mock.AnythingOfType("string"),
		[]any{"hash", expiresAt, "user_1"}).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.UpdateInviteToken(ctx, "user_1", "hash", expiresAt)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

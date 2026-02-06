package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// --- Mock UserRepo ---

type mockUserRepo struct {
	mock.Mock
}

func (m *mockUserRepo) GetByEmail(ctx context.Context, email string) (*types.User, error) {
	args := m.Called(ctx, email)
	if u := args.Get(0); u != nil {
		return u.(*types.User), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *mockUserRepo) GetByInviteTokenHash(ctx context.Context, tokenHash string) (*types.User, error) {
	args := m.Called(ctx, tokenHash)
	if u := args.Get(0); u != nil {
		return u.(*types.User), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *mockUserRepo) UpdatePassword(ctx context.Context, userID string, newHash string) error {
	args := m.Called(ctx, userID, newHash)
	return args.Error(0)
}

func (m *mockUserRepo) UpdateLastLogin(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *mockUserRepo) UpdateStatus(ctx context.Context, userID string, status types.UserStatus, name string, passwordHash string) error {
	args := m.Called(ctx, userID, status, name, passwordHash)
	return args.Error(0)
}

// --- Mock PasswordHasher ---

type mockPasswordHasher struct {
	mock.Mock
}

func (m *mockPasswordHasher) CompareHashAndPassword(hashedPassword, password string) error {
	args := m.Called(hashedPassword, password)
	return args.Error(0)
}

func (m *mockPasswordHasher) GenerateFromPassword(password string) (string, error) {
	args := m.Called(password)
	return args.String(0), args.Error(1)
}

// --- Mock AuthTxManager ---

type mockAuthTxManager struct {
	mock.Mock
	// txUserRepo and txSessionRepo are provided to the callback when called
	txUserRepo    UserRepo
	txSessionRepo SessionRepo
}

// RunInTx executes the callback with the pre-configured transaction repos.
// This mock immediately executes the callback (simulating a successful transaction)
// unless an error is configured.
func (m *mockAuthTxManager) RunInTx(ctx context.Context, fn func(ctx context.Context, txUserRepo UserRepo, txSessionRepo SessionRepo) error) error {
	args := m.Called(ctx, fn)
	if args.Error(0) != nil {
		return args.Error(0)
	}
	// Execute the callback with transaction-scoped repos
	return fn(ctx, m.txUserRepo, m.txSessionRepo)
}

// newMockAuthTxManager creates a mock tx manager that executes callbacks
// with the provided mock repos.
func newMockAuthTxManager(txUserRepo UserRepo, txSessionRepo SessionRepo) *mockAuthTxManager {
	return &mockAuthTxManager{
		txUserRepo:    txUserRepo,
		txSessionRepo: txSessionRepo,
	}
}

// --- Test Fixtures ---

func activeUser() *types.User {
	return &types.User{
		ID:             "user_test123",
		OrganizationID: "org_test456",
		Email:          "test@example.com",
		PasswordHash:   "$2a$12$hashedpassword",
		Role:           types.RoleOwner,
		Status:         types.UserStatusActive,
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func invitedUser() *types.User {
	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // future
	return &types.User{
		ID:              "user_invite789",
		OrganizationID:  "org_test456",
		Email:           "invited@example.com",
		Role:            types.RoleMember,
		Status:          types.UserStatusInvited,
		InviteTokenHash: HashToken("raw_invite_token"),
		InviteExpiresAt: &expiresAt,
		CreatedAt:       time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
	}
}

func newTestAuthService(
	userRepo *mockUserRepo,
	hasher *mockPasswordHasher,
	txManager *mockAuthTxManager,
	secSvc *mockSecurityService,
	clock *mockClock,
) *authService {
	sessionRepo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(sessionRepo, tokenGen, DefaultSessionConfig(), clock, nil)

	return NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})
}

// ============================================================
// Login Tests
// ============================================================

func TestAuthService_Login_Success(t *testing.T) {
	user := activeUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}

	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)

	// Set up tx-scoped repos
	txUserRepo := new(mockUserRepo)
	txSessionRepo := new(mockSessionRepo)
	txManager := newMockAuthTxManager(txUserRepo, txSessionRepo)

	// The AuthService uses the sessionSvc internally which needs token gen
	// We need to wire up the session service that will be used inside the tx
	sessionRepo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(sessionRepo, tokenGen, DefaultSessionConfig(), clock, nil)

	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	// Step 1: User lookup
	userRepo.On("GetByEmail", mock.Anything, "test@example.com").Return(user, nil)

	// Step 2: Password verification
	hasher.On("CompareHashAndPassword", user.PasswordHash, "correct_password").Return(nil)

	// Step 4: Transaction operations
	txManager.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	// 4a. Update last login
	txUserRepo.On("UpdateLastLogin", mock.Anything, user.ID).Return(nil)

	// 4b. Session creation (via sessionSvc.withRepo which uses txSessionRepo)
	tokenGen.On("GenerateSessionID").Return("sess_test_session_id", nil)
	tokenGen.On("GenerateCSRF").Return("csrf_test_token", nil)
	txSessionRepo.On("Create", mock.Anything, mock.MatchedBy(func(s *types.Session) bool {
		return s.ID == "sess_test_session_id" &&
			s.UserID == user.ID &&
			s.OrganizationID == user.OrganizationID &&
			s.CSRFToken == "csrf_test_token"
	})).Return(nil)

	// 4c. Lazy cleanup
	txSessionRepo.On("DeleteExpiredByUser", mock.Anything, user.ID).Return(nil)

	// Step 5: Record success
	secSvc.On("RecordAttempt", mock.Anything, "login", "test@example.com", "192.168.1.1", true, "").Return(nil)

	// Execute
	resultUser, resultSession, err := svc.Login(context.Background(), "test@example.com", "correct_password", "192.168.1.1")

	require.NoError(t, err)
	assert.Equal(t, user.ID, resultUser.ID)
	assert.Equal(t, user.Email, resultUser.Email)
	assert.NotNil(t, resultSession)
	assert.Equal(t, "sess_test_session_id", resultSession.ID)
	assert.Equal(t, user.ID, resultSession.UserID)

	userRepo.AssertExpectations(t)
	hasher.AssertExpectations(t)
	txUserRepo.AssertExpectations(t)
	txSessionRepo.AssertExpectations(t)
	secSvc.AssertExpectations(t)
}

func TestAuthService_Login_UserNotFound_MaskedAsInvalidCreds(t *testing.T) {
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	// User not found
	userRepo.On("GetByEmail", mock.Anything, "nonexistent@example.com").
		Return(nil, types.NewAppError(types.ErrCodeAuthUserNotFound, "user not found", nil))

	// Should record failed attempt
	secSvc.On("RecordAttempt", mock.Anything, "login", "nonexistent@example.com", "10.0.0.1", false, "user_not_found").Return(nil)

	_, _, err := svc.Login(context.Background(), "nonexistent@example.com", "any_password", "10.0.0.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	// Must return ErrCodeAuthInvalidCreds (not ErrCodeAuthUserNotFound) for enumeration protection
	assert.Equal(t, types.ErrCodeAuthInvalidCreds, appErr.Code)
	assert.Contains(t, appErr.Message, "invalid email or password")

	userRepo.AssertExpectations(t)
	secSvc.AssertExpectations(t)
}

func TestAuthService_Login_InvalidPassword(t *testing.T) {
	user := activeUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	userRepo.On("GetByEmail", mock.Anything, "test@example.com").Return(user, nil)
	hasher.On("CompareHashAndPassword", user.PasswordHash, "wrong_password").Return(errors.New("hash mismatch"))
	secSvc.On("RecordAttempt", mock.Anything, "login", "test@example.com", "192.168.1.1", false, "invalid_creds").Return(nil)

	_, _, err := svc.Login(context.Background(), "test@example.com", "wrong_password", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthInvalidCreds, appErr.Code)

	userRepo.AssertExpectations(t)
	hasher.AssertExpectations(t)
	secSvc.AssertExpectations(t)
}

func TestAuthService_Login_AccountNotActive(t *testing.T) {
	user := activeUser()
	user.Status = types.UserStatusInvited // Not active
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	userRepo.On("GetByEmail", mock.Anything, "test@example.com").Return(user, nil)
	hasher.On("CompareHashAndPassword", user.PasswordHash, "correct_password").Return(nil)
	secSvc.On("RecordAttempt", mock.Anything, "login", "test@example.com", "192.168.1.1", false, "account_not_active").Return(nil)

	_, _, err := svc.Login(context.Background(), "test@example.com", "correct_password", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthAccountNotActive, appErr.Code)

	userRepo.AssertExpectations(t)
	hasher.AssertExpectations(t)
	secSvc.AssertExpectations(t)
}

func TestAuthService_Login_OrgDeleted(t *testing.T) {
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	// Org is soft-deleted
	userRepo.On("GetByEmail", mock.Anything, "test@example.com").
		Return(nil, types.NewAppError(types.ErrCodeAuthOrgDeleted, "org deleted", nil))

	_, _, err := svc.Login(context.Background(), "test@example.com", "any_password", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthOrgDeleted, appErr.Code)

	userRepo.AssertExpectations(t)
}

func TestAuthService_Login_TransactionRollback(t *testing.T) {
	user := activeUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)

	txUserRepo := new(mockUserRepo)
	txSessionRepo := new(mockSessionRepo)
	txManager := newMockAuthTxManager(txUserRepo, txSessionRepo)

	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(new(mockSessionRepo), tokenGen, DefaultSessionConfig(), clock, nil)

	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	userRepo.On("GetByEmail", mock.Anything, "test@example.com").Return(user, nil)
	hasher.On("CompareHashAndPassword", user.PasswordHash, "correct_password").Return(nil)
	txManager.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	// UpdateLastLogin succeeds but session creation fails
	txUserRepo.On("UpdateLastLogin", mock.Anything, user.ID).Return(nil)
	tokenGen.On("GenerateSessionID").Return("", errors.New("entropy failure"))

	_, _, err := svc.Login(context.Background(), "test@example.com", "correct_password", "192.168.1.1")

	require.Error(t, err)
	// No security success event should be recorded since the transaction failed
	secSvc.AssertNotCalled(t, "RecordAttempt", mock.Anything, "login", "test@example.com", "192.168.1.1", true, "")
}

func TestAuthService_Login_LazyCleanupFailureDoesNotBlockLogin(t *testing.T) {
	user := activeUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)

	txUserRepo := new(mockUserRepo)
	txSessionRepo := new(mockSessionRepo)
	txManager := newMockAuthTxManager(txUserRepo, txSessionRepo)

	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(new(mockSessionRepo), tokenGen, DefaultSessionConfig(), clock, nil)

	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	userRepo.On("GetByEmail", mock.Anything, "test@example.com").Return(user, nil)
	hasher.On("CompareHashAndPassword", user.PasswordHash, "correct_password").Return(nil)
	txManager.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	txUserRepo.On("UpdateLastLogin", mock.Anything, user.ID).Return(nil)
	tokenGen.On("GenerateSessionID").Return("sess_test_id", nil)
	tokenGen.On("GenerateCSRF").Return("csrf_test", nil)
	txSessionRepo.On("Create", mock.Anything, mock.Anything).Return(nil)

	// Cleanup fails - this should NOT fail the login
	txSessionRepo.On("DeleteExpiredByUser", mock.Anything, user.ID).
		Return(errors.New("cleanup failed"))

	secSvc.On("RecordAttempt", mock.Anything, "login", "test@example.com", "192.168.1.1", true, "").Return(nil)

	resultUser, resultSession, err := svc.Login(context.Background(), "test@example.com", "correct_password", "192.168.1.1")

	require.NoError(t, err)
	assert.Equal(t, user.ID, resultUser.ID)
	assert.NotNil(t, resultSession)
}

// ============================================================
// AcceptInvite Tests
// ============================================================

func TestAuthService_AcceptInvite_Success(t *testing.T) {
	user := invitedUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)

	txUserRepo := new(mockUserRepo)
	txSessionRepo := new(mockSessionRepo)
	txManager := newMockAuthTxManager(txUserRepo, txSessionRepo)

	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(new(mockSessionRepo), tokenGen, DefaultSessionConfig(), clock, nil)

	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	tokenHash := HashToken("raw_invite_token")

	// Step 1: Find user by invite token
	userRepo.On("GetByInviteTokenHash", mock.Anything, tokenHash).Return(user, nil)

	// Step 3: Hash the new password
	hasher.On("GenerateFromPassword", "NewSecurePassword1!").Return("$2a$12$new_hashed_password", nil)

	// Step 4: Transaction
	txManager.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	// 4a. Update user status
	txUserRepo.On("UpdateStatus", mock.Anything, user.ID, types.UserStatusActive, "John Doe", "$2a$12$new_hashed_password").Return(nil)

	// 4b. Create session
	tokenGen.On("GenerateSessionID").Return("sess_invite_session", nil)
	tokenGen.On("GenerateCSRF").Return("csrf_invite_token", nil)
	txSessionRepo.On("Create", mock.Anything, mock.MatchedBy(func(s *types.Session) bool {
		return s.UserID == user.ID &&
			s.OrganizationID == user.OrganizationID
	})).Return(nil)

	resultUser, resultSession, err := svc.AcceptInvite(context.Background(), "raw_invite_token", "John Doe", "NewSecurePassword1!", "192.168.1.1")

	require.NoError(t, err)
	assert.Equal(t, user.ID, resultUser.ID)
	assert.Equal(t, types.UserStatusActive, resultUser.Status)
	assert.Equal(t, "John Doe", resultUser.Name)
	assert.Empty(t, resultUser.InviteTokenHash)
	assert.Nil(t, resultUser.InviteExpiresAt)
	assert.NotNil(t, resultSession)
	assert.Equal(t, "sess_invite_session", resultSession.ID)

	userRepo.AssertExpectations(t)
	hasher.AssertExpectations(t)
	txUserRepo.AssertExpectations(t)
	txSessionRepo.AssertExpectations(t)
}

func TestAuthService_AcceptInvite_InvalidToken(t *testing.T) {
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	badTokenHash := HashToken("invalid_token")
	userRepo.On("GetByInviteTokenHash", mock.Anything, badTokenHash).
		Return(nil, types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid invite token", nil))

	_, _, err := svc.AcceptInvite(context.Background(), "invalid_token", "John", "Password1!", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthTokenInvalid, appErr.Code)
}

func TestAuthService_AcceptInvite_ExpiredToken(t *testing.T) {
	user := invitedUser()
	// Set invite to expired (in the past relative to clock)
	expiredAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	user.InviteExpiresAt = &expiredAt

	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)} // After expiry
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	tokenHash := HashToken("raw_invite_token")
	userRepo.On("GetByInviteTokenHash", mock.Anything, tokenHash).Return(user, nil)

	_, _, err := svc.AcceptInvite(context.Background(), "raw_invite_token", "John", "Password1!", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthTokenExpired, appErr.Code)
}

func TestAuthService_AcceptInvite_TransactionRollback_KeepsInviteValid(t *testing.T) {
	user := invitedUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)

	txUserRepo := new(mockUserRepo)
	txSessionRepo := new(mockSessionRepo)
	txManager := newMockAuthTxManager(txUserRepo, txSessionRepo)

	tokenGen := new(mockTokenGenerator)
	sessSvc := NewSessionService(new(mockSessionRepo), tokenGen, DefaultSessionConfig(), clock, nil)

	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	tokenHash := HashToken("raw_invite_token")
	userRepo.On("GetByInviteTokenHash", mock.Anything, tokenHash).Return(user, nil)
	hasher.On("GenerateFromPassword", "NewPassword1!").Return("$2a$12$hashed", nil)
	txManager.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	// Status update succeeds, but session creation fails
	txUserRepo.On("UpdateStatus", mock.Anything, user.ID, types.UserStatusActive, "John", "$2a$12$hashed").Return(nil)
	tokenGen.On("GenerateSessionID").Return("", errors.New("entropy failure"))

	_, _, err := svc.AcceptInvite(context.Background(), "raw_invite_token", "John", "NewPassword1!", "192.168.1.1")

	// The transaction should have failed, meaning the invite token remains valid
	require.Error(t, err)
	// The original user object should NOT have been modified
	assert.Equal(t, types.UserStatusInvited, user.Status)
	assert.NotEmpty(t, user.InviteTokenHash)
}

func TestAuthService_AcceptInvite_PasswordHashFailure(t *testing.T) {
	user := invitedUser()
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	userRepo := new(mockUserRepo)
	hasher := new(mockPasswordHasher)
	secSvc := new(mockSecurityService)
	txManager := newMockAuthTxManager(nil, nil)

	sessSvc := NewSessionService(new(mockSessionRepo), new(mockTokenGenerator), DefaultSessionConfig(), clock, nil)
	svc := NewAuthService(AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessSvc,
		Security:       secSvc,
		TxManager:      txManager,
		Hasher:         hasher,
		Clock:          clock,
	})

	tokenHash := HashToken("raw_invite_token")
	userRepo.On("GetByInviteTokenHash", mock.Anything, tokenHash).Return(user, nil)
	hasher.On("GenerateFromPassword", "NewPassword1!").Return("", errors.New("bcrypt error"))

	_, _, err := svc.AcceptInvite(context.Background(), "raw_invite_token", "John", "NewPassword1!", "192.168.1.1")

	require.Error(t, err)
	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalUnexpected, appErr.Code)
}

// ============================================================
// HashToken Tests
// ============================================================

func TestHashToken_Deterministic(t *testing.T) {
	hash1 := HashToken("test_token_123")
	hash2 := HashToken("test_token_123")
	assert.Equal(t, hash1, hash2, "same input should produce same hash")
}

func TestHashToken_DifferentInputs(t *testing.T) {
	hash1 := HashToken("token_a")
	hash2 := HashToken("token_b")
	assert.NotEqual(t, hash1, hash2, "different inputs should produce different hashes")
}

func TestHashToken_Format(t *testing.T) {
	hash := HashToken("any_token")
	// SHA-256 produces 32 bytes = 64 hex characters
	assert.Equal(t, 64, len(hash))
}

// ============================================================
// bcryptHasher Tests
// ============================================================

func TestBcryptHasher_RoundTrip(t *testing.T) {
	h := &bcryptHasher{}

	hash, err := h.GenerateFromPassword("test_password")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	err = h.CompareHashAndPassword(hash, "test_password")
	assert.NoError(t, err)
}

func TestBcryptHasher_WrongPassword(t *testing.T) {
	h := &bcryptHasher{}

	hash, err := h.GenerateFromPassword("correct_password")
	require.NoError(t, err)

	err = h.CompareHashAndPassword(hash, "wrong_password")
	assert.Error(t, err)
}

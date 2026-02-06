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

// --- Mock SessionRepo ---

type mockSessionRepo struct {
	mock.Mock
}

func (m *mockSessionRepo) Create(ctx context.Context, session *types.Session) error {
	args := m.Called(ctx, session)
	return args.Error(0)
}

func (m *mockSessionRepo) GetByID(ctx context.Context, sessionID string) (*types.Session, error) {
	args := m.Called(ctx, sessionID)
	if s := args.Get(0); s != nil {
		return s.(*types.Session), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *mockSessionRepo) DeleteByID(ctx context.Context, sessionID string) error {
	args := m.Called(ctx, sessionID)
	return args.Error(0)
}

func (m *mockSessionRepo) DeleteByUser(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *mockSessionRepo) DeleteExpiredByUser(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

// --- Mock TokenGenerator ---

type mockTokenGenerator struct {
	mock.Mock
}

func (m *mockTokenGenerator) GenerateSessionID() (string, error) {
	args := m.Called()
	return args.String(0), args.Error(1)
}

func (m *mockTokenGenerator) GenerateCSRF() (string, error) {
	args := m.Called()
	return args.String(0), args.Error(1)
}

func (m *mockTokenGenerator) GenerateSecureToken() (string, error) {
	args := m.Called()
	return args.String(0), args.Error(1)
}

func (m *mockTokenGenerator) GenerateOAuthState() (string, error) {
	args := m.Called()
	return args.String(0), args.Error(1)
}

// --- SessionService Tests ---

func newTestSessionService(repo *mockSessionRepo, tokenGen *mockTokenGenerator, clock *mockClock) *sessionService {
	return NewSessionService(repo, tokenGen, DefaultSessionConfig(), clock, nil)
}

func TestSessionService_CreateSession_Success(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	tokenGen.On("GenerateSessionID").Return("sess_abc123", nil)
	tokenGen.On("GenerateCSRF").Return("csrf_token_xyz", nil)

	repo.On("Create", mock.Anything, mock.MatchedBy(func(s *types.Session) bool {
		return s.ID == "sess_abc123" &&
			s.UserID == "user_1" &&
			s.OrganizationID == "org_1" &&
			s.CSRFToken == "csrf_token_xyz" &&
			s.IPAddress == "192.168.1.1" &&
			s.UserAgent == "TestBrowser/1.0" &&
			s.ExpiresAt.Equal(clock.now.Add(7*24*time.Hour)) &&
			s.LastActivityAt.Equal(clock.now) &&
			s.CreatedAt.Equal(clock.now)
	})).Return(nil)

	session, sessionID, err := svc.CreateSession(context.Background(), "user_1", "org_1", "192.168.1.1", "TestBrowser/1.0")
	require.NoError(t, err)
	assert.Equal(t, "sess_abc123", sessionID)
	assert.Equal(t, "sess_abc123", session.ID)
	assert.Equal(t, "user_1", session.UserID)
	assert.Equal(t, "org_1", session.OrganizationID)
	assert.Equal(t, "csrf_token_xyz", session.CSRFToken)
	assert.Equal(t, "192.168.1.1", session.IPAddress)
	assert.Equal(t, "TestBrowser/1.0", session.UserAgent)

	repo.AssertExpectations(t)
	tokenGen.AssertExpectations(t)
}

func TestSessionService_CreateSession_SessionIDGenerationError(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	tokenGen.On("GenerateSessionID").Return("", errors.New("entropy failure"))

	_, _, err := svc.CreateSession(context.Background(), "user_1", "org_1", "192.168.1.1", "TestBrowser/1.0")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalUnexpected, appErr.Code)
}

func TestSessionService_CreateSession_CSRFGenerationError(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	tokenGen.On("GenerateSessionID").Return("sess_abc123", nil)
	tokenGen.On("GenerateCSRF").Return("", errors.New("entropy failure"))

	_, _, err := svc.CreateSession(context.Background(), "user_1", "org_1", "192.168.1.1", "TestBrowser/1.0")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalUnexpected, appErr.Code)
}

func TestSessionService_CreateSession_RepoError(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	tokenGen.On("GenerateSessionID").Return("sess_abc123", nil)
	tokenGen.On("GenerateCSRF").Return("csrf_token_xyz", nil)
	repo.On("Create", mock.Anything, mock.Anything).Return(errors.New("db error"))

	_, _, err := svc.CreateSession(context.Background(), "user_1", "org_1", "192.168.1.1", "TestBrowser/1.0")
	require.Error(t, err)
}

func TestSessionService_ValidateSession_Success(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	existingSession := &types.Session{
		ID:             "sess_abc123",
		UserID:         "user_1",
		OrganizationID: "org_1",
		CSRFToken:      "csrf_token",
		ExpiresAt:      clock.now.Add(24 * time.Hour), // Expires tomorrow
		LastActivityAt: clock.now.Add(-1 * time.Hour),
	}

	repo.On("GetByID", mock.Anything, "sess_abc123").Return(existingSession, nil)

	session, err := svc.ValidateSession(context.Background(), "sess_abc123")
	require.NoError(t, err)
	assert.Equal(t, "sess_abc123", session.ID)
	assert.Equal(t, "user_1", session.UserID)
}

func TestSessionService_ValidateSession_NotFound(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	repo.On("GetByID", mock.Anything, "sess_nonexistent").Return(nil,
		types.NewAppError(types.ErrCodeAuthSessionExpired, "session not found", nil))

	_, err := svc.ValidateSession(context.Background(), "sess_nonexistent")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthSessionExpired, appErr.Code)
}

func TestSessionService_ValidateSession_Expired(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	expiredSession := &types.Session{
		ID:             "sess_expired",
		UserID:         "user_1",
		OrganizationID: "org_1",
		ExpiresAt:      clock.now.Add(-1 * time.Hour), // Expired 1 hour ago
	}

	repo.On("GetByID", mock.Anything, "sess_expired").Return(expiredSession, nil)

	_, err := svc.ValidateSession(context.Background(), "sess_expired")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthSessionExpired, appErr.Code)
}

func TestSessionService_ValidateCSRF_Success(t *testing.T) {
	svc := newTestSessionService(new(mockSessionRepo), new(mockTokenGenerator),
		&mockClock{now: time.Now()})

	session := &types.Session{
		CSRFToken: "valid_csrf_token",
	}

	err := svc.ValidateCSRF(session, "valid_csrf_token")
	require.NoError(t, err)
}

func TestSessionService_ValidateCSRF_Invalid(t *testing.T) {
	svc := newTestSessionService(new(mockSessionRepo), new(mockTokenGenerator),
		&mockClock{now: time.Now()})

	session := &types.Session{
		CSRFToken: "correct_token",
	}

	err := svc.ValidateCSRF(session, "wrong_token")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthTokenInvalid, appErr.Code)
}

func TestSessionService_ValidateCSRF_NilSession(t *testing.T) {
	svc := newTestSessionService(new(mockSessionRepo), new(mockTokenGenerator),
		&mockClock{now: time.Now()})

	err := svc.ValidateCSRF(nil, "some_token")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeAuthSessionExpired, appErr.Code)
}

func TestSessionService_InvalidateSession(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	repo.On("DeleteByID", mock.Anything, "sess_abc123").Return(nil)

	err := svc.InvalidateSession(context.Background(), "sess_abc123")
	require.NoError(t, err)
	repo.AssertExpectations(t)
}

func TestSessionService_InvalidateAllUserSessions(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	repo.On("DeleteByUser", mock.Anything, "user_1").Return(nil)

	err := svc.InvalidateAllUserSessions(context.Background(), "user_1")
	require.NoError(t, err)
	repo.AssertExpectations(t)
}

func TestSessionService_CleanExpiredSessions(t *testing.T) {
	repo := new(mockSessionRepo)
	tokenGen := new(mockTokenGenerator)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := newTestSessionService(repo, tokenGen, clock)

	repo.On("DeleteExpiredByUser", mock.Anything, "user_1").Return(nil)

	err := svc.CleanExpiredSessions(context.Background(), "user_1")
	require.NoError(t, err)
	repo.AssertExpectations(t)
}

// --- CryptoTokenGenerator Tests ---

func TestCryptoTokenGenerator_GenerateSessionID(t *testing.T) {
	gen := NewCryptoTokenGenerator()

	id, err := gen.GenerateSessionID()
	require.NoError(t, err)
	assert.True(t, len(id) > 0)
	assert.Equal(t, "sess_", id[:5])
	// 5 (prefix) + 64 (hex encoding of 32 bytes) = 69 chars
	assert.Equal(t, 69, len(id))
}

func TestCryptoTokenGenerator_GenerateSessionID_Unique(t *testing.T) {
	gen := NewCryptoTokenGenerator()

	id1, err := gen.GenerateSessionID()
	require.NoError(t, err)
	id2, err := gen.GenerateSessionID()
	require.NoError(t, err)

	assert.NotEqual(t, id1, id2)
}

func TestCryptoTokenGenerator_GenerateCSRF(t *testing.T) {
	gen := NewCryptoTokenGenerator()

	token, err := gen.GenerateCSRF()
	require.NoError(t, err)
	// 64 hex chars (32 bytes)
	assert.Equal(t, 64, len(token))
}

func TestCryptoTokenGenerator_GenerateSecureToken(t *testing.T) {
	gen := NewCryptoTokenGenerator()

	token, err := gen.GenerateSecureToken()
	require.NoError(t, err)
	assert.Equal(t, 64, len(token))
}

func TestCryptoTokenGenerator_GenerateOAuthState(t *testing.T) {
	gen := NewCryptoTokenGenerator()

	state, err := gen.GenerateOAuthState()
	require.NoError(t, err)
	assert.Equal(t, 64, len(state))
}

// --- CanonicalizeEmail Tests ---

func TestCanonicalizeEmail(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already lowercase",
			input:    "user@example.com",
			expected: "user@example.com",
		},
		{
			name:     "mixed case",
			input:    "User@Example.COM",
			expected: "user@example.com",
		},
		{
			name:     "leading whitespace",
			input:    "  user@example.com",
			expected: "user@example.com",
		},
		{
			name:     "trailing whitespace",
			input:    "user@example.com  ",
			expected: "user@example.com",
		},
		{
			name:     "both whitespace and mixed case",
			input:    "  User@Example.COM  ",
			expected: "user@example.com",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only whitespace",
			input:    "   ",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CanonicalizeEmail(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// --- DefaultSessionConfig Tests ---

func TestDefaultSessionConfig(t *testing.T) {
	config := DefaultSessionConfig()
	assert.Equal(t, 7*24*time.Hour, config.SessionDuration)
	assert.Equal(t, "sess_", config.SessionIDPrefix)
}

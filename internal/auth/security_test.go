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

// --- Mock SecurityRepo ---

type mockSecurityRepo struct {
	mock.Mock
}

func (m *mockSecurityRepo) LogAttempt(ctx context.Context, event *types.SecurityEvent) error {
	args := m.Called(ctx, event)
	return args.Error(0)
}

func (m *mockSecurityRepo) CountRecentFailuresByIP(ctx context.Context, ip string, since time.Time) (int, error) {
	args := m.Called(ctx, ip, since)
	return args.Int(0), args.Error(1)
}

func (m *mockSecurityRepo) CountRecentFailuresByIdentifier(ctx context.Context, identifier string, since time.Time) (int, error) {
	args := m.Called(ctx, identifier, since)
	return args.Int(0), args.Error(1)
}

// --- Mock Clock ---

type mockClock struct {
	now time.Time
}

func (c *mockClock) Now() time.Time { return c.now }

// --- SecurityService Tests ---

func TestSecurityService_RecordAttempt_Success(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewSecurityService(repo, DefaultSecurityConfig(), clock, nil)

	repo.On("LogAttempt", mock.Anything, mock.MatchedBy(func(e *types.SecurityEvent) bool {
		return e.EventType == "login" &&
			e.Identifier == "test@example.com" &&
			e.IPAddress == "192.168.1.1" &&
			e.Success == false &&
			e.FailureReason == "invalid_creds" &&
			e.AttemptedAt.Equal(clock.now)
	})).Return(nil)

	err := svc.RecordAttempt(context.Background(), "login", "test@example.com", "192.168.1.1", false, "invalid_creds")
	require.NoError(t, err)
	repo.AssertExpectations(t)
}

func TestSecurityService_RecordAttempt_SuccessfulLogin(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewSecurityService(repo, DefaultSecurityConfig(), clock, nil)

	repo.On("LogAttempt", mock.Anything, mock.MatchedBy(func(e *types.SecurityEvent) bool {
		return e.EventType == "login" &&
			e.Success == true &&
			e.FailureReason == ""
	})).Return(nil)

	err := svc.RecordAttempt(context.Background(), "login", "test@example.com", "192.168.1.1", true, "")
	require.NoError(t, err)
	repo.AssertExpectations(t)
}

func TestSecurityService_RecordAttempt_RepoError(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewSecurityService(repo, DefaultSecurityConfig(), clock, nil)

	repo.On("LogAttempt", mock.Anything, mock.Anything).Return(errors.New("db error"))

	err := svc.RecordAttempt(context.Background(), "login", "test@example.com", "192.168.1.1", false, "invalid_creds")
	require.Error(t, err)
}

func TestSecurityService_IsIPBlocked_NotBlocked(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIP", mock.Anything, "192.168.1.1", expectedSince).Return(50, nil)

	blocked := svc.IsIPBlocked(context.Background(), "192.168.1.1")
	assert.False(t, blocked)
	repo.AssertExpectations(t)
}

func TestSecurityService_IsIPBlocked_Blocked(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIP", mock.Anything, "10.0.0.1", expectedSince).Return(100, nil)

	blocked := svc.IsIPBlocked(context.Background(), "10.0.0.1")
	assert.True(t, blocked)
	repo.AssertExpectations(t)
}

func TestSecurityService_IsIPBlocked_FailsOpen(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewSecurityService(repo, DefaultSecurityConfig(), clock, nil)

	repo.On("CountRecentFailuresByIP", mock.Anything, mock.Anything, mock.Anything).
		Return(0, errors.New("db error"))

	blocked := svc.IsIPBlocked(context.Background(), "192.168.1.1")
	assert.False(t, blocked, "should fail open on database error")
}

func TestSecurityService_IsIdentifierBlocked_NotBlocked(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIdentifier", mock.Anything, "user@example.com", expectedSince).Return(3, nil)

	blocked := svc.IsIdentifierBlocked(context.Background(), "user@example.com")
	assert.False(t, blocked)
	repo.AssertExpectations(t)
}

func TestSecurityService_IsIdentifierBlocked_Blocked(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIdentifier", mock.Anything, "victim@example.com", expectedSince).Return(5, nil)

	blocked := svc.IsIdentifierBlocked(context.Background(), "victim@example.com")
	assert.True(t, blocked)
	repo.AssertExpectations(t)
}

func TestSecurityService_IsIdentifierBlocked_FailsOpen(t *testing.T) {
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewSecurityService(repo, DefaultSecurityConfig(), clock, nil)

	repo.On("CountRecentFailuresByIdentifier", mock.Anything, mock.Anything, mock.Anything).
		Return(0, errors.New("db error"))

	blocked := svc.IsIdentifierBlocked(context.Background(), "user@example.com")
	assert.False(t, blocked, "should fail open on database error")
}

func TestSecurityService_IsIdentifierBlocked_ExactThreshold(t *testing.T) {
	// Test that exactly at threshold (5) is blocked (>= comparison)
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIdentifier", mock.Anything, "user@example.com", expectedSince).Return(5, nil)

	blocked := svc.IsIdentifierBlocked(context.Background(), "user@example.com")
	assert.True(t, blocked, "should be blocked at exact threshold")
}

func TestSecurityService_IsIdentifierBlocked_BelowThreshold(t *testing.T) {
	// Test that one below threshold (4) is not blocked
	repo := new(mockSecurityRepo)
	clock := &mockClock{now: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)}
	config := DefaultSecurityConfig()
	svc := NewSecurityService(repo, config, clock, nil)

	expectedSince := clock.now.Add(-config.WindowDuration)
	repo.On("CountRecentFailuresByIdentifier", mock.Anything, "user@example.com", expectedSince).Return(4, nil)

	blocked := svc.IsIdentifierBlocked(context.Background(), "user@example.com")
	assert.False(t, blocked, "should not be blocked below threshold")
}

// --- BruteForceProtector Tests ---

type mockSecurityService struct {
	mock.Mock
}

func (m *mockSecurityService) RecordAttempt(ctx context.Context, eventType string, identifier string, ip string, success bool, reason string) error {
	args := m.Called(ctx, eventType, identifier, ip, success, reason)
	return args.Error(0)
}

func (m *mockSecurityService) IsIPBlocked(ctx context.Context, ip string) bool {
	args := m.Called(ctx, ip)
	return args.Bool(0)
}

func (m *mockSecurityService) IsIdentifierBlocked(ctx context.Context, identifier string) bool {
	args := m.Called(ctx, identifier)
	return args.Bool(0)
}

func TestBruteForceProtector_CheckLoginAllowed_AllClear(t *testing.T) {
	secSvc := new(mockSecurityService)
	protector := NewBruteForceProtector(secSvc)

	secSvc.On("IsIdentifierBlocked", mock.Anything, "user@example.com").Return(false)
	secSvc.On("IsIPBlocked", mock.Anything, "192.168.1.1").Return(false)

	allowed, err := protector.CheckLoginAllowed(context.Background(), "user@example.com", "192.168.1.1")
	require.NoError(t, err)
	assert.True(t, allowed)
	secSvc.AssertExpectations(t)
}

func TestBruteForceProtector_CheckLoginAllowed_IdentifierBlocked(t *testing.T) {
	secSvc := new(mockSecurityService)
	protector := NewBruteForceProtector(secSvc)

	secSvc.On("IsIdentifierBlocked", mock.Anything, "victim@example.com").Return(true)
	// IP check should NOT be called if identifier is already blocked

	allowed, err := protector.CheckLoginAllowed(context.Background(), "victim@example.com", "192.168.1.1")
	require.NoError(t, err)
	assert.False(t, allowed)
	secSvc.AssertNotCalled(t, "IsIPBlocked", mock.Anything, mock.Anything)
}

func TestBruteForceProtector_CheckLoginAllowed_IPBlocked(t *testing.T) {
	secSvc := new(mockSecurityService)
	protector := NewBruteForceProtector(secSvc)

	secSvc.On("IsIdentifierBlocked", mock.Anything, "user@example.com").Return(false)
	secSvc.On("IsIPBlocked", mock.Anything, "10.0.0.1").Return(true)

	allowed, err := protector.CheckLoginAllowed(context.Background(), "user@example.com", "10.0.0.1")
	require.NoError(t, err)
	assert.False(t, allowed)
	secSvc.AssertExpectations(t)
}

func TestBruteForceProtector_RecordAttempt_Failed(t *testing.T) {
	secSvc := new(mockSecurityService)
	protector := NewBruteForceProtector(secSvc)

	secSvc.On("RecordAttempt", mock.Anything, "login", "user@example.com", "192.168.1.1", false, "invalid_creds").Return(nil)

	err := protector.RecordAttempt(context.Background(), "user@example.com", "192.168.1.1", false)
	require.NoError(t, err)
	secSvc.AssertExpectations(t)
}

func TestBruteForceProtector_RecordAttempt_Successful(t *testing.T) {
	secSvc := new(mockSecurityService)
	protector := NewBruteForceProtector(secSvc)

	secSvc.On("RecordAttempt", mock.Anything, "login", "user@example.com", "192.168.1.1", true, "").Return(nil)

	err := protector.RecordAttempt(context.Background(), "user@example.com", "192.168.1.1", true)
	require.NoError(t, err)
	secSvc.AssertExpectations(t)
}

func TestDefaultSecurityConfig(t *testing.T) {
	config := DefaultSecurityConfig()
	assert.Equal(t, 100, config.IPBlockThreshold)
	assert.Equal(t, 5, config.IdentifierBlockThreshold)
	assert.Equal(t, 15*time.Minute, config.WindowDuration)
}

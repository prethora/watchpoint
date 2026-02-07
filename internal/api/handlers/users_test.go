package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations for User Handler
// =============================================================================

type mockUserRepo struct {
	getByIDFn          func(ctx context.Context, id, orgID string) (*types.User, error)
	getByEmailFn       func(ctx context.Context, email string) (*types.User, error)
	updateFn           func(ctx context.Context, user *types.User) error
	deleteFn           func(ctx context.Context, id, orgID string) error
	countOwnersFn      func(ctx context.Context, orgID string) (int, error)
	createInvitedFn    func(ctx context.Context, user *types.User, tokenHash string, expiresAt time.Time) error
	updateInviteTokenFn func(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	listByOrgFn        func(ctx context.Context, orgID string, role *types.UserRole, limit int, cursor string) ([]*types.User, error)

	capturedInvitedUser *types.User
}

func (m *mockUserRepo) GetByID(ctx context.Context, id, orgID string) (*types.User, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id, orgID)
	}
	return nil, types.NewAppError(types.ErrCodeNotFoundUser, "not found", nil)
}

func (m *mockUserRepo) GetByEmail(ctx context.Context, email string) (*types.User, error) {
	if m.getByEmailFn != nil {
		return m.getByEmailFn(ctx, email)
	}
	return nil, types.NewAppError(types.ErrCodeNotFoundUser, "not found", nil)
}

func (m *mockUserRepo) Update(ctx context.Context, user *types.User) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, user)
	}
	return nil
}

func (m *mockUserRepo) Delete(ctx context.Context, id, orgID string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id, orgID)
	}
	return nil
}

func (m *mockUserRepo) CountOwners(ctx context.Context, orgID string) (int, error) {
	if m.countOwnersFn != nil {
		return m.countOwnersFn(ctx, orgID)
	}
	return 2, nil // Default: 2 owners (safe for most tests)
}

func (m *mockUserRepo) CreateInvited(ctx context.Context, user *types.User, tokenHash string, expiresAt time.Time) error {
	m.capturedInvitedUser = user
	if m.createInvitedFn != nil {
		return m.createInvitedFn(ctx, user, tokenHash, expiresAt)
	}
	return nil
}

func (m *mockUserRepo) UpdateInviteToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	if m.updateInviteTokenFn != nil {
		return m.updateInviteTokenFn(ctx, userID, tokenHash, expiresAt)
	}
	return nil
}

func (m *mockUserRepo) ListByOrg(ctx context.Context, orgID string, role *types.UserRole, limit int, cursor string) ([]*types.User, error) {
	if m.listByOrgFn != nil {
		return m.listByOrgFn(ctx, orgID, role, limit, cursor)
	}
	return nil, nil
}

type mockUserSessionRepo struct {
	deleteByUserFn func(ctx context.Context, userID string) error
	deleteCalled   bool
	deletedUserID  string
}

func (m *mockUserSessionRepo) DeleteByUser(ctx context.Context, userID string) error {
	m.deleteCalled = true
	m.deletedUserID = userID
	if m.deleteByUserFn != nil {
		return m.deleteByUserFn(ctx, userID)
	}
	return nil
}

type mockUserEmailService struct {
	sendInviteFn func(ctx context.Context, toEmail, inviteURL string, role types.UserRole) error
	sendCalled   bool
	sentEmail    string
	sentRole     types.UserRole
}

func (m *mockUserEmailService) SendInvite(ctx context.Context, toEmail, inviteURL string, role types.UserRole) error {
	m.sendCalled = true
	m.sentEmail = toEmail
	m.sentRole = role
	if m.sendInviteFn != nil {
		return m.sendInviteFn(ctx, toEmail, inviteURL, role)
	}
	return nil
}

type mockUserAuditLogger struct {
	events []types.AuditEvent
}

func (m *mockUserAuditLogger) Log(ctx context.Context, event types.AuditEvent) error {
	m.events = append(m.events, event)
	return nil
}

// =============================================================================
// Test Helpers
// =============================================================================

func newTestUserHandler() (*UserHandler, *mockUserRepo, *mockUserSessionRepo, *mockUserEmailService, *mockUserAuditLogger) {
	userRepo := &mockUserRepo{}
	sessionRepo := &mockUserSessionRepo{}
	emailSvc := &mockUserEmailService{}
	audit := &mockUserAuditLogger{}
	logger := slog.Default()
	validator := core.NewValidator(logger)

	h := NewUserHandler(userRepo, sessionRepo, emailSvc, audit, validator, logger)
	return h, userRepo, sessionRepo, emailSvc, audit
}

func userContext(orgID string, actorID string, role types.UserRole) context.Context {
	ctx := context.Background()
	actor := types.Actor{
		ID:             actorID,
		Type:           types.ActorTypeUser,
		OrganizationID: orgID,
		Role:           role,
	}
	ctx = types.WithActor(ctx, actor)
	return ctx
}

// withURLParam creates a chi context with URL parameters.
func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// =============================================================================
// User Handler Tests: List
// =============================================================================

func TestUserHandler_List(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"

	testUsers := []*types.User{
		{
			ID:             "usr_1",
			OrganizationID: orgID,
			Email:          "user1@test.com",
			Name:           "User One",
			Role:           types.RoleOwner,
			Status:         types.UserStatusActive,
			CreatedAt:      time.Now().UTC(),
		},
		{
			ID:             "usr_2",
			OrganizationID: orgID,
			Email:          "user2@test.com",
			Name:           "User Two",
			Role:           types.RoleMember,
			Status:         types.UserStatusActive,
			CreatedAt:      time.Now().UTC(),
		},
	}

	userRepo.listByOrgFn = func(_ context.Context, oid string, role *types.UserRole, limit int, cursor string) ([]*types.User, error) {
		assert.Equal(t, orgID, oid)
		assert.Equal(t, 20, limit)
		return testUsers, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	req = req.WithContext(userContext(orgID, "usr_actor", types.RoleMember))
	w := httptest.NewRecorder()

	h.List(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []UserDTO          `json:"data"`
		Meta *types.ResponseMeta `json:"meta"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Data, 2)
	assert.Equal(t, "user1@test.com", resp.Data[0].Email)
}

func TestUserHandler_List_WithRoleFilter(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"

	userRepo.listByOrgFn = func(_ context.Context, _ string, role *types.UserRole, _ int, _ string) ([]*types.User, error) {
		require.NotNil(t, role)
		assert.Equal(t, types.RoleAdmin, *role)
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/users?role=admin", nil)
	req = req.WithContext(userContext(orgID, "usr_actor", types.RoleMember))
	w := httptest.NewRecorder()

	h.List(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUserHandler_List_InvalidRole(t *testing.T) {
	h, _, _, _, _ := newTestUserHandler()

	req := httptest.NewRequest(http.MethodGet, "/v1/users?role=invalid", nil)
	req = req.WithContext(userContext("org_1", "usr_actor", types.RoleMember))
	w := httptest.NewRecorder()

	h.List(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// User Handler Tests: Invite
// =============================================================================

func TestUserHandler_Invite_Success(t *testing.T) {
	h, userRepo, _, emailSvc, audit := newTestUserHandler()
	orgID := "org_test123"

	body := `{"email": "newuser@test.com", "role": "member"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/invite", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Invite(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	// Verify the invited user was created.
	require.NotNil(t, userRepo.capturedInvitedUser)
	assert.Equal(t, "newuser@test.com", userRepo.capturedInvitedUser.Email)
	assert.Equal(t, types.RoleMember, userRepo.capturedInvitedUser.Role)
	assert.Equal(t, types.UserStatusInvited, userRepo.capturedInvitedUser.Status)
	assert.Equal(t, orgID, userRepo.capturedInvitedUser.OrganizationID)

	// Verify email was sent.
	assert.True(t, emailSvc.sendCalled, "EmailService.SendInvite should have been called")
	assert.Equal(t, "newuser@test.com", emailSvc.sentEmail)
	assert.Equal(t, types.RoleMember, emailSvc.sentRole)

	// Verify audit event.
	require.Len(t, audit.events, 1)
	assert.Equal(t, "user.invited", audit.events[0].Action)
}

func TestUserHandler_Invite_DuplicateEmail_409(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"

	userRepo.createInvitedFn = func(_ context.Context, _ *types.User, _ string, _ time.Time) error {
		return types.NewAppError(types.ErrCodeConflictEmail, "user already exists", nil)
	}

	body := `{"email": "existing@test.com", "role": "member"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/invite", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Invite(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var errResp core.APIErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "User already exists", errResp.Error.Message)
}

func TestUserHandler_Invite_EmailServiceFails_500(t *testing.T) {
	h, _, _, emailSvc, _ := newTestUserHandler()
	orgID := "org_test123"

	// Per USER-004: Return 500 if email provider fails so admin knows the invite didn't go out.
	emailSvc.sendInviteFn = func(_ context.Context, _, _ string, _ types.UserRole) error {
		return types.NewAppError(types.ErrCodeUpstreamEmailProvider, "email provider failed", nil)
	}

	body := `{"email": "newuser@test.com", "role": "admin"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/invite", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Invite(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestUserHandler_Invite_InvalidEmail(t *testing.T) {
	h, _, _, _, _ := newTestUserHandler()

	body := `{"email": "not-an-email", "role": "member"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/invite", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext("org_1", "usr_admin", types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Invite(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUserHandler_Invite_InvalidRole(t *testing.T) {
	h, _, _, _, _ := newTestUserHandler()

	body := `{"email": "user@test.com", "role": "superadmin"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/invite", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext("org_1", "usr_admin", types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Invite(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// User Handler Tests: ResendInvite
// =============================================================================

func TestUserHandler_ResendInvite_Success(t *testing.T) {
	h, userRepo, _, emailSvc, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_invited"

	userRepo.getByIDFn = func(_ context.Context, id, oid string) (*types.User, error) {
		return &types.User{
			ID:             id,
			OrganizationID: oid,
			Email:          "invited@test.com",
			Role:           types.RoleMember,
			Status:         types.UserStatusInvited,
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/users/"+targetID+"/resend-invite", nil)
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.ResendInvite(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, emailSvc.sendCalled)
	assert.Equal(t, "invited@test.com", emailSvc.sentEmail)
}

func TestUserHandler_ResendInvite_NotInvitedState(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_active"

	userRepo.getByIDFn = func(_ context.Context, id, oid string) (*types.User, error) {
		return &types.User{
			ID:     id,
			Status: types.UserStatusActive, // Not invited
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/users/"+targetID+"/resend-invite", nil)
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.ResendInvite(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// User Handler Tests: UpdateRole
// =============================================================================

func TestUserHandler_UpdateRole_OwnerCanPromoteToOwner(t *testing.T) {
	h, userRepo, _, _, audit := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:             targetID,
			OrganizationID: orgID,
			Email:          "admin@test.com",
			Role:           types.RoleAdmin, // Currently admin
			Status:         types.UserStatusActive,
		}, nil
	}

	body := `{"role": "owner"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/"+targetID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_owner", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.UpdateRole(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, audit.events, 1)
	assert.Equal(t, "user.role_changed", audit.events[0].Action)
}

func TestUserHandler_UpdateRole_AdminCannotPromoteToOwner(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleMember,
		}, nil
	}

	body := `{"role": "owner"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/"+targetID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.UpdateRole(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserHandler_UpdateRole_AdminCannotDemoteOwner(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleOwner, // Target is an owner
		}, nil
	}

	body := `{"role": "admin"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/"+targetID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_admin", types.RoleAdmin))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.UpdateRole(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserHandler_UpdateRole_LastOwnerCannotBeDemoted(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleOwner,
		}, nil
	}

	// Only 1 owner exists.
	userRepo.countOwnersFn = func(_ context.Context, _ string) (int, error) {
		return 1, nil
	}

	body := `{"role": "admin"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/"+targetID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_owner_actor", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.UpdateRole(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)

	var errResp core.APIErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp.Error.Message, "last owner")
}

func TestUserHandler_UpdateRole_NoChange(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleMember,
		}, nil
	}

	// Same role -> 200 OK with no mutation.
	body := `{"role": "member"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/"+targetID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userContext(orgID, "usr_owner", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.UpdateRole(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// User Handler Tests: Delete
// =============================================================================

func TestUserHandler_Delete_OwnerCanDeleteMember(t *testing.T) {
	h, userRepo, sessionRepo, _, audit := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_target"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:             targetID,
			OrganizationID: orgID,
			Role:           types.RoleMember,
			Status:         types.UserStatusActive,
		}, nil
	}

	deleteCalled := false
	userRepo.deleteFn = func(_ context.Context, id, oid string) error {
		assert.Equal(t, targetID, id)
		assert.Equal(t, orgID, oid)
		deleteCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+targetID, nil)
	req = req.WithContext(userContext(orgID, "usr_owner", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, deleteCalled)
	assert.True(t, sessionRepo.deleteCalled)
	assert.Equal(t, targetID, sessionRepo.deletedUserID)
	require.Len(t, audit.events, 1)
	assert.Equal(t, "user.removed", audit.events[0].Action)
}

func TestUserHandler_Delete_SelfDeletion(t *testing.T) {
	h, userRepo, sessionRepo, _, audit := newTestUserHandler()
	orgID := "org_test123"
	actorID := "usr_self"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:             actorID,
			OrganizationID: orgID,
			Role:           types.RoleMember,
		}, nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+actorID, nil)
	req = req.WithContext(userContext(orgID, actorID, types.RoleMember))
	req = withURLParam(req, "id", actorID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, sessionRepo.deleteCalled)
	require.Len(t, audit.events, 1)
	assert.Equal(t, "user.left_organization", audit.events[0].Action)
}

func TestUserHandler_Delete_LastOwnerCannotBeDeleted(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_last_owner"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleOwner,
		}, nil
	}

	userRepo.countOwnersFn = func(_ context.Context, _ string) (int, error) {
		return 1, nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+targetID, nil)
	req = req.WithContext(userContext(orgID, "usr_other_owner", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserHandler_Delete_AdminCanOnlyDeleteMembers(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_admin_target"

	// Target is an Admin.
	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleAdmin,
		}, nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+targetID, nil)
	req = req.WithContext(userContext(orgID, "usr_admin_actor", types.RoleAdmin))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserHandler_Delete_MemberCannotDeleteOthers(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_other_member"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return &types.User{
			ID:   targetID,
			Role: types.RoleMember,
		}, nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+targetID, nil)
	req = req.WithContext(userContext(orgID, "usr_member_actor", types.RoleMember))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserHandler_Delete_UserNotFound(t *testing.T) {
	h, userRepo, _, _, _ := newTestUserHandler()
	orgID := "org_test123"
	targetID := "usr_nonexistent"

	userRepo.getByIDFn = func(_ context.Context, _, _ string) (*types.User, error) {
		return nil, types.NewAppError(types.ErrCodeNotFoundUser, "user not found", nil)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+targetID, nil)
	req = req.WithContext(userContext(orgID, "usr_owner", types.RoleOwner))
	req = withURLParam(req, "id", targetID)
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// Token Generation Test
// =============================================================================

func TestGenerateInviteToken(t *testing.T) {
	rawToken, tokenHash, err := generateInviteToken()
	require.NoError(t, err)

	// Token should be 64 hex characters (32 bytes).
	assert.Len(t, rawToken, 64)

	// Hash should be 64 hex characters (SHA-256 = 32 bytes).
	assert.Len(t, tokenHash, 64)

	// Token and hash must be different.
	assert.NotEqual(t, rawToken, tokenHash)

	// Generate another token -- they should be unique.
	rawToken2, _, err := generateInviteToken()
	require.NoError(t, err)
	assert.NotEqual(t, rawToken, rawToken2)
}

func TestToUserDTO(t *testing.T) {
	now := time.Now().UTC()
	user := &types.User{
		ID:     "usr_1",
		Email:  "test@example.com",
		Name:   "Test User",
		Role:   types.RoleAdmin,
		Status: types.UserStatusActive,
		LastLoginAt: &now,
	}

	dto := toUserDTO(user)

	assert.Equal(t, "usr_1", dto.ID)
	assert.Equal(t, "test@example.com", dto.Email)
	assert.Equal(t, "Test User", dto.Name)
	assert.Equal(t, types.RoleAdmin, dto.Role)
	assert.Equal(t, "active", dto.Status)
	assert.NotNil(t, dto.LastLoginAt)
	assert.Nil(t, dto.InviteExpiresAt)
}

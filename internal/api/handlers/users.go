// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the User handler as defined in 05d-api-organization.md
// Section 4. It covers:
//   - Listing organization members
//   - Inviting users with secure token generation
//   - Resending invites
//   - Role transitions with RBAC enforcement
//   - User deletion with cascading session invalidation
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- Service Interfaces ---

// UserRepo defines the data access contract for user operations used by
// the user handler.
// Mirrors the concrete db.UserRepository methods relevant to this handler.
type UserRepo interface {
	GetByID(ctx context.Context, id string, orgID string) (*types.User, error)
	GetByEmail(ctx context.Context, email string) (*types.User, error)
	Update(ctx context.Context, user *types.User) error
	Delete(ctx context.Context, id string, orgID string) error
	CountOwners(ctx context.Context, orgID string) (int, error)
	CreateInvited(ctx context.Context, user *types.User, tokenHash string, expiresAt time.Time) error
	UpdateInviteToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error
	ListByOrg(ctx context.Context, orgID string, role *types.UserRole, limit int, cursor string) ([]*types.User, error)
}

// UserSessionRepo provides session invalidation for user deletion.
// Per 05d-api-organization.md Section 2.2.
type UserSessionRepo interface {
	DeleteByUser(ctx context.Context, userID string) error
}

// UserEmailService abstracts email delivery for the invite flow.
// Per 05d-api-organization.md Section 2.1.
type UserEmailService interface {
	SendInvite(ctx context.Context, toEmail string, inviteURL string, role types.UserRole) error
}

// UserAuditLogger defines the audit logging contract for user operations.
type UserAuditLogger interface {
	Log(ctx context.Context, event types.AuditEvent) error
}

// --- Request/Response Models ---
// Per 05d-api-organization.md Section 4.2.

// ListUsersParams defines parameters for listing users.
type ListUsersParams struct {
	Role   *types.UserRole `json:"role,omitempty"`
	Limit  int             `json:"limit"`
	Cursor string          `json:"cursor,omitempty"`
}

// ListUsersResponse wraps user DTOs with pagination info.
type ListUsersResponse struct {
	Data     []UserDTO      `json:"data"`
	PageInfo types.PageInfo `json:"pagination"`
}

// UserDTO is the safe response for user listing.
type UserDTO struct {
	ID              string         `json:"id"`
	Email           string         `json:"email"`
	Name            string         `json:"name,omitempty"`
	Role            types.UserRole `json:"role"`
	Status          string         `json:"status"`
	InviteExpiresAt *time.Time     `json:"invite_expires_at,omitempty"`
	LastLoginAt     *time.Time     `json:"last_login_at,omitempty"`
}

// InviteUserRequest is the request body for POST /v1/users/invite.
// Per 05d-api-organization.md Section 4.2.
type InviteUserRequest struct {
	Email string         `json:"email" validate:"required,email"`
	Role  types.UserRole `json:"role" validate:"required,oneof=owner admin member"`
}

// UpdateUserRequest is the request body for PATCH /v1/users/{id}.
// Per 05d-api-organization.md Section 4.2.
type UpdateUserRequest struct {
	Role *types.UserRole `json:"role" validate:"required,oneof=owner admin member"`
}

// --- Constants ---

const (
	// inviteTokenLength is the number of random bytes for invite tokens.
	inviteTokenLength = 32

	// inviteExpiryDuration is the default invite expiry (7 days).
	inviteExpiryDuration = 7 * 24 * time.Hour

	// inviteURLTemplate is the base URL template for invite links.
	// The actual base URL should come from configuration. For now, we use
	// a placeholder that can be overridden.
	inviteURLTemplate = "/auth/accept-invite?token=%s"
)

// --- Handler ---

// UserHandler manages user lifecycle, invites, and RBAC enforcement.
// Per 05d-api-organization.md Section 4.1.
type UserHandler struct {
	userRepo     UserRepo
	sessionRepo  UserSessionRepo
	emailService UserEmailService
	audit        UserAuditLogger
	validator    *core.Validator
	logger       *slog.Logger
}

// NewUserHandler creates a new UserHandler with the provided dependencies.
func NewUserHandler(
	userRepo UserRepo,
	sessionRepo UserSessionRepo,
	emailSvc UserEmailService,
	audit UserAuditLogger,
	v *core.Validator,
	l *slog.Logger,
) *UserHandler {
	if l == nil {
		l = slog.Default()
	}
	return &UserHandler{
		userRepo:     userRepo,
		sessionRepo:  sessionRepo,
		emailService: emailSvc,
		audit:        audit,
		validator:    v,
		logger:       l,
	}
}

// --- Handler Methods ---

// List handles GET /v1/users.
// Returns paginated list of organization members.
// Per 05d-api-organization.md Section 4.3.
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Parse query parameters.
	params := ListUsersParams{
		Limit: 20,
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 1 || limit > 100 {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"limit must be a number between 1 and 100",
				nil,
			))
			return
		}
		params.Limit = limit
	}

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		params.Cursor = cursor
	}

	if roleStr := r.URL.Query().Get("role"); roleStr != "" {
		role := types.UserRole(roleStr)
		switch role {
		case types.RoleOwner, types.RoleAdmin, types.RoleMember:
			params.Role = &role
		default:
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"role must be one of: owner, admin, member",
				nil,
			))
			return
		}
	}

	users, err := h.userRepo.ListByOrg(r.Context(), orgID, params.Role, params.Limit, params.Cursor)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Build pagination info. Repo returns up to limit+1 rows for cursor detection.
	pageInfo := types.PageInfo{}
	if len(users) > params.Limit {
		pageInfo.HasMore = true
		users = users[:params.Limit]
	}

	// Convert to DTOs.
	data := make([]UserDTO, 0, len(users))
	for _, u := range users {
		data = append(data, toUserDTO(u))
	}

	// Set cursor to created_at of last item.
	if pageInfo.HasMore && len(data) > 0 {
		lastUser := users[len(users)-1]
		pageInfo.NextCursor = lastUser.CreatedAt.Format(time.RFC3339Nano)
	}

	resp := core.APIResponse{
		Data: data,
		Meta: &types.ResponseMeta{
			Pagination: &pageInfo,
		},
	}

	core.JSON(w, r, http.StatusOK, resp)
}

// Invite handles POST /v1/users/invite.
//
// Per USER-004 flow simulation and 05d-api-organization.md Section 4.4:
//  1. Validate input (email format, role).
//  2. Generate secure invite token.
//  3. Hash token (SHA-256 for storage).
//  4. Call repo.CreateInvited (catch unique constraint -> 409).
//  5. Call EmailService.SendInvite (return 500 if fails).
//  6. Emit audit event.
//  7. Return 201 Created.
func (h *UserHandler) Invite(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, _ := types.GetActor(r.Context())

	var req InviteUserRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Generate secure invite token.
	rawToken, tokenHash, err := generateInviteToken()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to generate invite token",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to generate invite token",
			err,
		))
		return
	}

	// Step 3: Create invited user.
	userID := "usr_" + uuid.New().String()
	expiresAt := time.Now().UTC().Add(inviteExpiryDuration)
	user := &types.User{
		ID:             userID,
		OrganizationID: orgID,
		Email:          req.Email,
		Role:           req.Role,
		Status:         types.UserStatusInvited,
		CreatedAt:      time.Now().UTC(),
	}

	// Step 4: Insert into DB.
	if err := h.userRepo.CreateInvited(r.Context(), user, tokenHash, expiresAt); err != nil {
		// Check for 409 Conflict (email already exists).
		var appErr *types.AppError
		if errors.As(err, &appErr) && appErr.Code == types.ErrCodeConflictEmail {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeConflictEmail,
				"User already exists",
				nil,
			))
			return
		}
		core.Error(w, r, err)
		return
	}

	// Step 5: Send invite email.
	// Per USER-004: Return 500 if SendGrid fails so admin knows the invite didn't go out.
	inviteURL := fmt.Sprintf(inviteURLTemplate, rawToken)
	if h.emailService != nil {
		if err := h.emailService.SendInvite(r.Context(), req.Email, inviteURL, req.Role); err != nil {
			h.logger.ErrorContext(r.Context(), "failed to send invite email",
				"email", req.Email,
				"error", err,
			)
			core.Error(w, r, types.NewAppError(
				types.ErrCodeInternalUnexpected,
				"failed to send invite email",
				err,
			))
			return
		}
	}

	// Step 6: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "user.invited", userID, "user")

	// Step 7: Return 201 Created.
	resp := toUserDTO(user)
	resp.InviteExpiresAt = &expiresAt

	core.JSON(w, r, http.StatusCreated, core.APIResponse{Data: resp})
}

// ResendInvite handles POST /v1/users/{id}/resend-invite.
//
// Per 05d-api-organization.md Section 4.4 item 2:
//  1. Regenerate token for existing invited user.
//  2. Update expiry.
//  3. Resend email.
func (h *UserHandler) ResendInvite(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, _ := types.GetActor(r.Context())

	userID := chi.URLParam(r, "id")
	if userID == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"user ID is required",
			nil,
		))
		return
	}

	// Verify user exists and is in invited state.
	user, err := h.userRepo.GetByID(r.Context(), userID, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	if user.Status != types.UserStatusInvited {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"user is not in invited state",
			nil,
		))
		return
	}

	// Generate new token.
	rawToken, tokenHash, err := generateInviteToken()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to generate invite token for resend",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to generate invite token",
			err,
		))
		return
	}

	// Update the token and expiry.
	expiresAt := time.Now().UTC().Add(inviteExpiryDuration)
	if err := h.userRepo.UpdateInviteToken(r.Context(), userID, tokenHash, expiresAt); err != nil {
		core.Error(w, r, err)
		return
	}

	// Resend email.
	inviteURL := fmt.Sprintf(inviteURLTemplate, rawToken)
	if h.emailService != nil {
		if err := h.emailService.SendInvite(r.Context(), user.Email, inviteURL, user.Role); err != nil {
			h.logger.ErrorContext(r.Context(), "failed to resend invite email",
				"email", user.Email,
				"error", err,
			)
			core.Error(w, r, types.NewAppError(
				types.ErrCodeInternalUnexpected,
				"failed to resend invite email",
				err,
			))
			return
		}
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "user.invite_resent", userID, "user")

	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: map[string]string{"message": "Invite resent successfully"},
	})
}

// UpdateRole handles PATCH /v1/users/{id}.
//
// Per 05d-api-organization.md Section 4.4 item 3 (Role Transition Matrix):
//   - Admin cannot promote to Owner.
//   - Admin cannot demote Owner.
//   - Owner can perform any transition.
//   - Must check last-owner constraint when demoting an owner.
func (h *UserHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"user ID is required",
			nil,
		))
		return
	}

	var req UpdateUserRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Fetch the target user.
	target, err := h.userRepo.GetByID(r.Context(), targetID, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	newRole := *req.Role

	// No change needed.
	if target.Role == newRole {
		core.JSON(w, r, http.StatusOK, core.APIResponse{Data: toUserDTO(target)})
		return
	}

	// Enforce role transition matrix.
	if err := h.validateRoleTransition(r.Context(), actor, target, newRole, orgID); err != nil {
		core.Error(w, r, err)
		return
	}

	// Apply the role change.
	target.Role = newRole
	if err := h.userRepo.Update(r.Context(), target); err != nil {
		core.Error(w, r, err)
		return
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "user.role_changed", targetID, "user")

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: toUserDTO(target)})
}

// Delete handles DELETE /v1/users/{id}.
//
// Per 05d-api-organization.md Section 4.4 item 4 (Deletion Constraints):
//   - Self-Deletion: Allowed ("Leave Org").
//   - Last Owner: CountOwners check prevents removing the last Owner.
//   - Cascading: Transactionally invalidates sessions.
//   - API Keys remain active (ON DELETE SET NULL on created_by_user_id).
func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"user ID is required",
			nil,
		))
		return
	}

	// Fetch the target user to verify existence and check constraints.
	target, err := h.userRepo.GetByID(r.Context(), targetID, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	isSelfDeletion := actor.ID == targetID

	// Permission checks:
	// - Self-deletion is always allowed.
	// - Owner can delete any user.
	// - Admin can only delete Members.
	if !isSelfDeletion {
		if !actor.RoleHasAtLeast(types.RoleAdmin) {
			core.Error(w, r, types.NewAppError(
				types.ErrCodePermissionRole,
				"insufficient permissions to delete this user",
				nil,
			))
			return
		}
		// Admin cannot delete Owners or other Admins.
		if actor.Role == types.RoleAdmin && target.Role != types.RoleMember {
			core.Error(w, r, types.NewAppError(
				types.ErrCodePermissionRole,
				"Admin can only remove Members",
				nil,
			))
			return
		}
	}

	// Last-owner check: prevent removing/deleting the last owner.
	if target.Role == types.RoleOwner {
		ownerCount, err := h.userRepo.CountOwners(r.Context(), orgID)
		if err != nil {
			core.Error(w, r, err)
			return
		}
		if ownerCount <= 1 {
			core.Error(w, r, types.NewAppError(
				types.ErrCodePermissionRole,
				"cannot remove the last owner of the organization",
				nil,
			))
			return
		}
	}

	// Cascade: invalidate all sessions for the target user.
	if h.sessionRepo != nil {
		if err := h.sessionRepo.DeleteByUser(r.Context(), targetID); err != nil {
			h.logger.WarnContext(r.Context(), "failed to invalidate sessions during user deletion",
				"user_id", targetID,
				"error", err,
			)
			// Continue with deletion even if session cleanup fails.
		}
	}

	// Soft-delete the user.
	if err := h.userRepo.Delete(r.Context(), targetID, orgID); err != nil {
		core.Error(w, r, err)
		return
	}

	// Emit audit event.
	action := "user.removed"
	if isSelfDeletion {
		action = "user.left_organization"
	}
	h.emitAuditEvent(r.Context(), actor, action, targetID, "user")

	// Return 204 No Content.
	w.WriteHeader(http.StatusNoContent)
}

// --- Role Transition Validation ---

// validateRoleTransition enforces the role transition matrix per
// 05d-api-organization.md Section 4.4 item 3.
func (h *UserHandler) validateRoleTransition(ctx context.Context, actor types.Actor, target *types.User, newRole types.UserRole, orgID string) error {
	// Admin cannot promote to Owner.
	if actor.Role == types.RoleAdmin && newRole == types.RoleOwner {
		return types.NewAppError(
			types.ErrCodePermissionRole,
			"Admin cannot promote users to Owner",
			nil,
		)
	}

	// Admin cannot demote/modify Owners.
	if actor.Role == types.RoleAdmin && target.Role == types.RoleOwner {
		return types.NewAppError(
			types.ErrCodePermissionRole,
			"Admin cannot modify Owner roles",
			nil,
		)
	}

	// Non-owners cannot change roles (Members can't change roles at all).
	if !actor.RoleHasAtLeast(types.RoleAdmin) {
		return types.NewAppError(
			types.ErrCodePermissionRole,
			"insufficient permissions to change user roles",
			nil,
		)
	}

	// Last-owner protection when demoting an owner.
	if target.Role == types.RoleOwner && newRole != types.RoleOwner {
		ownerCount, err := h.userRepo.CountOwners(ctx, orgID)
		if err != nil {
			return err
		}
		if ownerCount <= 1 {
			return types.NewAppError(
				types.ErrCodePermissionRole,
				"cannot demote the last owner of the organization",
				nil,
			)
		}
	}

	return nil
}

// --- Helper Functions ---

// generateInviteToken generates a cryptographically secure random token
// and returns both the raw token (for the email link) and its SHA-256 hash
// (for database storage).
func generateInviteToken() (rawToken string, tokenHash string, err error) {
	randomBytes := make([]byte, inviteTokenLength)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", fmt.Errorf("crypto/rand read failed: %w", err)
	}

	rawToken = hex.EncodeToString(randomBytes)

	// Hash with SHA-256 for storage. We use SHA-256 rather than bcrypt for
	// invite tokens because: (1) tokens are high-entropy random strings
	// (not passwords), (2) lookup is by hash (SELECT WHERE hash = $1),
	// and (3) bcrypt comparison requires the plaintext which makes DB
	// lookups impractical without a separate identifier.
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash = hex.EncodeToString(hash[:])

	return rawToken, tokenHash, nil
}

// toUserDTO converts a types.User to the safe UserDTO response.
func toUserDTO(u *types.User) UserDTO {
	return UserDTO{
		ID:              u.ID,
		Email:           u.Email,
		Name:            u.Name,
		Role:            u.Role,
		Status:          string(u.Status),
		InviteExpiresAt: u.InviteExpiresAt,
		LastLoginAt:     u.LastLoginAt,
	}
}

// emitAuditEvent logs an audit event. Errors are logged but not propagated
// to avoid failing the primary operation due to audit log failures.
func (h *UserHandler) emitAuditEvent(ctx context.Context, actor types.Actor, action, resourceID, resourceType string) {
	if h.audit == nil {
		return
	}

	event := types.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		Timestamp:    time.Now().UTC(),
	}

	if err := h.audit.Log(ctx, event); err != nil {
		h.logger.WarnContext(ctx, "failed to log audit event",
			"action", action,
			"resource_id", resourceID,
			"error", err,
		)
	}
}

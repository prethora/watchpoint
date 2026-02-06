package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"golang.org/x/crypto/bcrypt"

	"watchpoint/internal/types"
)

// bcryptCost is the bcrypt cost factor used for password hashing.
// Per 05f-api-auth.md Section 7.1: bcrypt with cost 12.
const bcryptCost = 12

// UserRepo defines the data access methods needed by AuthService for user
// operations. This mirrors the auth-specific UserRepository defined in
// 05f-api-auth.md Section 4.1, plus additional methods required by the
// login and accept-invite flows.
type UserRepo interface {
	GetByEmail(ctx context.Context, email string) (*types.User, error)
	GetByInviteTokenHash(ctx context.Context, tokenHash string) (*types.User, error)
	UpdatePassword(ctx context.Context, userID string, newHash string) error
	UpdateLastLogin(ctx context.Context, userID string) error
	UpdateStatus(ctx context.Context, userID string, status types.UserStatus, name string, passwordHash string) error
}

// AuthTxManager abstracts transactional execution for the AuthService.
// The callback receives transaction-scoped repositories for user and session
// operations, ensuring all writes within the callback participate in the
// same database transaction.
type AuthTxManager interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context, txUserRepo UserRepo, txSessionRepo SessionRepo) error) error
}

// PasswordHasher abstracts bcrypt operations for testability.
type PasswordHasher interface {
	CompareHashAndPassword(hashedPassword, password string) error
	GenerateFromPassword(password string) (string, error)
}

// bcryptHasher is the production implementation of PasswordHasher.
type bcryptHasher struct{}

func (b *bcryptHasher) CompareHashAndPassword(hashedPassword, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
}

func (b *bcryptHasher) GenerateFromPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// HashToken produces a hex-encoded SHA-256 hash of a raw token string.
// Used for invite tokens and password reset tokens where the hash must be
// searchable in the database (unlike bcrypt which is salted and non-searchable).
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// authService implements the AuthService interface defined in
// 05f-api-auth.md Section 4.2.
//
// Dependencies (all injected via AuthServiceConfig):
//   - UserRepo: For user lookup (GetByEmail, GetByInviteTokenHash) outside transactions
//   - SessionService: For session creation (token generation, session struct building)
//   - SecurityService: For brute force tracking (RecordAttempt on success/failure)
//   - AuthTxManager: For transactional execution of login and accept-invite flows
//   - PasswordHasher: For bcrypt password verification and generation
//   - Clock: For testable time (invite expiry checks)
type authService struct {
	userRepo   UserRepo
	sessionSvc *sessionService
	security   types.SecurityService
	txManager  AuthTxManager
	hasher     PasswordHasher
	clock      types.Clock
	logger     *slog.Logger
}

// AuthServiceConfig holds the dependencies for creating an AuthService.
type AuthServiceConfig struct {
	UserRepo       UserRepo
	SessionService *sessionService
	Security       types.SecurityService
	TxManager      AuthTxManager
	Hasher         PasswordHasher
	Clock          types.Clock
	Logger         *slog.Logger
}

// NewAuthService creates a new AuthService implementation.
// If Hasher is nil, the production bcryptHasher is used.
// If Clock is nil, RealClock is used.
// If Logger is nil, slog.Default() is used.
func NewAuthService(cfg AuthServiceConfig) *authService {
	hasher := cfg.Hasher
	if hasher == nil {
		hasher = &bcryptHasher{}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = types.RealClock{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &authService{
		userRepo:   cfg.UserRepo,
		sessionSvc: cfg.SessionService,
		security:   cfg.Security,
		txManager:  cfg.TxManager,
		hasher:     hasher,
		clock:      clock,
		logger:     logger,
	}
}

// Login verifies credentials and creates a session within a transaction.
//
// Per USER-006 flow simulation and 05f-api-auth.md Section 4.2:
//  1. Fetch user by email (outside transaction).
//  2. Verify password hash (bcrypt). If invalid, record failure and return ErrAuthInvalidCreds.
//  3. Check user status is 'active'. If not, return ErrAuthAccountNotActive.
//  4. Start DB transaction:
//     a. Update last_login_at.
//     b. Create session.
//     c. Delete expired sessions for the user (lazy cleanup per DASH-001).
//  5. Record success attempt via SecurityService.
//  6. Return user and session.
//
// Security Event Recording: On both success and failure, calls
// SecurityService.RecordAttempt(ctx, "login", email, ip, success, reason).
//
// Enumeration Protection: Returns generic "Invalid email or password" for both
// user-not-found and invalid-password cases (Section 7.4).
func (s *authService) Login(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error) {
	// Step 1: Fetch user by email
	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		// Mask user-not-found as invalid creds for enumeration protection (Section 7.4)
		if appErr, ok := err.(*types.AppError); ok && appErr.Code == types.ErrCodeAuthUserNotFound {
			_ = s.security.RecordAttempt(ctx, "login", email, ip, false, "user_not_found")
			return nil, nil, types.NewAppError(types.ErrCodeAuthInvalidCreds, "invalid email or password", nil)
		}
		// ErrOrgDeleted and other errors pass through as-is
		return nil, nil, err
	}

	// Step 2: Verify password hash
	if err := s.hasher.CompareHashAndPassword(user.PasswordHash, password); err != nil {
		_ = s.security.RecordAttempt(ctx, "login", email, ip, false, "invalid_creds")
		return nil, nil, types.NewAppError(types.ErrCodeAuthInvalidCreds, "invalid email or password", nil)
	}

	// Step 3: Check user status is active
	if user.Status != types.UserStatusActive {
		_ = s.security.RecordAttempt(ctx, "login", email, ip, false, "account_not_active")
		return nil, nil, types.NewAppError(types.ErrCodeAuthAccountNotActive, "account not active", nil)
	}

	// Step 4: Transactional operations
	var session *types.Session
	err = s.txManager.RunInTx(ctx, func(txCtx context.Context, txUserRepo UserRepo, txSessionRepo SessionRepo) error {
		// 4a. Update last_login_at (USER-006 step 7)
		if updateErr := txUserRepo.UpdateLastLogin(txCtx, user.ID); updateErr != nil {
			return updateErr
		}

		// 4b. Create session (USER-006 step 8)
		// Use a transaction-scoped session service that writes to the tx connection
		txSessionSvc := s.sessionSvc.withRepo(txSessionRepo)
		sess, _, createErr := txSessionSvc.CreateSession(txCtx, user.ID, user.OrganizationID, ip, "")
		if createErr != nil {
			return createErr
		}
		session = sess

		// 4c. Lazy session cleanup - delete expired sessions for this user (DASH-001)
		if cleanupErr := txSessionRepo.DeleteExpiredByUser(txCtx, user.ID); cleanupErr != nil {
			// Log but don't fail the login for cleanup errors.
			// The scheduled job will catch orphaned sessions.
			s.logger.Warn("failed to clean expired sessions during login",
				"user_id", user.ID,
				"error", cleanupErr,
			)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Step 5: Record successful login attempt
	_ = s.security.RecordAttempt(ctx, "login", email, ip, true, "")

	s.logger.Info("user logged in",
		"user_id", user.ID,
		"email", email,
	)

	return user, session, nil
}

// AcceptInvite validates the invite token, sets user password, and activates
// the account within a single database transaction.
//
// Per USER-005 flow simulation and 05f-api-auth.md Section 4.2 / 7.2:
//  1. Hash the raw token (SHA-256) and look up the user by invite_token_hash.
//  2. Verify the invite has not expired.
//  3. Hash the new password (bcrypt).
//  4. Start DB transaction:
//     a. Update user: set password_hash, name, status='active', clear invite_token_hash.
//     b. Create session.
//  5. If any step in the transaction fails, rollback to keep the invite
//     token valid for retry (AcceptInvite Atomicity per Section 7.2).
func (s *authService) AcceptInvite(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error) {
	// Step 1: Hash the raw token and look up the user
	tokenHash := HashToken(token)
	user, err := s.userRepo.GetByInviteTokenHash(ctx, tokenHash)
	if err != nil {
		return nil, nil, err
	}

	// Step 2: Verify invite hasn't expired
	if user.InviteExpiresAt != nil && s.clock.Now().After(*user.InviteExpiresAt) {
		return nil, nil, types.NewAppError(types.ErrCodeAuthTokenExpired, "invite token has expired", nil)
	}

	// Step 3: Hash the new password
	passwordHash, err := s.hasher.GenerateFromPassword(password)
	if err != nil {
		return nil, nil, types.NewAppError(types.ErrCodeInternalUnexpected, "failed to hash password", err)
	}

	// Step 4: Execute atomically within a single transaction
	var session *types.Session
	err = s.txManager.RunInTx(ctx, func(txCtx context.Context, txUserRepo UserRepo, txSessionRepo SessionRepo) error {
		// 4a. Update user: status -> active, set name/password, clear invite token
		// Per USER-005 step 4: UPDATE users SET password_hash=$1, status='active',
		//   name=$2, invite_token_hash=NULL WHERE id=$3
		if updateErr := txUserRepo.UpdateStatus(txCtx, user.ID, types.UserStatusActive, name, passwordHash); updateErr != nil {
			return updateErr
		}

		// 4b. Create session (USER-005 step 5)
		txSessionSvc := s.sessionSvc.withRepo(txSessionRepo)
		sess, _, createErr := txSessionSvc.CreateSession(txCtx, user.ID, user.OrganizationID, ip, "")
		if createErr != nil {
			return createErr
		}
		session = sess

		return nil
	})
	if err != nil {
		// Transaction rolled back - invite token remains valid for retry
		return nil, nil, err
	}

	// Update the returned user object to reflect the committed changes
	user.Status = types.UserStatusActive
	user.Name = name
	user.InviteTokenHash = ""
	user.InviteExpiresAt = nil

	s.logger.Info("invite accepted",
		"user_id", user.ID,
		"email", user.Email,
	)

	return user, session, nil
}

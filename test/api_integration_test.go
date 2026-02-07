//go:build integration

// Package test contains integration tests that exercise the full API stack
// against a real PostgreSQL database running in Docker. These tests are
// skipped by default during `go test ./...` and must be run explicitly
// with the integration build tag:
//
//	go test -v -tags integration ./test/
//
// Prerequisites:
//   - Docker PostgreSQL running on localhost:5432
//   - Migrations applied (see migrations/ directory)
//   - DATABASE_URL set or default postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"watchpoint/internal/api/handlers"
	"watchpoint/internal/auth"
	"watchpoint/internal/billing"
	"watchpoint/internal/config"
	"watchpoint/internal/core"
	"watchpoint/internal/db"
	"watchpoint/internal/external"
	"watchpoint/internal/forecasts"
	"watchpoint/internal/types"
)

// testDBURL returns the database URL for integration tests.
// Falls back to a sensible default for local Docker-based development.
func testDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable"
}

// connectTestDB attempts to connect to the test database.
// Returns nil pool and skips the test if the database is unavailable.
func connectTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(testDBURL())
	if err != nil {
		t.Skipf("skipping integration test: cannot parse DB URL: %v", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("skipping integration test: cannot create pool: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping integration test: database not available: %v", err)
	}

	// Verify the schema exists by checking for a known table.
	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_name = 'organizations'
		)`,
	).Scan(&exists)
	if err != nil || !exists {
		pool.Close()
		t.Skipf("skipping integration test: schema not applied (organizations table missing)")
	}

	return pool
}

// cleanupTestData removes all test data from the database.
// Called before and after each test to ensure isolation.
func cleanupTestData(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// Delete in dependency order to respect foreign key constraints.
	tables := []string{
		"notification_deliveries",
		"notifications",
		"watchpoint_evaluation_state",
		"watchpoints",
		"sessions",
		"api_keys",
		"audit_log",
		"security_events",
		"usage_history",
		"rate_limits",
		"users",
		"organizations",
	}
	for _, table := range tables {
		_, err := pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s", table))
		if err != nil {
			// Table might not exist in all migration states; log and continue.
			t.Logf("cleanup: failed to delete from %s: %v", table, err)
		}
	}
}

// sessionAuthenticator resolves session tokens by looking them up in the
// database. This provides realistic authentication in integration tests.
type sessionAuthenticator struct {
	sessionRepo *db.SessionRepository
	userRepo    *db.UserRepository
}

func (a *sessionAuthenticator) ResolveToken(ctx context.Context, token string) (*types.Actor, error) {
	if token == "" {
		return nil, types.NewAppError(types.ErrCodeAuthTokenMissing, "no token provided", nil)
	}

	// Strip "Bearer " prefix if present.
	token = strings.TrimPrefix(token, "Bearer ")

	// Look for session token (prefixed with "sess_").
	if strings.HasPrefix(token, "sess_") {
		session, err := a.sessionRepo.GetByID(ctx, token)
		if err != nil {
			return nil, types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid session", err)
		}
		if time.Now().After(session.ExpiresAt) {
			return nil, types.NewAppError(types.ErrCodeAuthSessionExpired, "session expired", nil)
		}

		// Look up the user to get their role.
		user, err := a.userRepo.GetByID(ctx, session.UserID, session.OrganizationID)
		if err != nil {
			return nil, types.NewAppError(types.ErrCodeAuthTokenInvalid, "user not found", err)
		}

		return &types.Actor{
			ID:             user.ID,
			Type:           types.ActorTypeUser,
			OrganizationID: session.OrganizationID,
			Role:           user.Role,
			Scopes:         types.RoleScopeMap[user.Role],
			Source:         "session",
		}, nil
	}

	return nil, types.NewAppError(types.ErrCodeAuthTokenInvalid, "unsupported token type", nil)
}

// noopRateLimitStore always allows requests.
type noopRateLimitStore struct{}

func (s *noopRateLimitStore) IncrementAndCheck(_ context.Context, _ string, _ int, _ time.Duration) (core.RateLimitResult, error) {
	return core.RateLimitResult{Allowed: true, Remaining: 1000, ResetAt: time.Now().Add(time.Hour)}, nil
}

// noopIdempotencyStore does nothing.
type noopIdempotencyStore struct{}

func (s *noopIdempotencyStore) Get(_ context.Context, _ string, _ string) (*core.IdempotencyRecord, error) {
	return nil, nil
}
func (s *noopIdempotencyStore) Create(_ context.Context, _, _, _ string) error                     { return nil }
func (s *noopIdempotencyStore) Complete(_ context.Context, _, _ string, _ int, _ []byte) error     { return nil }
func (s *noopIdempotencyStore) Fail(_ context.Context, _, _ string) error                          { return nil }

// noopMetricsCollector does nothing.
type noopMetricsCollector struct{}

func (s *noopMetricsCollector) RecordRequest(_, _, _ string, _ time.Duration) {}

// noopForecastService returns errors for all forecast operations.
type noopForecastService struct{}

func (s *noopForecastService) GetPointForecast(_ context.Context, _, _ float64, _, _ time.Time) (*forecasts.ForecastResponse, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "not available", nil)
}
func (s *noopForecastService) GetBatchForecast(_ context.Context, _ forecasts.BatchForecastRequest) (*forecasts.BatchForecastResult, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "not available", nil)
}
func (s *noopForecastService) GetVariables(_ context.Context) ([]forecasts.VariableResponseMetadata, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "not available", nil)
}
func (s *noopForecastService) GetStatus(_ context.Context) (*forecasts.SystemStatus, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "not available", nil)
}
func (s *noopForecastService) GetSnapshot(_ context.Context, _, _ float64) (*types.ForecastSnapshot, error) {
	return nil, nil // graceful degradation per WPForecastProvider contract
}
func (s *noopForecastService) GetVerificationMetrics(_ context.Context, _ string, _, _ time.Time) (*forecasts.VerificationReport, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "not available", nil)
}

// auditAdapter bridges db.AuditRepository to handlers interfaces.
type auditAdapter struct {
	repo *db.AuditRepository
}

func (a *auditAdapter) Log(ctx context.Context, event types.AuditEvent) error {
	return a.repo.Log(ctx, &event)
}

// wpAuditAdapter bridges db.AuditRepository to handlers.WPAuditLogger.
type wpAuditAdapter struct {
	repo *db.AuditRepository
}

func (a *wpAuditAdapter) Log(ctx context.Context, entry *types.AuditEvent) error {
	return a.repo.Log(ctx, entry)
}

// orgNotifAdapter converts pointer slice to value slice.
type orgNotifAdapter struct {
	repo *db.NotificationRepository
}

func (a *orgNotifAdapter) List(ctx context.Context, filter types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error) {
	items, pi, err := a.repo.List(ctx, filter)
	if err != nil {
		return nil, pi, err
	}
	result := make([]types.NotificationHistoryItem, len(items))
	for i, item := range items {
		if item != nil {
			result[i] = *item
		}
	}
	return result, pi, nil
}

// orgWPPauseAdapter bridges PauseAllByOrgID with default reason.
type orgWPPauseAdapter struct {
	repo *db.WatchPointRepository
}

func (a *orgWPPauseAdapter) PauseAllByOrgID(ctx context.Context, orgID string) error {
	return a.repo.PauseAllByOrgID(ctx, orgID, "org_deletion")
}

// apiKeyRepoAdapter bridges the handler's param type to db's param type.
type apiKeyRepoAdapter struct {
	repo *db.APIKeyRepository
}

func (a *apiKeyRepoAdapter) List(ctx context.Context, orgID string, params handlers.APIKeyListParams) ([]*types.APIKey, error) {
	return a.repo.List(ctx, orgID, db.ListAPIKeysParams{
		ActiveOnly: params.ActiveOnly, Prefix: params.Prefix, Limit: params.Limit, Cursor: params.Cursor,
	})
}
func (a *apiKeyRepoAdapter) Create(ctx context.Context, key *types.APIKey) error {
	return a.repo.Create(ctx, key)
}
func (a *apiKeyRepoAdapter) GetByID(ctx context.Context, id string, orgID string) (*types.APIKey, error) {
	return a.repo.GetByID(ctx, id, orgID)
}
func (a *apiKeyRepoAdapter) Delete(ctx context.Context, id string, orgID string) error {
	return a.repo.Delete(ctx, id, orgID)
}
func (a *apiKeyRepoAdapter) Rotate(ctx context.Context, oldKeyID string, orgID string, newKey *types.APIKey, graceEnd time.Time) error {
	return a.repo.Rotate(ctx, oldKeyID, orgID, newKey, graceEnd)
}
func (a *apiKeyRepoAdapter) CountRecentByUser(ctx context.Context, userID string, since time.Time) (int, error) {
	return a.repo.CountRecentByUser(ctx, userID, since)
}

// oauthAdapter bridges external.OAuthManager to handlers.OAuthManager.
type oauthAdapter struct {
	inner external.OAuthManager
}

func (a *oauthAdapter) GetProvider(name string) (handlers.OAuthProvider, error) {
	return a.inner.GetProvider(name)
}

// emailSvcStub implements handlers.UserEmailService as a no-op.
type emailSvcStub struct{}

func (s *emailSvcStub) SendInvite(_ context.Context, _, _ string, _ types.UserRole) error {
	return nil
}

// orgBillingStub implements handlers.OrgBillingService as a no-op.
type orgBillingStub struct {
	svc external.BillingService
}

func (s *orgBillingStub) EnsureCustomer(ctx context.Context, orgID string, email string) (string, error) {
	return s.svc.EnsureCustomer(ctx, orgID, email)
}
func (s *orgBillingStub) CancelSubscription(_ context.Context, _ string) error {
	return nil
}

// authServiceAdapter wraps the real auth service for handler consumption.
type authServiceAdapter struct {
	real interface {
		Login(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error)
		AcceptInvite(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error)
	}
}

func (a *authServiceAdapter) Login(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error) {
	return a.real.Login(ctx, email, password, ip)
}
func (a *authServiceAdapter) AcceptInvite(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error) {
	return a.real.AcceptInvite(ctx, token, name, password, ip)
}
func (a *authServiceAdapter) RequestPasswordReset(_ context.Context, _ string) error {
	return nil
}
func (a *authServiceAdapter) CompletePasswordReset(_ context.Context, _, _ string) error {
	return types.NewAppError(types.ErrCodeInternalUnexpected, "not implemented", nil)
}

// pgAuthTxManager implements auth.AuthTxManager using pgxpool transactions.
type pgAuthTxManager struct {
	pool *pgxpool.Pool
}

func (m *pgAuthTxManager) RunInTx(ctx context.Context, fn func(ctx context.Context, txUserRepo auth.UserRepo, txSessionRepo auth.SessionRepo) error) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to begin transaction", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txUserRepo := db.NewUserRepository(tx)
	txSessionRepo := db.NewSessionRepository(tx)

	if err := fn(ctx, txUserRepo, txSessionRepo); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// pgRepoRegistry implements types.RepositoryRegistry.
type pgRepoRegistry struct {
	wp    *db.WatchPointRepository
	org   *db.OrganizationRepository
	user  *db.UserRepository
	notif *db.NotificationRepository
	pool  *pgxpool.Pool
}

func (r *pgRepoRegistry) WatchPoints() types.WatchPointRepository     { return r.wp }
func (r *pgRepoRegistry) Organizations() types.OrganizationRepository { return r.org }
func (r *pgRepoRegistry) Users() types.UserRepository                 { return r.user }
func (r *pgRepoRegistry) Notifications() types.NotificationRepository { return r.notif }
func (r *pgRepoRegistry) Close() error                                { r.pool.Close(); return nil }

// buildIntegrationServer creates a fully wired server with real DB repositories
// and a session-based authenticator for integration testing.
func buildIntegrationServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()

	setIntegrationEnv(t)

	cfg, err := config.LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Repositories
	userRepo := db.NewUserRepository(pool)
	orgRepo := db.NewOrganizationRepository(pool)
	sessionRepo := db.NewSessionRepository(pool)
	securityRepo := db.NewSecurityRepository(pool)
	watchpointRepo := db.NewWatchPointRepository(pool)
	notificationRepo := db.NewNotificationRepository(pool)
	auditRepo := db.NewAuditRepository(pool)
	apiKeyRepo := db.NewAPIKeyRepository(pool)
	usageDBImpl := db.NewUsageDBImpl(pool)
	usageHistoryRepo := db.NewUsageHistoryRepo(pool)

	repos := &pgRepoRegistry{
		wp:    watchpointRepo,
		org:   orgRepo,
		user:  userRepo,
		notif: notificationRepo,
		pool:  pool,
	}

	// Services
	securitySvc := auth.NewSecurityService(securityRepo, auth.DefaultSecurityConfig(), nil, logger)
	tokenGen := auth.NewCryptoTokenGenerator()
	sessionSvc := auth.NewSessionService(sessionRepo, tokenGen, auth.DefaultSessionConfig(), nil, logger)
	txManager := &pgAuthTxManager{pool: pool}
	realAuthSvc := auth.NewAuthService(auth.AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessionSvc,
		Security:       securitySvc,
		TxManager:      txManager,
		Logger:         logger,
	})
	planRegistry := billing.NewStaticPlanRegistry()
	usageReporter := billing.NewUsageReporter(usageDBImpl, usageDBImpl, usageHistoryRepo, planRegistry)

	stubBilling := external.NewStubBillingService(logger)
	stubOAuth := &oauthAdapter{inner: external.NewStubOAuthManager(logger, "google")}
	forecastSvc := &noopForecastService{}

	// Server
	srv, err := core.NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Use a session-based authenticator for realistic auth testing.
	srv.SecurityService = securitySvc
	srv.Authenticator = &sessionAuthenticator{sessionRepo: sessionRepo, userRepo: userRepo}
	srv.RateLimitStore = &noopRateLimitStore{}
	srv.IdempotencyStore = &noopIdempotencyStore{}
	srv.Metrics = &noopMetricsCollector{}

	// Wire handlers
	audit := &auditAdapter{repo: auditRepo}

	authHandler := handlers.NewAuthHandler(
		&authServiceAdapter{real: realAuthSvc},
		sessionSvc,
		stubOAuth,
		handlers.DefaultCookieConfig(),
		logger,
		srv.Validator,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		r.Route("/auth", authHandler.RegisterRoutes)
	})

	forecastHandler := handlers.NewForecastHandler(forecastSvc, srv.Validator, logger)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		r.Route("/forecasts", forecastHandler.RegisterRoutes)
	})

	usageHandler := handlers.NewUsageHandler(usageReporter, orgRepo, srv.Validator)
	billingHandler := handlers.NewBillingHandler(stubBilling, orgRepo, usageHandler, audit, cfg, srv.Validator, logger)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, billingHandler.RegisterRoutes)

	orgHandler := handlers.NewOrganizationHandler(
		orgRepo,
		&orgWPPauseAdapter{repo: watchpointRepo},
		&orgNotifAdapter{repo: notificationRepo},
		auditRepo,
		userRepo,
		planRegistry,
		&orgBillingStub{svc: stubBilling},
		srv.Validator,
		logger,
	)
	userHandler := handlers.NewUserHandler(userRepo, sessionRepo, &emailSvcStub{}, audit, srv.Validator, logger)
	apiKeyHandler := handlers.NewAPIKeyHandler(&apiKeyRepoAdapter{repo: apiKeyRepo}, audit, logger)

	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		orgHandler.RegisterRoutes(r, userHandler, apiKeyHandler)
	})

	wpAudit := &wpAuditAdapter{repo: auditRepo}
	wpHandler := handlers.NewWatchPointHandler(
		watchpointRepo, notificationRepo, srv.Validator, logger,
		forecastSvc, usageReporter, wpAudit, nil,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, wpHandler.RegisterRoutes)

	srv.MountRoutes()

	return httptest.NewServer(srv.Handler())
}

// setIntegrationEnv sets environment variables for the integration test config.
func setIntegrationEnv(t *testing.T) {
	t.Helper()

	t.Setenv("APP_ENV", "local")
	t.Setenv("PORT", "0") // not used by httptest.Server
	t.Setenv("API_EXTERNAL_URL", "http://localhost:8080")
	t.Setenv("DASHBOARD_URL", "http://localhost:3000")
	t.Setenv("DATABASE_URL", testDBURL())
	t.Setenv("FORECAST_BUCKET", "watchpoint-forecasts-test")
	t.Setenv("SQS_EVAL_URGENT", "http://localhost:4566/000000000000/eval-queue-urgent")
	t.Setenv("SQS_EVAL_STANDARD", "http://localhost:4566/000000000000/eval-queue-standard")
	t.Setenv("SQS_NOTIFICATIONS", "http://localhost:4566/000000000000/notification-queue")
	t.Setenv("SQS_DLQ", "http://localhost:4566/000000000000/dead-letter-queue-shared")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_integration")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_integration")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_integration")
	t.Setenv("SES_REGION", "us-east-1")
	t.Setenv("RUNPOD_API_KEY", "rp_integration")
	t.Setenv("RUNPOD_ENDPOINT_ID", "integration-endpoint")
	t.Setenv("SESSION_KEY", "integration-test-session-key-minimum-32-chars!!")
	t.Setenv("ADMIN_API_KEY", "integration-admin-key")
}

// TestIntegration_SignupLoginCreateGetWatchPoint exercises the core user journey:
// 1. Create an organization and user via direct DB setup (simulating signup)
// 2. Login via POST /v1/auth/login
// 3. Extract session token from cookie
// 4. Create a WatchPoint via POST /v1/watchpoints (authenticated)
// 5. Get the WatchPoint via GET /v1/watchpoints/{id} (authenticated)
// 6. Verify all status codes and that data persisted correctly.
func TestIntegration_SignupLoginCreateGetWatchPoint(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	cleanupTestData(t, pool)
	defer cleanupTestData(t, pool)

	ts := buildIntegrationServer(t, pool)
	defer ts.Close()

	client := ts.Client()
	// Do not follow redirects automatically.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	ctx := context.Background()

	// =====================================================================
	// Step 0: Verify health endpoint works
	// =====================================================================
	resp := doRequest(t, client, "GET", ts.URL+"/health", "", nil)
	assertStatus(t, resp, http.StatusOK)
	t.Log("Health endpoint OK")

	// =====================================================================
	// Step 1: Create organization and user directly in DB (simulating signup)
	// =====================================================================
	orgID := "org_inttest_001"
	userID := "usr_inttest_001"
	userEmail := "integration@watchpoint.test"
	userPassword := "SecureP@ssw0rd123"

	// Hash the password with bcrypt.
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(userPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	// Get default plan limits.
	planRegistry := billing.NewStaticPlanRegistry()
	freeLimits := planRegistry.GetLimits(types.PlanFree)
	limitsJSON, _ := json.Marshal(freeLimits)

	// Insert organization.
	_, err = pool.Exec(ctx,
		`INSERT INTO organizations (id, name, billing_email, plan, plan_limits, notification_preferences, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, '{}', NOW(), NOW())`,
		orgID, "Integration Test Org", userEmail, string(types.PlanFree), string(limitsJSON),
	)
	if err != nil {
		t.Fatalf("failed to insert org: %v", err)
	}
	t.Logf("Created organization: %s", orgID)

	// Insert user.
	_, err = pool.Exec(ctx,
		`INSERT INTO users (id, organization_id, email, password_hash, role, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())`,
		userID, orgID, userEmail, string(passwordHash), string(types.RoleOwner), string(types.UserStatusActive),
	)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}
	t.Logf("Created user: %s (%s)", userID, userEmail)

	// =====================================================================
	// Step 2: Login via POST /v1/auth/login
	// =====================================================================
	loginBody := fmt.Sprintf(`{"email":"%s","password":"%s"}`, userEmail, userPassword)
	resp = doRequest(t, client, "POST", ts.URL+"/v1/auth/login", "", []byte(loginBody))
	assertStatus(t, resp, http.StatusOK)

	// Extract session cookie.
	var sessionID string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "session_id" {
			sessionID = cookie.Value
			break
		}
	}
	if sessionID == "" {
		t.Fatal("login response did not include session_id cookie")
	}
	t.Logf("Login successful, session_id: %s...", sessionID[:20])

	// Parse the auth response to verify structure.
	var authResp struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
			ExpiresAt string `json:"expires_at"`
			User      struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
	}
	parseResponse(t, resp, &authResp)
	if authResp.Data.CSRFToken == "" {
		t.Error("expected non-empty CSRF token in login response")
	}
	if authResp.Data.User.Email != userEmail {
		t.Errorf("login user email: got %q, want %q", authResp.Data.User.Email, userEmail)
	}
	t.Log("Login response verified")

	// =====================================================================
	// Step 3: Create a WatchPoint via POST /v1/watchpoints
	// =====================================================================
	createWPBody := `{
		"name": "Integration Test WatchPoint",
		"location": {
			"latitude": 40.7128,
			"longitude": -74.0060
		},
		"timezone": "America/New_York",
		"time_window": {
			"start": "2026-06-01T00:00:00Z",
			"end": "2026-06-30T23:59:59Z"
		},
		"conditions": [{
			"variable": "precipitation_probability",
			"operator": "gt",
			"threshold": 80
		}],
		"channels": {
			"email": {"enabled": true}
		}
	}`

	resp = doRequest(t, client, "POST", ts.URL+"/v1/watchpoints", sessionID, []byte(createWPBody))
	assertStatus(t, resp, http.StatusCreated)

	// Parse the create response.
	var createResp struct {
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Status   string `json:"status"`
			Location struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"location"`
		} `json:"data"`
	}
	parseResponse(t, resp, &createResp)
	wpID := createResp.Data.ID
	if wpID == "" {
		t.Fatal("created WatchPoint has empty ID")
	}
	if createResp.Data.Name != "Integration Test WatchPoint" {
		t.Errorf("WatchPoint name: got %q, want %q", createResp.Data.Name, "Integration Test WatchPoint")
	}
	t.Logf("Created WatchPoint: %s (status: %s)", wpID, createResp.Data.Status)

	// =====================================================================
	// Step 4: Get the WatchPoint via GET /v1/watchpoints/{id}
	// =====================================================================
	resp = doRequest(t, client, "GET", ts.URL+"/v1/watchpoints/"+wpID, sessionID, nil)
	assertStatus(t, resp, http.StatusOK)

	var getResp struct {
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Status   string `json:"status"`
			Location struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"location"`
			OrganizationID string `json:"organization_id"`
		} `json:"data"`
	}
	parseResponse(t, resp, &getResp)

	if getResp.Data.ID != wpID {
		t.Errorf("GET WatchPoint ID: got %q, want %q", getResp.Data.ID, wpID)
	}
	if getResp.Data.Name != "Integration Test WatchPoint" {
		t.Errorf("GET WatchPoint name: got %q, want %q", getResp.Data.Name, "Integration Test WatchPoint")
	}
	if getResp.Data.OrganizationID != orgID {
		t.Errorf("GET WatchPoint org_id: got %q, want %q", getResp.Data.OrganizationID, orgID)
	}
	t.Logf("Get WatchPoint verified: %s", wpID)

	// =====================================================================
	// Step 5: Verify database side-effects
	// =====================================================================

	// Verify session exists in DB.
	var sessionCount int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sessions WHERE user_id = $1`, userID,
	).Scan(&sessionCount)
	if err != nil {
		t.Fatalf("failed to count sessions: %v", err)
	}
	if sessionCount < 1 {
		t.Error("expected at least 1 session in database after login")
	}
	t.Logf("Database verified: %d session(s) for user", sessionCount)

	// Verify WatchPoint exists in DB with correct org.
	var dbWPOrgID string
	err = pool.QueryRow(ctx,
		`SELECT organization_id FROM watchpoints WHERE id = $1`, wpID,
	).Scan(&dbWPOrgID)
	if err != nil {
		t.Fatalf("failed to query WatchPoint from DB: %v", err)
	}
	if dbWPOrgID != orgID {
		t.Errorf("DB WatchPoint org_id: got %q, want %q", dbWPOrgID, orgID)
	}
	t.Log("Database side-effects verified")
}

// =============================================================================
// Test Helpers
// =============================================================================

// doRequest creates and executes an HTTP request.
// If sessionID is non-empty, it's sent both as an Authorization Bearer header
// (for the AuthMiddleware) and as a session_id cookie (for cookie-based auth).
func doRequest(t *testing.T, client *http.Client, method, url, sessionID string, body []byte) *http.Response {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("failed to create %s %s request: %v", method, url, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if sessionID != "" {
		// AuthMiddleware expects the token in the Authorization header.
		req.Header.Set("Authorization", "Bearer "+sessionID)
		// Also send as cookie for any handler-level cookie extraction (e.g., logout).
		req.AddCookie(&http.Cookie{
			Name:  "session_id",
			Value: sessionID,
		})
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, url, err)
	}

	return resp
}

// assertStatus checks that the response has the expected status code.
// On failure, it logs the response body for debugging.
func assertStatus(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		body, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(body)) // re-wrap for subsequent reads
		t.Fatalf("expected status %d, got %d; body: %s", expected, resp.StatusCode, string(body))
	}
}

// parseResponse reads and unmarshals the JSON response body into v.
func parseResponse(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body)) // re-wrap
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("failed to unmarshal response: %v; body: %s", err, string(body))
	}
}

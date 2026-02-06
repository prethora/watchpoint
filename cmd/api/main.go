// Package main is the entry point for the WatchPoint API server.
//
// It initializes the configuration, creates temporary stub dependencies, builds
// the HTTP server with the core chassis (middleware, routing, health checks),
// and starts listening for requests.
//
// In local mode (APP_ENV=local), it runs as a standard HTTP server on the
// configured port. In Lambda mode, it will use the chiadapter to bridge API
// Gateway events to the chi router (to be wired when the Lambda adapter
// dependency is added).
//
// Graceful shutdown is handled via OS signal interception (SIGINT, SIGTERM).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/api/handlers"
	"watchpoint/internal/billing"
	"watchpoint/internal/config"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// run encapsulates the startup lifecycle so that main() can cleanly exit on error.
func run() error {
	// Load configuration. For local development, pass nil as the SecretProvider
	// since SSM resolution is bypassed when APP_ENV=local.
	cfg, err := config.LoadConfig(nil)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	// Initialize structured logger.
	logger := newLogger(cfg.LogLevel)
	logger.Info("watchpoint API starting",
		"environment", cfg.Environment,
		"version", cfg.Build.Version,
		"commit", cfg.Build.Commit,
		"port", cfg.Server.Port,
	)

	// Create temporary stub dependencies.
	// These will be replaced with real implementations as subsequent phases
	// deliver database repositories, auth services, etc.
	repos := &stubRepositoryRegistry{}
	securityService := &stubSecurityService{}
	authenticator := &stubAuthenticator{}
	rateLimitStore := &stubRateLimitStore{}
	idempotencyStore := &stubIdempotencyStore{}
	metrics := &stubMetricsCollector{}

	// Build the server.
	srv, err := core.NewServer(cfg, repos, logger)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	// Inject stub dependencies that are not part of the NewServer constructor.
	srv.SecurityService = securityService
	srv.Authenticator = authenticator
	srv.RateLimitStore = rateLimitStore
	srv.IdempotencyStore = idempotencyStore
	srv.Metrics = metrics

	// Wire the Auth handler with stub services.
	// When real auth/session services are available (from Phase 5 Task 1-2),
	// they will replace the stub auth/session services here.
	stubAuthSvc := &stubAuthHandlerService{}
	stubSessSvc := &stubSessionHandlerService{}
	authHandler := handlers.NewAuthHandler(
		stubAuthSvc,
		stubSessSvc,
		nil, // OAuthManager - not wired yet
		handlers.DefaultCookieConfig(),
		logger,
		srv.Validator,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		r.Route("/auth", authHandler.RegisterRoutes)
	})

	// Wire the Billing handler with stub services.
	// When real billing/usage services are available (from Phase 3-5),
	// they will replace these stubs.
	stubBillingSvc := &stubBillingHandlerService{}
	stubOrgBillingRepo := &stubOrgBillingReader{}
	stubUsageRep := &stubUsageReporterService{}
	stubAudit := &stubAuditLoggerService{}

	usageHandler := handlers.NewUsageHandler(stubUsageRep, stubOrgBillingRepo, srv.Validator)
	billingHandler := handlers.NewBillingHandler(
		stubBillingSvc,
		stubOrgBillingRepo,
		usageHandler,
		stubAudit,
		cfg,
		srv.Validator,
		logger,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, billingHandler.RegisterRoutes)

	// Wire the Organization + User handlers with stub services.
	// When real repository and service implementations are available, they will
	// replace these stubs.
	stubOrgRepo := &stubOrgHandlerRepo{}
	stubOrgWPRepo := &stubOrgWatchPointRepo{}
	stubOrgNotifRepo := &stubOrgNotificationRepo{}
	stubOrgAuditRepo := &stubOrgAuditRepo{}
	stubOrgUserRepo := &stubOrgUserRepo{}
	stubOrgBillingSvc := &stubOrgBillingServiceImpl{}
	planRegistry := billing.NewStaticPlanRegistry()

	orgHandler := handlers.NewOrganizationHandler(
		stubOrgRepo,
		stubOrgWPRepo,
		stubOrgNotifRepo,
		stubOrgAuditRepo,
		stubOrgUserRepo,
		planRegistry,
		stubOrgBillingSvc,
		srv.Validator,
		logger,
	)

	stubUserRepo := &stubUserHandlerRepo{}
	stubUserSessionRepo := &stubUserSessionRepo{}
	stubEmailSvc := &stubEmailServiceImpl{}

	userHandler := handlers.NewUserHandler(
		stubUserRepo,
		stubUserSessionRepo,
		stubEmailSvc,
		stubAudit,
		srv.Validator,
		logger,
	)

	// Wire the API Key handler (already exists from previous task).
	stubAPIKeyRepo := &stubAPIKeyHandlerRepo{}
	apiKeyHandler := handlers.NewAPIKeyHandler(stubAPIKeyRepo, stubAudit, logger)

	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		orgHandler.RegisterRoutes(r, userHandler, apiKeyHandler)
	})

	// Mount all routes (middleware chain + versioned endpoints + health).
	srv.MountRoutes()

	// Determine if running in Lambda mode.
	// AWS Lambda sets AWS_LAMBDA_RUNTIME_API when executing inside the runtime.
	if isLambdaEnvironment() {
		return runLambda(srv, logger)
	}

	return runHTTPServer(srv, cfg, logger)
}

// isLambdaEnvironment returns true if the process is running inside AWS Lambda.
func isLambdaEnvironment() bool {
	_, hasRuntimeAPI := os.LookupEnv("AWS_LAMBDA_RUNTIME_API")
	_, hasServerPort := os.LookupEnv("_LAMBDA_SERVER_PORT")
	return hasRuntimeAPI || hasServerPort
}

// runLambda starts the server in AWS Lambda mode using the chi adapter.
// This is a placeholder that will be implemented when the aws-lambda-go-api-proxy
// dependency is added.
func runLambda(srv *core.Server, logger *slog.Logger) error {
	// TODO: Wire chiadapter.New(srv.Handler()) and lambda.Start() when the
	// aws-lambda-go and aws-lambda-go-api-proxy dependencies are added.
	logger.Error("Lambda mode is not yet implemented; run with APP_ENV=local for HTTP mode")
	return fmt.Errorf("lambda mode not yet implemented")
}

// runHTTPServer starts the server in standard HTTP mode with graceful shutdown.
func runHTTPServer(srv *core.Server, cfg *config.Config, logger *slog.Logger) error {
	addr := ":" + cfg.Server.Port

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Channel to capture server errors from ListenAndServe.
	serverErr := make(chan error, 1)

	go func() {
		logger.Info("HTTP server listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// Wait for shutdown signal or server error.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-shutdown:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
	}

	// Graceful shutdown with a 10-second deadline.
	logger.Info("initiating graceful shutdown")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}

	// Shutdown server resources (DB pools, etc.).
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server resource shutdown error", "error", err)
		return fmt.Errorf("server shutdown: %w", err)
	}

	logger.Info("server stopped cleanly")
	return nil
}

// newLogger creates a structured slog.Logger configured for the given log level.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: false,
	})
	return slog.New(handler)
}

// =============================================================================
// Temporary Stub Dependencies
//
// These stubs satisfy the interfaces required by core.Server until real
// implementations are delivered in later phases. They are intentionally minimal
// and will be removed when the real dependencies are wired.
// =============================================================================

// stubRepositoryRegistry implements types.RepositoryRegistry with no-op repositories.
type stubRepositoryRegistry struct{}

func (s *stubRepositoryRegistry) WatchPoints() types.WatchPointRepository     { return &stubWatchPointRepo{} }
func (s *stubRepositoryRegistry) Organizations() types.OrganizationRepository { return &stubOrganizationRepo{} }
func (s *stubRepositoryRegistry) Users() types.UserRepository                 { return &stubUserRepo{} }
func (s *stubRepositoryRegistry) Notifications() types.NotificationRepository { return &stubNotificationRepo{} }

type stubWatchPointRepo struct{}
type stubOrganizationRepo struct{}
type stubUserRepo struct{}
type stubNotificationRepo struct{}

// stubSecurityService implements types.SecurityService with permissive defaults.
type stubSecurityService struct{}

func (s *stubSecurityService) RecordAttempt(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (s *stubSecurityService) IsIPBlocked(_ context.Context, _ string) bool        { return false }
func (s *stubSecurityService) IsIdentifierBlocked(_ context.Context, _ string) bool { return false }

// stubAuthenticator implements core.Authenticator that returns a system actor
// for any token, allowing the server to start without real auth infrastructure.
type stubAuthenticator struct{}

func (s *stubAuthenticator) ResolveToken(_ context.Context, _ string) (*types.Actor, error) {
	return &types.Actor{
		ID:             "system_stub",
		Type:           types.ActorTypeSystem,
		OrganizationID: "org_stub",
		Source:         "stub",
	}, nil
}

// stubRateLimitStore implements core.RateLimitStore that always allows requests.
type stubRateLimitStore struct{}

func (s *stubRateLimitStore) IncrementAndCheck(_ context.Context, _ string, _ int, _ time.Duration) (core.RateLimitResult, error) {
	return core.RateLimitResult{
		Allowed:   true,
		Remaining: 1000,
		ResetAt:   time.Now().Add(time.Hour),
	}, nil
}

// stubIdempotencyStore implements core.IdempotencyStore with no persistence.
type stubIdempotencyStore struct{}

func (s *stubIdempotencyStore) Get(_ context.Context, _ string, _ string) (*core.IdempotencyRecord, error) {
	return nil, nil
}
func (s *stubIdempotencyStore) Create(_ context.Context, _, _, _ string) error        { return nil }
func (s *stubIdempotencyStore) Complete(_ context.Context, _, _ string, _ int, _ []byte) error { return nil }
func (s *stubIdempotencyStore) Fail(_ context.Context, _, _ string) error             { return nil }

// stubMetricsCollector implements core.MetricsCollector with no-op recording.
type stubMetricsCollector struct{}

func (s *stubMetricsCollector) RecordRequest(_, _, _ string, _ time.Duration) {}

// stubAuthHandlerService implements handlers.AuthService with error responses.
// This stub returns ErrCodeInternalUnexpected for all operations, indicating that
// real auth service implementations need to be wired. It allows the server to start
// and serve requests while returning meaningful errors.
type stubAuthHandlerService struct{}

func (s *stubAuthHandlerService) Login(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
	return nil, nil, types.NewAppError(types.ErrCodeAuthInvalidCreds, "auth service not configured", nil)
}

func (s *stubAuthHandlerService) AcceptInvite(_ context.Context, _, _, _, _ string) (*types.User, *types.Session, error) {
	return nil, nil, types.NewAppError(types.ErrCodeInternalUnexpected, "auth service not configured", nil)
}

func (s *stubAuthHandlerService) RequestPasswordReset(_ context.Context, _ string) error {
	return nil // Swallowed by handler per enumeration protection
}

func (s *stubAuthHandlerService) CompletePasswordReset(_ context.Context, _, _ string) error {
	return types.NewAppError(types.ErrCodeInternalUnexpected, "auth service not configured", nil)
}

// stubSessionHandlerService implements handlers.SessionService with no-op behavior.
type stubSessionHandlerService struct{}

func (s *stubSessionHandlerService) InvalidateSession(_ context.Context, _ string) error {
	return nil
}

// stubBillingHandlerService implements handlers.BillingService with error responses.
type stubBillingHandlerService struct{}

func (s *stubBillingHandlerService) EnsureCustomer(_ context.Context, _, _ string) (string, error) {
	return "", types.NewAppError(types.ErrCodeInternalUnexpected, "billing service not configured", nil)
}

func (s *stubBillingHandlerService) CreateCheckoutSession(_ context.Context, _ string, _ types.PlanTier, _ types.RedirectURLs) (string, string, error) {
	return "", "", types.NewAppError(types.ErrCodeInternalUnexpected, "billing service not configured", nil)
}

func (s *stubBillingHandlerService) CreatePortalSession(_ context.Context, _, _ string) (string, error) {
	return "", types.NewAppError(types.ErrCodeInternalUnexpected, "billing service not configured", nil)
}

func (s *stubBillingHandlerService) GetInvoices(_ context.Context, _ string, _ types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error) {
	return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalUnexpected, "billing service not configured", nil)
}

func (s *stubBillingHandlerService) GetSubscription(_ context.Context, _ string) (*types.SubscriptionDetails, error) {
	return nil, types.NewAppError(types.ErrCodeInternalUnexpected, "billing service not configured", nil)
}

// stubOrgBillingReader implements handlers.OrgBillingReader with error responses.
type stubOrgBillingReader struct{}

func (s *stubOrgBillingReader) GetByID(_ context.Context, orgID string) (*types.Organization, error) {
	return &types.Organization{
		ID:           orgID,
		Name:         "Stub Organization",
		BillingEmail: "stub@example.com",
		Plan:         types.PlanFree,
	}, nil
}

// stubUsageReporterService implements handlers.UsageReporter with error responses.
type stubUsageReporterService struct{}

func (s *stubUsageReporterService) GetCurrentUsage(_ context.Context, _ string) (*types.UsageSnapshot, error) {
	return nil, types.NewAppError(types.ErrCodeInternalUnexpected, "usage reporter not configured", nil)
}

func (s *stubUsageReporterService) GetUsageHistory(_ context.Context, _ string, _, _ time.Time, _ types.TimeGranularity) ([]*types.UsageDataPoint, error) {
	return nil, types.NewAppError(types.ErrCodeInternalUnexpected, "usage reporter not configured", nil)
}

// stubAuditLoggerService implements handlers.AuditLogger with no-op behavior.
type stubAuditLoggerService struct{}

func (s *stubAuditLoggerService) Log(_ context.Context, _ types.AuditEvent) error {
	return nil
}

// =============================================================================
// Stub Dependencies for Organization & User Handlers
// =============================================================================

// stubOrgHandlerRepo implements handlers.OrgRepo with stub responses.
type stubOrgHandlerRepo struct{}

func (s *stubOrgHandlerRepo) GetByID(_ context.Context, _ string) (*types.Organization, error) {
	return nil, types.NewAppError(types.ErrCodeNotFoundOrg, "organization service not configured", nil)
}
func (s *stubOrgHandlerRepo) Create(_ context.Context, _ *types.Organization) error { return nil }
func (s *stubOrgHandlerRepo) Update(_ context.Context, _ *types.Organization) error { return nil }
func (s *stubOrgHandlerRepo) Delete(_ context.Context, _ string) error              { return nil }

// stubOrgWatchPointRepo implements handlers.OrgWatchPointRepo with no-op.
type stubOrgWatchPointRepo struct{}

func (s *stubOrgWatchPointRepo) PauseAllByOrgID(_ context.Context, _ string) error { return nil }

// stubOrgNotificationRepo implements handlers.OrgNotificationRepo with empty results.
type stubOrgNotificationRepo struct{}

func (s *stubOrgNotificationRepo) List(_ context.Context, _ types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error) {
	return nil, types.PageInfo{}, nil
}

// stubOrgAuditRepo implements handlers.OrgAuditRepo with no-op behavior.
type stubOrgAuditRepo struct{}

func (s *stubOrgAuditRepo) Log(_ context.Context, _ *types.AuditEvent) error { return nil }
func (s *stubOrgAuditRepo) List(_ context.Context, _ types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error) {
	return nil, types.PageInfo{}, nil
}

// stubOrgUserRepo implements handlers.OrgUserRepo with no-op behavior.
type stubOrgUserRepo struct{}

func (s *stubOrgUserRepo) CreateWithProvider(_ context.Context, _ *types.User) error { return nil }

// stubOrgBillingServiceImpl implements handlers.OrgBillingService with no-op behavior.
type stubOrgBillingServiceImpl struct{}

func (s *stubOrgBillingServiceImpl) EnsureCustomer(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (s *stubOrgBillingServiceImpl) CancelSubscription(_ context.Context, _ string) error {
	return nil
}

// stubUserHandlerRepo implements handlers.UserRepo with stub responses.
type stubUserHandlerRepo struct{}

func (s *stubUserHandlerRepo) GetByID(_ context.Context, _, _ string) (*types.User, error) {
	return nil, types.NewAppError(types.ErrCodeNotFoundUser, "user service not configured", nil)
}
func (s *stubUserHandlerRepo) GetByEmail(_ context.Context, _ string) (*types.User, error) {
	return nil, types.NewAppError(types.ErrCodeNotFoundUser, "user service not configured", nil)
}
func (s *stubUserHandlerRepo) Update(_ context.Context, _ *types.User) error { return nil }
func (s *stubUserHandlerRepo) Delete(_ context.Context, _, _ string) error   { return nil }
func (s *stubUserHandlerRepo) CountOwners(_ context.Context, _ string) (int, error) {
	return 1, nil
}
func (s *stubUserHandlerRepo) CreateInvited(_ context.Context, _ *types.User, _ string, _ time.Time) error {
	return nil
}
func (s *stubUserHandlerRepo) UpdateInviteToken(_ context.Context, _, _ string, _ time.Time) error {
	return nil
}
func (s *stubUserHandlerRepo) ListByOrg(_ context.Context, _ string, _ *types.UserRole, _ int, _ string) ([]*types.User, error) {
	return nil, nil
}

// stubUserSessionRepo implements handlers.UserSessionRepo with no-op behavior.
type stubUserSessionRepo struct{}

func (s *stubUserSessionRepo) DeleteByUser(_ context.Context, _ string) error { return nil }

// stubEmailServiceImpl implements handlers.UserEmailService with no-op behavior.
type stubEmailServiceImpl struct{}

func (s *stubEmailServiceImpl) SendInvite(_ context.Context, _, _ string, _ types.UserRole) error {
	return nil
}

// stubAPIKeyHandlerRepo implements handlers.APIKeyRepo with stub responses.
type stubAPIKeyHandlerRepo struct{}

func (s *stubAPIKeyHandlerRepo) List(_ context.Context, _ string, _ handlers.APIKeyListParams) ([]*types.APIKey, error) {
	return nil, nil
}
func (s *stubAPIKeyHandlerRepo) Create(_ context.Context, _ *types.APIKey) error { return nil }
func (s *stubAPIKeyHandlerRepo) GetByID(_ context.Context, _, _ string) (*types.APIKey, error) {
	return nil, types.NewAppError(types.ErrCodeNotFoundAPIKey, "not found", nil)
}
func (s *stubAPIKeyHandlerRepo) Delete(_ context.Context, _, _ string) error { return nil }
func (s *stubAPIKeyHandlerRepo) Rotate(_ context.Context, _, _ string, _ *types.APIKey, _ time.Time) error {
	return nil
}
func (s *stubAPIKeyHandlerRepo) CountRecentByUser(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, nil
}

// Compile-time interface assertions to ensure stubs satisfy their contracts.
var (
	_ types.RepositoryRegistry  = (*stubRepositoryRegistry)(nil)
	_ types.SecurityService     = (*stubSecurityService)(nil)
	_ core.Authenticator        = (*stubAuthenticator)(nil)
	_ core.RateLimitStore       = (*stubRateLimitStore)(nil)
	_ core.IdempotencyStore     = (*stubIdempotencyStore)(nil)
	_ core.MetricsCollector     = (*stubMetricsCollector)(nil)
	_ handlers.AuthService      = (*stubAuthHandlerService)(nil)
	_ handlers.SessionService   = (*stubSessionHandlerService)(nil)
	_ handlers.BillingService   = (*stubBillingHandlerService)(nil)
	_ handlers.OrgBillingReader = (*stubOrgBillingReader)(nil)
	_ handlers.UsageReporter    = (*stubUsageReporterService)(nil)
	_ handlers.AuditLogger      = (*stubAuditLoggerService)(nil)

	// Organization & User handler stubs
	_ handlers.OrgRepo              = (*stubOrgHandlerRepo)(nil)
	_ handlers.OrgWatchPointRepo    = (*stubOrgWatchPointRepo)(nil)
	_ handlers.OrgNotificationRepo  = (*stubOrgNotificationRepo)(nil)
	_ handlers.OrgAuditRepo         = (*stubOrgAuditRepo)(nil)
	_ handlers.OrgUserRepo          = (*stubOrgUserRepo)(nil)
	_ handlers.OrgBillingService    = (*stubOrgBillingServiceImpl)(nil)
	_ handlers.UserRepo             = (*stubUserHandlerRepo)(nil)
	_ handlers.UserSessionRepo      = (*stubUserSessionRepo)(nil)
	_ handlers.UserEmailService     = (*stubEmailServiceImpl)(nil)
	_ handlers.APIKeyRepo           = (*stubAPIKeyHandlerRepo)(nil)
)

// Package main is the entry point for the WatchPoint API server.
//
// It initializes the configuration, creates the database connection pool,
// wires real repositories and services for internal dependencies, uses stub
// implementations for external third-party services (Stripe, SES, OAuth)
// that require real API keys, and starts the HTTP server.
//
// In local mode (APP_ENV=local), it runs as a standard HTTP server on the
// configured port. In Lambda mode, it will use the chiadapter to bridge
// API Gateway events to the chi router.
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
	"github.com/jackc/pgx/v5/pgxpool"

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

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// run encapsulates the startup lifecycle so that main() can cleanly exit on error.
func run() error {
	// Load configuration. For non-local environments, create a real SSMProvider
	// to resolve secrets from AWS SSM Parameter Store. For local development,
	// SSM resolution is bypassed when APP_ENV=local (nil provider is safe).
	var secretProvider config.SecretProvider
	if os.Getenv("APP_ENV") != "local" {
		secretProvider = config.NewSSMProvider(os.Getenv("AWS_REGION"))
	}
	cfg, err := config.LoadConfig(secretProvider)
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

	// -------------------------------------------------------------------
	// Database Connection Pool
	// -------------------------------------------------------------------
	pool, err := initDBPool(cfg)
	if err != nil {
		return fmt.Errorf("initializing database pool: %w", err)
	}
	defer pool.Close()

	// Verify database connectivity at startup (fail-fast).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping failed: %w", err)
	}
	logger.Info("database connection pool established")

	// -------------------------------------------------------------------
	// Real Repositories (backed by pgxpool)
	// -------------------------------------------------------------------
	userRepo := db.NewUserRepository(pool)
	orgRepo := db.NewOrganizationRepository(pool)
	sessionRepo := db.NewSessionRepository(pool)
	securityRepo := db.NewSecurityRepository(pool)
	watchpointRepo := db.NewWatchPointRepository(pool)
	notificationRepo := db.NewNotificationRepository(pool)
	// forecastRunRepo is available but not yet used by the API server
	// (used by data-poller and eval-worker). Declared for reference:
	// _ = db.NewForecastRunRepository(pool)
	auditRepo := db.NewAuditRepository(pool)
	apiKeyRepo := db.NewAPIKeyRepository(pool)
	subStateRepo := db.NewSubscriptionStateRepo(pool, logger)
	usageDBImpl := db.NewUsageDBImpl(pool)
	usageHistoryRepo := db.NewUsageHistoryRepo(pool)

	// Build a real RepositoryRegistry for core.Server.
	repos := &pgRepositoryRegistry{
		wpRepo:    watchpointRepo,
		orgRepo:   orgRepo,
		userRepo:  userRepo,
		notifRepo: notificationRepo,
		pool:      pool,
	}

	// -------------------------------------------------------------------
	// Real Services (internal domain logic)
	// -------------------------------------------------------------------

	// Security Service
	securitySvc := auth.NewSecurityService(
		securityRepo,
		auth.DefaultSecurityConfig(),
		nil, // use RealClock
		logger,
	)

	// Session Service
	tokenGen := auth.NewCryptoTokenGenerator()
	sessionSvc := auth.NewSessionService(
		sessionRepo,
		tokenGen,
		auth.DefaultSessionConfig(),
		nil, // use RealClock
		logger,
	)

	// Auth Service (wrapping real service + stub for unimplemented methods)
	txManager := &pgAuthTxManager{pool: pool}
	realAuthSvc := auth.NewAuthService(auth.AuthServiceConfig{
		UserRepo:       userRepo,
		SessionService: sessionSvc,
		Security:       securitySvc,
		TxManager:      txManager,
		Hasher:         nil, // use production bcryptHasher
		Clock:          nil, // use RealClock
		Logger:         logger,
	})

	// Plan Registry (static in-memory)
	planRegistry := billing.NewStaticPlanRegistry()

	// Usage Reporter (for billing and usage endpoints)
	usageReporter := billing.NewUsageReporter(
		usageDBImpl, // implements billing.OrgLookup
		usageDBImpl, // implements billing.UsageDB
		usageHistoryRepo,
		planRegistry,
	)

	// -------------------------------------------------------------------
	// External Service Stubs (require real API keys in production)
	// -------------------------------------------------------------------
	// These are stubbed because they interact with third-party APIs.
	// In production, these would be replaced with real implementations
	// using config.Billing.StripeSecretKey, AWS SES (IAM), etc.
	stubBillingSvc := external.NewStubBillingService(logger)
	stubOAuthMgr := &oauthManagerAdapter{inner: external.NewStubOAuthManager(logger, "google", "github")}
	stubWebhookVerifier := external.NewStubWebhookVerifier(logger)
	stubEmailProvider := external.NewStubEmailProvider(logger)

	// -------------------------------------------------------------------
	// Infrastructure Stubs (no real implementations yet)
	// -------------------------------------------------------------------
	// These components don't have real implementations in the codebase yet.
	// They will be implemented in later phases.
	stubAuthenticator := &noopAuthenticator{}
	stubRateLimitStore := &noopRateLimitStore{}
	stubIdempotencyStore := &noopIdempotencyStore{}
	stubMetrics := &noopMetricsCollector{}

	// -------------------------------------------------------------------
	// Build the Server
	// -------------------------------------------------------------------
	srv, err := core.NewServer(cfg, repos, logger)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	// Inject infrastructure dependencies.
	srv.SecurityService = securitySvc
	srv.Authenticator = stubAuthenticator
	srv.RateLimitStore = stubRateLimitStore
	srv.IdempotencyStore = stubIdempotencyStore
	srv.Metrics = stubMetrics

	// -------------------------------------------------------------------
	// Wire Handlers with Real Dependencies
	// -------------------------------------------------------------------

	// Auth Handler
	authHandlerSvc := &authServiceAdapter{real: realAuthSvc}
	authHandler := handlers.NewAuthHandler(
		authHandlerSvc,
		sessionSvc,
		stubOAuthMgr,
		handlers.DefaultCookieConfig(),
		logger,
		srv.Validator,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		r.Route("/auth", authHandler.RegisterRoutes)
	})

	// Forecast Handler (stub forecast service -- requires S3/Zarr reader)
	stubForecastSvc := &noopForecastService{}
	forecastHandler := handlers.NewForecastHandler(stubForecastSvc, srv.Validator, logger)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		r.Route("/forecasts", forecastHandler.RegisterRoutes)
	})

	// Billing Handler
	billingAudit := &auditLoggerAdapter{repo: auditRepo}
	usageHandler := handlers.NewUsageHandler(usageReporter, orgRepo, srv.Validator)
	billingHandler := handlers.NewBillingHandler(
		stubBillingSvc,
		orgRepo,
		usageHandler,
		billingAudit,
		cfg,
		srv.Validator,
		logger,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, billingHandler.RegisterRoutes)

	// Stripe Webhook Handler
	stripeWebhookHandler := handlers.NewStripeWebhookHandler(
		stubWebhookVerifier,
		subStateRepo,
		&wpResumerAdapter{repo: watchpointRepo},
		string(cfg.Billing.StripeWebhookSecret),
		logger,
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, stripeWebhookHandler.RegisterRoutes)

	// Organization Handler
	orgNotifAdapter := &orgNotifRepoAdapter{repo: notificationRepo}
	wpPauseAdapter := &orgWPPauseAdapter{repo: watchpointRepo}
	orgHandler := handlers.NewOrganizationHandler(
		orgRepo,
		wpPauseAdapter,
		orgNotifAdapter,
		auditRepo,
		userRepo,
		planRegistry,
		&orgBillingAdapter{svc: stubBillingSvc},
		srv.Validator,
		logger,
	)

	// User Handler
	emailSvcAdapter := &emailServiceAdapter{provider: stubEmailProvider, cfg: cfg}
	userHandler := handlers.NewUserHandler(
		userRepo,
		sessionRepo,
		emailSvcAdapter,
		billingAudit,
		srv.Validator,
		logger,
	)

	// API Key Handler
	apiKeyRepoAdapter := &apiKeyRepoAdapter{repo: apiKeyRepo}
	apiKeyHandler := handlers.NewAPIKeyHandler(apiKeyRepoAdapter, billingAudit, logger)

	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, func(r chi.Router) {
		orgHandler.RegisterRoutes(r, userHandler, apiKeyHandler)
	})

	// WatchPoint Handler
	wpAuditAdapter := &wpAuditLoggerAdapter{repo: auditRepo}
	wpHandler := handlers.NewWatchPointHandler(
		watchpointRepo,
		notificationRepo,
		srv.Validator,
		logger,
		stubForecastSvc, // WPForecastProvider (GetSnapshot)
		usageReporter,   // WPUsageEnforcer (CheckLimit)
		wpAuditAdapter,
		nil, // EvalTrigger -- requires SQS client, not available locally
	)
	srv.V1RouteRegistrars = append(srv.V1RouteRegistrars, wpHandler.RegisterRoutes)

	// -------------------------------------------------------------------
	// Mount All Routes and Start Server
	// -------------------------------------------------------------------
	srv.MountRoutes()

	// Determine if running in Lambda mode.
	if isLambdaEnvironment() {
		return runLambda(srv, logger)
	}

	return runHTTPServer(srv, cfg, logger)
}

// initDBPool creates a pgxpool.Pool configured from the application config.
func initDBPool(cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(string(cfg.Database.URL))
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.Database.MaxConns)
	poolCfg.MinConns = int32(cfg.Database.MinConns)
	poolCfg.MaxConnLifetime = cfg.Database.MaxConnLifetime
	poolCfg.HealthCheckPeriod = cfg.Database.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	return pool, nil
}

// isLambdaEnvironment returns true if the process is running inside AWS Lambda.
func isLambdaEnvironment() bool {
	_, hasRuntimeAPI := os.LookupEnv("AWS_LAMBDA_RUNTIME_API")
	_, hasServerPort := os.LookupEnv("_LAMBDA_SERVER_PORT")
	return hasRuntimeAPI || hasServerPort
}

// runLambda starts the server in AWS Lambda mode using the chi adapter.
func runLambda(srv *core.Server, logger *slog.Logger) error {
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

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

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

	logger.Info("initiating graceful shutdown")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}

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
// Repository Registry (backed by pgxpool)
// =============================================================================

// pgRepositoryRegistry implements types.RepositoryRegistry using real
// PostgreSQL-backed repositories.
type pgRepositoryRegistry struct {
	wpRepo    *db.WatchPointRepository
	orgRepo   *db.OrganizationRepository
	userRepo  *db.UserRepository
	notifRepo *db.NotificationRepository
	pool      *pgxpool.Pool
}

func (r *pgRepositoryRegistry) WatchPoints() types.WatchPointRepository     { return r.wpRepo }
func (r *pgRepositoryRegistry) Organizations() types.OrganizationRepository { return r.orgRepo }
func (r *pgRepositoryRegistry) Users() types.UserRepository                 { return r.userRepo }
func (r *pgRepositoryRegistry) Notifications() types.NotificationRepository { return r.notifRepo }

// Close shuts down the underlying connection pool. Called by Server.Shutdown
// via the interface{ Close() error } type assertion.
func (r *pgRepositoryRegistry) Close() error {
	r.pool.Close()
	return nil
}

// =============================================================================
// Auth Transaction Manager (pgx-based)
// =============================================================================

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

	if err := tx.Commit(ctx); err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to commit transaction", err)
	}
	return nil
}

// =============================================================================
// Service Adapters (bridge interface mismatches)
// =============================================================================

// authServiceAdapter bridges auth.authService (Login, AcceptInvite) to the
// handlers.AuthService interface (which also requires RequestPasswordReset,
// CompletePasswordReset). The password reset methods return stub errors until
// they are implemented in the auth package.
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
	// Not yet implemented in auth package. Return nil per enumeration
	// protection: the handler should not reveal whether the email exists.
	return nil
}

func (a *authServiceAdapter) CompletePasswordReset(_ context.Context, _, _ string) error {
	return types.NewAppError(types.ErrCodeInternalUnexpected, "password reset not yet implemented", nil)
}

// auditLoggerAdapter bridges db.AuditRepository (takes *types.AuditEvent)
// to handlers.AuditLogger (takes types.AuditEvent by value).
type auditLoggerAdapter struct {
	repo *db.AuditRepository
}

func (a *auditLoggerAdapter) Log(ctx context.Context, event types.AuditEvent) error {
	return a.repo.Log(ctx, &event)
}

// wpAuditLoggerAdapter bridges db.AuditRepository to handlers.WPAuditLogger
// (takes *types.AuditEvent by pointer -- matches directly).
type wpAuditLoggerAdapter struct {
	repo *db.AuditRepository
}

func (a *wpAuditLoggerAdapter) Log(ctx context.Context, entry *types.AuditEvent) error {
	return a.repo.Log(ctx, entry)
}

// orgNotifRepoAdapter bridges db.NotificationRepository.List (returns
// []*types.NotificationHistoryItem) to handlers.OrgNotificationRepo.List
// (returns []types.NotificationHistoryItem by value).
type orgNotifRepoAdapter struct {
	repo *db.NotificationRepository
}

func (a *orgNotifRepoAdapter) List(ctx context.Context, filter types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error) {
	items, pageInfo, err := a.repo.List(ctx, filter)
	if err != nil {
		return nil, pageInfo, err
	}
	// Convert []*T to []T.
	result := make([]types.NotificationHistoryItem, len(items))
	for i, item := range items {
		if item != nil {
			result[i] = *item
		}
	}
	return result, pageInfo, nil
}

// orgWPPauseAdapter bridges db.WatchPointRepository.PauseAllByOrgID (takes
// orgID and reason) to handlers.OrgWatchPointRepo.PauseAllByOrgID (takes
// only orgID). Defaults to "org_deletion" as the pause reason.
type orgWPPauseAdapter struct {
	repo *db.WatchPointRepository
}

func (a *orgWPPauseAdapter) PauseAllByOrgID(ctx context.Context, orgID string) error {
	return a.repo.PauseAllByOrgID(ctx, orgID, "org_deletion")
}

// wpResumerAdapter bridges db.WatchPointRepository.ResumeAllByOrgID (takes
// orgID and string reason) to handlers.WatchPointResumer.ResumeAllByOrgID
// (takes orgID and types.PausedReason).
type wpResumerAdapter struct {
	repo *db.WatchPointRepository
}

func (a *wpResumerAdapter) ResumeAllByOrgID(ctx context.Context, orgID string, reason types.PausedReason) error {
	return a.repo.ResumeAllByOrgID(ctx, orgID, string(reason))
}

// apiKeyRepoAdapter bridges db.APIKeyRepository to handlers.APIKeyRepo.
// The only difference is the List method parameter type (db.ListAPIKeysParams
// vs handlers.APIKeyListParams -- same fields, different types).
type apiKeyRepoAdapter struct {
	repo *db.APIKeyRepository
}

func (a *apiKeyRepoAdapter) List(ctx context.Context, orgID string, params handlers.APIKeyListParams) ([]*types.APIKey, error) {
	return a.repo.List(ctx, orgID, db.ListAPIKeysParams{
		ActiveOnly: params.ActiveOnly,
		Prefix:     params.Prefix,
		Limit:      params.Limit,
		Cursor:     params.Cursor,
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

// emailServiceAdapter bridges external.EmailProvider to handlers.UserEmailService.
// The invite flow needs to construct an invite URL and send an email.
type emailServiceAdapter struct {
	provider external.EmailProvider
	cfg      *config.Config
}

func (a *emailServiceAdapter) SendInvite(ctx context.Context, toEmail string, inviteURL string, role types.UserRole) error {
	_, err := a.provider.Send(ctx, types.SendInput{
		To: toEmail,
		From: types.SenderIdentity{
			Address: "noreply@watchpoint.io",
			Name:    "WatchPoint",
		},
		Subject:     fmt.Sprintf("You've been invited to WatchPoint as %s", role),
		BodyHTML:    fmt.Sprintf("<p>You've been invited to WatchPoint as <strong>%s</strong>.</p><p><a href=\"%s\">Accept Invitation</a></p>", role, inviteURL),
		BodyText:    fmt.Sprintf("You've been invited to WatchPoint as %s.\n\nAccept: %s", role, inviteURL),
		ReferenceID: fmt.Sprintf("invite_%s", toEmail),
	})
	return err
}

// oauthManagerAdapter bridges external.OAuthManager (returns external.OAuthProvider)
// to handlers.OAuthManager (returns handlers.OAuthProvider). Both interfaces
// have the same methods, but Go's type system treats them as distinct.
type oauthManagerAdapter struct {
	inner external.OAuthManager
}

func (a *oauthManagerAdapter) GetProvider(name string) (handlers.OAuthProvider, error) {
	return a.inner.GetProvider(name)
}

// orgBillingAdapter bridges external.BillingService (which has EnsureCustomer
// but not CancelSubscription) to handlers.OrgBillingService (which needs both).
type orgBillingAdapter struct {
	svc external.BillingService
}

func (a *orgBillingAdapter) EnsureCustomer(ctx context.Context, orgID string, email string) (string, error) {
	return a.svc.EnsureCustomer(ctx, orgID, email)
}

func (a *orgBillingAdapter) CancelSubscription(_ context.Context, _ string) error {
	// Not yet implemented. In production, this would call Stripe to cancel.
	// For now, return nil as a no-op.
	return nil
}

// =============================================================================
// Infrastructure Stubs (no real implementations yet)
//
// These satisfy interfaces for components that don't have concrete
// implementations in this phase. They will be replaced in later phases.
// =============================================================================

// noopAuthenticator implements core.Authenticator that returns a system actor
// for any token, allowing the server to start without real auth infrastructure.
type noopAuthenticator struct{}

func (s *noopAuthenticator) ResolveToken(_ context.Context, _ string) (*types.Actor, error) {
	return &types.Actor{
		ID:             "system_stub",
		Type:           types.ActorTypeSystem,
		OrganizationID: "org_stub",
		Source:         "stub",
	}, nil
}

// noopRateLimitStore implements core.RateLimitStore that always allows requests.
type noopRateLimitStore struct{}

func (s *noopRateLimitStore) IncrementAndCheck(_ context.Context, _ string, _ int, _ time.Duration) (core.RateLimitResult, error) {
	return core.RateLimitResult{
		Allowed:   true,
		Remaining: 1000,
		ResetAt:   time.Now().Add(time.Hour),
	}, nil
}

// noopIdempotencyStore implements core.IdempotencyStore with no persistence.
type noopIdempotencyStore struct{}

func (s *noopIdempotencyStore) Get(_ context.Context, _ string, _ string) (*core.IdempotencyRecord, error) {
	return nil, nil
}
func (s *noopIdempotencyStore) Create(_ context.Context, _, _, _ string) error        { return nil }
func (s *noopIdempotencyStore) Complete(_ context.Context, _, _ string, _ int, _ []byte) error { return nil }
func (s *noopIdempotencyStore) Fail(_ context.Context, _, _ string) error             { return nil }

// noopMetricsCollector implements core.MetricsCollector with no-op recording.
type noopMetricsCollector struct{}

func (s *noopMetricsCollector) RecordRequest(_, _, _ string, _ time.Duration) {}

// noopForecastService implements handlers.ForecastServiceInterface with error
// responses. The real ForecastService requires an S3 client and Zarr reader
// which are not available in local mode.
type noopForecastService struct{}

func (s *noopForecastService) GetPointForecast(_ context.Context, _, _ float64, _, _ time.Time) (*forecasts.ForecastResponse, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

func (s *noopForecastService) GetBatchForecast(_ context.Context, _ forecasts.BatchForecastRequest) (*forecasts.BatchForecastResult, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

func (s *noopForecastService) GetVariables(_ context.Context) ([]forecasts.VariableResponseMetadata, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

func (s *noopForecastService) GetStatus(_ context.Context) (*forecasts.SystemStatus, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

func (s *noopForecastService) GetSnapshot(_ context.Context, _, _ float64) (*types.ForecastSnapshot, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

func (s *noopForecastService) GetVerificationMetrics(_ context.Context, _ string, _, _ time.Time) (*forecasts.VerificationReport, error) {
	return nil, types.NewAppError(types.ErrCodeUpstreamForecast, "forecast service not configured", nil)
}

// =============================================================================
// Compile-time Interface Assertions
// =============================================================================

var (
	_ types.RepositoryRegistry = (*pgRepositoryRegistry)(nil)
	_ auth.AuthTxManager       = (*pgAuthTxManager)(nil)
	_ core.Authenticator       = (*noopAuthenticator)(nil)
	_ core.RateLimitStore      = (*noopRateLimitStore)(nil)
	_ core.IdempotencyStore    = (*noopIdempotencyStore)(nil)
	_ core.MetricsCollector    = (*noopMetricsCollector)(nil)
	_ handlers.AuthService     = (*authServiceAdapter)(nil)
	_ handlers.AuditLogger     = (*auditLoggerAdapter)(nil)
	_ handlers.WPAuditLogger   = (*wpAuditLoggerAdapter)(nil)
	_ handlers.OrgNotificationRepo = (*orgNotifRepoAdapter)(nil)
	_ handlers.OrgWatchPointRepo   = (*orgWPPauseAdapter)(nil)
	_ handlers.WatchPointResumer   = (*wpResumerAdapter)(nil)
	_ handlers.APIKeyRepo          = (*apiKeyRepoAdapter)(nil)
	_ handlers.UserEmailService    = (*emailServiceAdapter)(nil)
	_ handlers.OAuthManager        = (*oauthManagerAdapter)(nil)
	_ handlers.OrgBillingService   = (*orgBillingAdapter)(nil)
	_ handlers.ForecastServiceInterface = (*noopForecastService)(nil)
)

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

// Compile-time interface assertions to ensure stubs satisfy their contracts.
var (
	_ types.RepositoryRegistry = (*stubRepositoryRegistry)(nil)
	_ types.SecurityService    = (*stubSecurityService)(nil)
	_ core.Authenticator       = (*stubAuthenticator)(nil)
	_ core.RateLimitStore      = (*stubRateLimitStore)(nil)
	_ core.IdempotencyStore    = (*stubIdempotencyStore)(nil)
	_ core.MetricsCollector    = (*stubMetricsCollector)(nil)
)

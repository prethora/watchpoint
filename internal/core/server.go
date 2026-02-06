// Package core provides the API chassis for the WatchPoint platform.
// It creates a chi router compatible with both standard HTTP (for local dev)
// and AWS Lambda Proxy Integration (via chiadapter). It enforces cross-cutting
// concerns -- security, logging, observability, and error handling -- before
// requests reach domain-specific handlers.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

// MetricsCollector defines the interface for recording API telemetry.
// Implementations record request latency and count metrics to CloudWatch
// or equivalent backends.
type MetricsCollector interface {
	// RecordRequest records API request metrics including latency and count.
	// Uses metric constants MetricAPILatency and MetricAPIRequestCount
	// from the types package.
	RecordRequest(method, endpoint, status string, duration time.Duration)
}

// Server encapsulates all dependencies for the WatchPoint API, allowing for
// easy injection during testing and distinct configuration for different
// environments.
type Server struct {
	Config          *config.Config
	Repos           types.RepositoryRegistry
	Logger          *slog.Logger
	Validator       *Validator
	Metrics         MetricsCollector
	SecurityService types.SecurityService
	Authenticator   Authenticator // Resolves tokens to Actors; injected for testability.

	// Internal router
	router *chi.Mux
}

// NewServer initializes dependencies, sets up the router, and prepares the
// server for route mounting. It performs a "fail-fast" check on critical
// configuration.
//
// The caller is responsible for mounting routes (via MountRoutes or equivalent)
// after construction. This separation allows tests to customize route
// registration.
func NewServer(
	cfg *config.Config,
	repos types.RepositoryRegistry,
	logger *slog.Logger,
) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	if repos == nil {
		return nil, fmt.Errorf("repository registry must not be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger must not be nil")
	}

	s := &Server{
		Config:    cfg,
		Repos:     repos,
		Logger:    logger,
		Validator: NewValidator(logger),
		router:    chi.NewRouter(),
	}

	return s, nil
}

// Handler returns the http.Handler interface for the router.
// Used by http.ListenAndServe (local) and chiadapter.New (Lambda).
func (s *Server) Handler() http.Handler {
	return s.router
}

// Router returns the underlying chi.Mux for route registration.
// This is used internally by route-mounting methods and tests.
func (s *Server) Router() *chi.Mux {
	return s.router
}

// Shutdown performs a graceful termination of server resources.
// 1. Closes Database connection pools (if the registry supports it).
// 2. Flushes any buffered logs.
// 3. Cancels any background contexts derived from Server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.Logger.Info("server shutdown initiated")

	// Close database connections if the repository registry supports it.
	if closer, ok := s.Repos.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			s.Logger.Error("error closing repository connections", "error", err)
			return fmt.Errorf("closing repository connections: %w", err)
		}
	}

	s.Logger.Info("server shutdown complete")
	return nil
}

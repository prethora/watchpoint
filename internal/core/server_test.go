package core

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

// mockRepositoryRegistry implements types.RepositoryRegistry for testing.
type mockRepositoryRegistry struct{}

func (m *mockRepositoryRegistry) WatchPoints() types.WatchPointRepository       { return nil }
func (m *mockRepositoryRegistry) Organizations() types.OrganizationRepository   { return nil }
func (m *mockRepositoryRegistry) Users() types.UserRepository                   { return nil }
func (m *mockRepositoryRegistry) Notifications() types.NotificationRepository   { return nil }

// mockMetricsCollector implements MetricsCollector for testing.
type mockMetricsCollector struct {
	calls []metricsCall
}

type metricsCall struct {
	method, endpoint, status string
	duration                 time.Duration
}

func (m *mockMetricsCollector) RecordRequest(method, endpoint, status string, duration time.Duration) {
	m.calls = append(m.calls, metricsCall{method, endpoint, status, duration})
}

// mockSecurityService implements types.SecurityService for testing.
type mockSecurityService struct{}

func (m *mockSecurityService) RecordAttempt(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (m *mockSecurityService) IsIPBlocked(_ context.Context, _ string) bool         { return false }
func (m *mockSecurityService) IsIdentifierBlocked(_ context.Context, _ string) bool { return false }

func TestNewServer_Success(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer returned nil server")
	}
	if srv.Config != cfg {
		t.Error("Config field not set correctly")
	}
	if srv.Repos != repos {
		t.Error("Repos field not set correctly")
	}
	if srv.Logger != logger {
		t.Error("Logger field not set correctly")
	}
	if srv.Validator == nil {
		t.Error("Validator should be initialized by constructor")
	}
	if srv.router == nil {
		t.Error("internal router should be initialized by constructor")
	}
}

func TestNewServer_NilConfig(t *testing.T) {
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()

	srv, err := NewServer(nil, repos, logger)
	if err == nil {
		t.Fatal("NewServer should return error for nil config")
	}
	if srv != nil {
		t.Error("NewServer should return nil server on error")
	}
}

func TestNewServer_NilRepos(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	logger := slog.Default()

	srv, err := NewServer(cfg, nil, logger)
	if err == nil {
		t.Fatal("NewServer should return error for nil repos")
	}
	if srv != nil {
		t.Error("NewServer should return nil server on error")
	}
}

func TestNewServer_NilLogger(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	repos := &mockRepositoryRegistry{}

	srv, err := NewServer(cfg, repos, nil)
	if err == nil {
		t.Fatal("NewServer should return error for nil logger")
	}
	if srv != nil {
		t.Error("NewServer should return nil server on error")
	}
}

func TestServer_Handler(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}

	handler := srv.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}
	// Verify it implements http.Handler
	var _ http.Handler = handler
}

func TestServer_Router(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}

	router := srv.Router()
	if router == nil {
		t.Fatal("Router() returned nil")
	}
}

func TestServer_Shutdown(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}

	err = srv.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown returned unexpected error: %v", err)
	}
}

// closableRepos wraps the mock with a Close method to test shutdown path.
type closableRepos struct {
	mockRepositoryRegistry
	closed bool
}

func (c *closableRepos) Close() error {
	c.closed = true
	return nil
}

func TestServer_Shutdown_ClosesRepos(t *testing.T) {
	cfg := &config.Config{Environment: "local"}
	repos := &closableRepos{}
	logger := slog.Default()

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}

	err = srv.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown returned unexpected error: %v", err)
	}
	if !repos.closed {
		t.Error("Shutdown should have called Close on the repository registry")
	}
}

func TestServer_ExportedFields(t *testing.T) {
	// Verify that all specified fields are accessible (exported).
	cfg := &config.Config{Environment: "local"}
	repos := &mockRepositoryRegistry{}
	logger := slog.Default()
	metrics := &mockMetricsCollector{}
	security := &mockSecurityService{}

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer returned unexpected error: %v", err)
	}

	// Set optional fields post-construction (these are exported)
	srv.Metrics = metrics
	srv.SecurityService = security

	if srv.Metrics != metrics {
		t.Error("Metrics field not set correctly")
	}
	if srv.SecurityService != security {
		t.Error("SecurityService field not set correctly")
	}
}

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"watchpoint/internal/config"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// testRepositoryRegistry implements types.RepositoryRegistry with nil
// repositories for tests that only exercise infrastructure routes (health,
// openapi) and don't hit domain handlers.
type testRepositoryRegistry struct{}

func (r *testRepositoryRegistry) WatchPoints() types.WatchPointRepository     { return nil }
func (r *testRepositoryRegistry) Organizations() types.OrganizationRepository { return nil }
func (r *testRepositoryRegistry) Users() types.UserRepository                 { return nil }
func (r *testRepositoryRegistry) Notifications() types.NotificationRepository { return nil }

// buildTestServer creates a minimal server for health/openapi endpoint tests.
func buildTestServer(t *testing.T) *core.Server {
	t.Helper()
	setTestEnv(t)

	cfg, err := config.LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := core.NewServer(cfg, &testRepositoryRegistry{}, logger)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Wire infrastructure stubs (same types used by production main.go).
	srv.SecurityService = &noopSecurityService{}
	srv.Authenticator = &noopAuthenticator{}
	srv.RateLimitStore = &noopRateLimitStore{}
	srv.IdempotencyStore = &noopIdempotencyStore{}
	srv.Metrics = &noopMetricsCollector{}

	srv.MountRoutes()
	return srv
}

// noopSecurityService is a test-only stub for types.SecurityService.
type noopSecurityService struct{}

func (s *noopSecurityService) RecordAttempt(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (s *noopSecurityService) IsIPBlocked(_ context.Context, _ string) bool        { return false }
func (s *noopSecurityService) IsIdentifierBlocked(_ context.Context, _ string) bool { return false }

// TestHealthEndpoint verifies that the fully wired server responds with 200
// on GET /health when all stub dependencies are in place.
func TestHealthEndpoint(t *testing.T) {
	srv := buildTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health: got status %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if status, ok := resp["status"]; !ok || status != "healthy" {
		t.Errorf("GET /health: got status=%v, want 'healthy'", status)
	}
}

// TestOpenAPIEndpoint verifies that the OpenAPI spec placeholder returns 200.
func TestOpenAPIEndpoint(t *testing.T) {
	srv := buildTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /openapi.json: got status %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestIsLambdaEnvironment verifies Lambda environment detection logic.
func TestIsLambdaEnvironment(t *testing.T) {
	os.Unsetenv("AWS_LAMBDA_RUNTIME_API")
	os.Unsetenv("_LAMBDA_SERVER_PORT")

	if isLambdaEnvironment() {
		t.Error("isLambdaEnvironment: expected false when no Lambda env vars are set")
	}

	t.Setenv("AWS_LAMBDA_RUNTIME_API", "localhost:8080")
	if !isLambdaEnvironment() {
		t.Error("isLambdaEnvironment: expected true when AWS_LAMBDA_RUNTIME_API is set")
	}
}

// TestNewLogger verifies that the logger factory handles various log levels.
func TestNewLogger(t *testing.T) {
	tests := []struct {
		level string
	}{
		{"debug"},
		{"info"},
		{"warn"},
		{"error"},
		{"unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			logger := newLogger(tt.level)
			if logger == nil {
				t.Fatalf("newLogger(%q) returned nil", tt.level)
			}
		})
	}
}

// setTestEnv sets the minimal environment variables required by config.LoadConfig
// for a local environment. It uses t.Setenv to ensure cleanup after the test.
func setTestEnv(t *testing.T) {
	t.Helper()

	t.Setenv("APP_ENV", "local")
	t.Setenv("PORT", "8080")
	t.Setenv("API_EXTERNAL_URL", "http://localhost:8080")
	t.Setenv("DASHBOARD_URL", "http://localhost:3000")
	t.Setenv("DATABASE_URL", "postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable")
	t.Setenv("FORECAST_BUCKET", "watchpoint-forecasts")
	t.Setenv("SQS_EVAL_URGENT", "http://localhost:4566/000000000000/eval-queue-urgent")
	t.Setenv("SQS_EVAL_STANDARD", "http://localhost:4566/000000000000/eval-queue-standard")
	t.Setenv("SQS_NOTIFICATIONS", "http://localhost:4566/000000000000/notification-queue")
	t.Setenv("SQS_DLQ", "http://localhost:4566/000000000000/dead-letter-queue-shared")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_dummy")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_dummy")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_dummy")
	t.Setenv("SENDGRID_API_KEY", "SG.dummy")
	t.Setenv("EMAIL_TEMPLATES_JSON", `{"default":{"threshold_crossed":"d-template-001"}}`)
	t.Setenv("RUNPOD_API_KEY", "rp_dummy")
	t.Setenv("RUNPOD_ENDPOINT_ID", "dummy-endpoint-id")
	t.Setenv("SESSION_KEY", "local-dev-session-key-minimum-32-chars-long-for-validation")
	t.Setenv("ADMIN_API_KEY", "local-dev-admin-api-key")
}

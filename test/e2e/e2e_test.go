//go:build e2e

// Package e2e contains end-to-end integration tests that exercise the full
// WatchPoint platform pipeline: API -> Batcher -> EvalWorker -> Notification
// Workers -> Database.
//
// These tests require the local stack to be running (scripts/start-local-stack.sh)
// and Docker Compose services to be healthy (postgres, localstack, minio).
//
// Run with:
//
//	go test -v -tags e2e -timeout 120s ./test/e2e/
//
// The tests are gated behind the "e2e" build tag and are NOT included in the
// standard `go test ./...` invocation. This prevents accidental execution
// during normal development where the local stack may not be running.
//
// Architecture references:
//   - flow-simulations.md INT-001, INT-002, INT-004
//   - 12-operations.md Section 2 (Local Development)
package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// env is the shared test environment initialized in TestMain.
// All E2E tests use this for database access, HTTP client, and configuration.
var env *TestEnv

// TestMain initializes the shared test environment and runs all tests.
// It validates that the local stack is running and the database is accessible
// before any tests execute.
//
// If the environment is not ready (e.g., services not running), TestMain
// prints a diagnostic message and exits with code 0 (skip) rather than
// failing. This allows `go test -tags e2e ./test/e2e/` to be run safely
// even when the local stack is down -- it simply skips all tests.
func TestMain(m *testing.M) {
	cfg := DefaultTestConfig()

	var err error
	env, err = NewTestEnv(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "E2E test environment not ready, skipping all tests: %v\n", err)
		// Exit 0 to avoid marking CI as failed when the local stack is not running.
		os.Exit(0)
	}

	// Run tests and capture the exit code. We do not use defer + os.Exit
	// because os.Exit does not run deferred functions. Instead, we close
	// resources explicitly after m.Run completes.
	code := m.Run()
	env.Close()
	os.Exit(code)
}

// TestE2ESuiteSmoke is a minimal smoke test that validates the E2E test
// infrastructure is working: database is connected, API is reachable, and
// the test helpers compile correctly.
//
// This test exists to satisfy the definition of done for the scaffold task:
// "`go test ./test/e2e` compiles and runs a dummy test."
func TestE2ESuiteSmoke(t *testing.T) {
	// Verify the test environment is initialized.
	if env == nil {
		t.Fatal("test environment not initialized")
	}

	// Verify the database connection is alive.
	if env.Pool == nil {
		t.Fatal("database pool not initialized")
	}

	// Verify we can query the database.
	count := QueryDBScalar[int](t, env,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'",
	)
	t.Logf("database has %d public tables", count)
	if count == 0 {
		t.Fatal("no public tables found -- migrations may not have been applied")
	}

	// Verify the API server is responding.
	resp, err := env.Client.Get(env.Config.APIURL + "/health")
	if err != nil {
		t.Fatalf("API health check failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("API health check returned %d, expected 200", resp.StatusCode)
	}

	t.Logf("E2E test infrastructure is healthy:")
	t.Logf("  API URL:     %s", env.Config.APIURL)
	t.Logf("  Database:    connected (%d tables)", count)
	t.Logf("  Project:     %s", env.Config.ProjectRoot)

	// Verify cleanup works without error on an empty database.
	env.CleanupTestData(t)
	t.Log("cleanup completed successfully")
}

// TestE2EHelperCompilation verifies that all helper functions are callable.
// This is a compile-time verification that the helper signatures are correct
// and all dependencies resolve. No actual API calls or pipeline invocations
// are made -- this test constructs JSON payloads and validates they are well-formed.
func TestE2EHelperCompilation(t *testing.T) {
	// Verify EventModeWatchPointJSON produces valid JSON.
	t.Run("EventModeWatchPointJSON", func(t *testing.T) {
		now := time.Now()
		later := now.Add(48 * time.Hour)

		data, err := EventModeWatchPointJSON(
			"Test WatchPoint",
			40.0, -74.0,
			"precipitation_probability", ">", 70.0,
			now, later,
			"email",
			map[string]interface{}{"address": "test@example.com"},
		)
		if err != nil {
			t.Fatalf("EventModeWatchPointJSON failed: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("EventModeWatchPointJSON returned empty data")
		}
		t.Logf("EventModeWatchPointJSON produced %d bytes", len(data))
	})

	// Verify MonitorModeWatchPointJSON produces valid JSON.
	t.Run("MonitorModeWatchPointJSON", func(t *testing.T) {
		data, err := MonitorModeWatchPointJSON(
			"Monitor WatchPoint",
			40.0, -74.0,
			[]string{"temperature_c", "precipitation_probability"},
			"email",
			map[string]interface{}{"address": "test@example.com"},
		)
		if err != nil {
			t.Fatalf("MonitorModeWatchPointJSON failed: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("MonitorModeWatchPointJSON returned empty data")
		}
		t.Logf("MonitorModeWatchPointJSON produced %d bytes", len(data))
	})

	// Verify AuthenticatedRequest creates a request with correct headers.
	t.Run("AuthenticatedRequest", func(t *testing.T) {
		auth := UserAndOrg{
			OrganizationID: "org_test",
			UserID:         "user_test",
			CSRFToken:      "csrf_token_value",
		}
		req := AuthenticatedRequest(t, "GET", "http://localhost:8080/v1/test", nil, auth)
		if req.Header.Get("X-CSRF-Token") != "csrf_token_value" {
			t.Fatal("AuthenticatedRequest did not set X-CSRF-Token header")
		}
		if req.Header.Get("Content-Type") != "application/json" {
			t.Fatal("AuthenticatedRequest did not set Content-Type header")
		}
		if req.Header.Get("Authorization") == "" {
			t.Fatal("AuthenticatedRequest did not set Authorization header")
		}
		if req.Header.Get("Authorization") != "Bearer e2e-local-token" {
			t.Fatalf("AuthenticatedRequest: expected 'Bearer e2e-local-token', got %q", req.Header.Get("Authorization"))
		}
	})
}

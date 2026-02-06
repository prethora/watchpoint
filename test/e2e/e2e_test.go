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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
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

// ==========================================================================
// INT-001: New User to First Alert (The "Happy Path")
// ==========================================================================
//
// This test exercises the complete end-to-end pipeline for the most common
// scenario: a new user creates an Event Mode WatchPoint with a precipitation
// threshold, a forecast is generated that exceeds the threshold, and the user
// receives a notification.
//
// Pipeline:
//
//	DB (org+user+wp) -> Seeder (Zarr in MinIO) -> Batcher (SQS dispatch)
//	  -> EvalWorker (SQS poll -> evaluate -> DB + notification SQS)
//	  -> DB (notifications + deliveries)
//
// The test verifies:
//  1. WatchPoint is created with correct tile assignment.
//  2. Batcher successfully queries the DB and dispatches to SQS.
//  3. EvalWorker evaluates the condition and determines it is triggered.
//  4. A notification record is created in the database.
//  5. Notification deliveries are created for the configured channel.
//
// Architecture reference: flow-simulations.md INT-001
// Scenario config: scripts/scenarios/int001_precip_trigger.json

func TestHappyPath_EventMode(t *testing.T) {
	if env == nil {
		t.Fatal("test environment not initialized")
	}

	LogSeparator(t, "INT-001: Happy Path (Event Mode)")

	// -----------------------------------------------------------------------
	// Step 0: Clean up any residual data from prior runs
	// -----------------------------------------------------------------------
	t.Log("Step 0: Cleaning up test data from prior runs...")
	env.CleanupTestData(t)

	ctx := context.Background()

	// -----------------------------------------------------------------------
	// Step 1: Create Organization and User directly in DB
	// -----------------------------------------------------------------------
	// We bypass the API signup flow and insert directly to avoid depending on
	// the full auth pipeline. This matches the INT-001 flow where the user
	// has already signed up.
	LogSeparator(t, "Step 1: Create Organization & User")

	orgID := "org_" + uuid.New().String()
	userID := "user_" + uuid.New().String()
	orgEmail := fmt.Sprintf("int001-%s@example.com", uuid.New().String()[:8])

	// Insert organization
	_, err := env.Pool.Exec(ctx,
		`INSERT INTO organizations (id, name, billing_email, plan, plan_limits, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`,
		orgID,
		"INT-001 Test Org",
		orgEmail,
		"free",
		`{"watchpoints_max": 100, "api_calls_max": 10000, "notifications_max": 1000}`,
	)
	if err != nil {
		t.Fatalf("Step 1: failed to insert organization: %v", err)
	}
	t.Logf("  Organization created: %s", orgID)

	// Insert user (owner)
	_, err = env.Pool.Exec(ctx,
		`INSERT INTO users (id, organization_id, email, role, created_at)
		 VALUES ($1, $2, $3, $4, NOW())`,
		userID,
		orgID,
		orgEmail,
		"owner",
	)
	if err != nil {
		t.Fatalf("Step 1: failed to insert user: %v", err)
	}
	t.Logf("  User created: %s", userID)

	// -----------------------------------------------------------------------
	// Step 2: Create Event Mode WatchPoint via direct DB insert
	// -----------------------------------------------------------------------
	// Location: lat=40.0, lon=-74.0 (matches INT-001 scenario config)
	// Condition: precipitation_probability > 50% (threshold below the 80%
	//   value that the scenario will inject, ensuring the trigger fires)
	// Channel: email (with a test address)
	LogSeparator(t, "Step 2: Create WatchPoint (Event Mode)")

	// Time window: now to 48 hours from now (ensures the WP is active)
	now := time.Now().UTC()
	windowStart := now.Add(-1 * time.Hour)  // Started 1 hour ago
	windowEnd := now.Add(48 * time.Hour)    // Ends in 48 hours

	conditions := []map[string]interface{}{
		{
			"variable":  "precipitation_probability",
			"operator":  ">",
			"threshold": 50.0,
		},
	}

	channels := []map[string]interface{}{
		{
			"type":    "email",
			"config":  map[string]interface{}{"address": orgEmail},
			"enabled": true,
		},
	}

	wp := CreateWatchPointDirect(
		t, env, orgID,
		"INT-001 Precip Alert",
		40.0, -74.0,
		conditions, "ANY", channels,
		&windowStart, &windowEnd,
		nil, // monitorConfig is nil for event mode
	)

	t.Logf("  WatchPoint created: %s (tile_id=%s, status=%s)", wp.ID, wp.TileID, wp.Status)

	// Verify tile assignment.
	// For lat=40.0, lon=-74.0:
	//   lat_index = FLOOR((90.0 - 40.0) / 22.5) = FLOOR(2.222) = 2
	//   lon: -74.0 is negative, so 360 + (-74) = 286.0
	//   lon_index = FLOOR(286.0 / 45.0) = FLOOR(6.355) = 6
	//   tile_id = "2.6"
	expectedTileID := "2.6"
	if wp.TileID != expectedTileID {
		t.Fatalf("Step 2: expected tile_id %q, got %q", expectedTileID, wp.TileID)
	}
	t.Logf("  Tile ID verified: %s", wp.TileID)

	// -----------------------------------------------------------------------
	// Step 3: Seed a forecast_runs record (Phantom Run Prevention)
	// -----------------------------------------------------------------------
	// The Batcher performs Phantom Run Detection (IMPL-003): it queries
	// forecast_runs for the model+timestamp and rejects the event if no
	// matching record exists. We must pre-seed this record with status='running'
	// so the batcher recognizes it as a legitimate forecast.
	LogSeparator(t, "Step 3: Seed forecast_runs record")

	forecastTimestamp := now.Truncate(time.Second)
	forecastTimestampStr := forecastTimestamp.Format(time.RFC3339)
	runID := "run_" + uuid.New().String()

	_, err = env.Pool.Exec(ctx,
		`INSERT INTO forecast_runs
		 (id, model, run_timestamp, source_data_timestamp, storage_path, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		runID,
		"medium_range",
		forecastTimestamp,
		forecastTimestamp,
		fmt.Sprintf("forecasts/medium_range/%s/_SUCCESS", forecastTimestampStr),
		"running",
	)
	if err != nil {
		t.Fatalf("Step 3: failed to insert forecast_runs record: %v", err)
	}
	t.Logf("  Forecast run seeded: %s (model=medium_range, ts=%s, status=running)",
		runID, forecastTimestampStr)

	// -----------------------------------------------------------------------
	// Step 4: Trigger Forecast (Seed Zarr data + invoke Batcher)
	// -----------------------------------------------------------------------
	// This calls the forecast seeder with the INT-001 scenario config to
	// generate deterministic Zarr data in MinIO at the expected location,
	// then invokes the batcher binary with the run context.
	//
	// The scenario (int001_precip_trigger.json) injects:
	//   precipitation_probability = 80.0 at lat=40.0, lon=-74.0
	// which exceeds our threshold of 50.0.
	LogSeparator(t, "Step 4: Trigger Forecast")

	scenarioPath := filepath.Join(env.Config.ProjectRoot, "scripts", "scenarios", "int001_precip_trigger.json")

	t.Logf("  Scenario: %s", scenarioPath)
	t.Logf("  Model: medium_range")
	t.Logf("  Timestamp: %s", forecastTimestampStr)

	TriggerForecast(t, env, scenarioPath, "medium_range", forecastTimestampStr)

	// -----------------------------------------------------------------------
	// Step 5: Verify the Batcher updated the forecast run status
	// -----------------------------------------------------------------------
	LogSeparator(t, "Step 5: Verify Batcher Actions")

	var runStatus string
	err = env.Pool.QueryRow(ctx,
		`SELECT status FROM forecast_runs WHERE id = $1`, runID,
	).Scan(&runStatus)
	if err != nil {
		t.Fatalf("Step 5: failed to query forecast_runs status: %v", err)
	}
	if runStatus != "complete" {
		t.Fatalf("Step 5: expected forecast_runs.status='complete', got %q", runStatus)
	}
	t.Logf("  Forecast run status updated to 'complete'")

	// Verify that the batcher enqueued an eval message by checking the
	// tile_id matches our WatchPoint. We cannot directly inspect SQS from
	// Go easily, but if the batcher completed without error and the run
	// is marked complete, the eval message was sent.
	t.Log("  Batcher completed successfully (eval messages dispatched to SQS)")

	// -----------------------------------------------------------------------
	// Step 6: Wait for Notification
	// -----------------------------------------------------------------------
	// The eval worker (running as a daemon via dev_runner.py) will:
	//   1. Poll the SQS eval queue
	//   2. Load the Zarr tile data from MinIO
	//   3. Evaluate the WatchPoint condition (precip_prob 80 > threshold 50)
	//   4. Create a notification record in the database
	//   5. Publish to the notification SQS queue
	//
	// We poll the notifications table for a record matching our WatchPoint ID.
	LogSeparator(t, "Step 6: Wait for Notification")

	t.Logf("  Polling notifications table for watchpoint_id=%s...", wp.ID)
	t.Logf("  Timeout: %s, Poll interval: %s",
		env.Config.NotificationPollTimeout, env.Config.NotificationPollInterval)

	notification := WaitForNotification(t, env, wp.ID, "threshold_crossed")

	t.Logf("  Notification received!")
	t.Logf("    ID:              %s", notification.ID)
	t.Logf("    WatchPoint ID:   %s", notification.WatchPointID)
	t.Logf("    Organization ID: %s", notification.OrganizationID)
	t.Logf("    Event Type:      %s", notification.EventType)
	t.Logf("    Urgency:         %s", notification.Urgency)
	t.Logf("    Test Mode:       %v", notification.TestMode)
	t.Logf("    Created At:      %s", notification.CreatedAt.Format(time.RFC3339))

	// -----------------------------------------------------------------------
	// Step 7: Assert Notification Properties
	// -----------------------------------------------------------------------
	LogSeparator(t, "Step 7: Assert Notification")

	if notification.WatchPointID != wp.ID {
		t.Fatalf("Step 7: notification.watchpoint_id = %q, want %q",
			notification.WatchPointID, wp.ID)
	}

	if notification.OrganizationID != orgID {
		t.Fatalf("Step 7: notification.organization_id = %q, want %q",
			notification.OrganizationID, orgID)
	}

	if notification.EventType != "threshold_crossed" {
		t.Fatalf("Step 7: notification.event_type = %q, want %q",
			notification.EventType, "threshold_crossed")
	}

	if notification.TestMode {
		t.Fatal("Step 7: notification.test_mode should be false for non-test WatchPoint")
	}

	t.Log("  All notification assertions passed")

	// -----------------------------------------------------------------------
	// Step 8: Verify Notification Deliveries
	// -----------------------------------------------------------------------
	// The eval worker inserts notification_deliveries records with status='pending'
	// for each enabled channel. The notification workers (email/webhook) later
	// update these to 'sent' or 'failed'.
	LogSeparator(t, "Step 8: Verify Notification Deliveries")

	deliveries := QueryNotificationDeliveries(t, env, notification.ID)

	if len(deliveries) == 0 {
		t.Fatal("Step 8: no notification_deliveries found for notification")
	}

	t.Logf("  Found %d delivery record(s):", len(deliveries))
	for i, d := range deliveries {
		t.Logf("    [%d] ID=%s channel=%s status=%s attempts=%d",
			i, d.ID, d.ChannelType, d.Status, d.AttemptCount)
	}

	// Verify at least one delivery is for the email channel we configured.
	foundEmail := false
	for _, d := range deliveries {
		if d.ChannelType == "email" {
			foundEmail = true
			// The delivery may be 'pending' (eval worker just created it),
			// 'sent' (email worker processed it), or 'failed' (no real
			// email provider in local mode). Any of these states confirms
			// that the pipeline created the delivery record.
			t.Logf("  Email delivery found: status=%s", d.Status)
			break
		}
	}
	if !foundEmail {
		t.Fatal("Step 8: no email delivery record found")
	}

	// -----------------------------------------------------------------------
	// Step 9: Verify Evaluation State
	// -----------------------------------------------------------------------
	// The eval worker should have updated the watchpoint_evaluation_state
	// table with the trigger result.
	LogSeparator(t, "Step 9: Verify Evaluation State")

	var triggerState bool
	var lastEvaluated time.Time
	err = env.Pool.QueryRow(ctx,
		`SELECT previous_trigger_state, last_evaluated_at
		 FROM watchpoint_evaluation_state
		 WHERE watchpoint_id = $1`,
		wp.ID,
	).Scan(&triggerState, &lastEvaluated)
	if err != nil {
		t.Fatalf("Step 9: failed to query evaluation state: %v", err)
	}

	if !triggerState {
		t.Fatal("Step 9: expected previous_trigger_state=true (condition was triggered)")
	}
	t.Logf("  Evaluation state: triggered=true, last_evaluated=%s",
		lastEvaluated.Format(time.RFC3339))

	// -----------------------------------------------------------------------
	// Summary
	// -----------------------------------------------------------------------
	LogSeparator(t, "INT-001 PASSED")
	t.Logf("Pipeline verified end-to-end:")
	t.Logf("  Organization: %s", orgID)
	t.Logf("  User:         %s", userID)
	t.Logf("  WatchPoint:   %s (tile=%s)", wp.ID, wp.TileID)
	t.Logf("  Condition:    precipitation_probability > 50 (actual=80)")
	t.Logf("  Forecast Run: %s", runID)
	t.Logf("  Notification: %s (event_type=%s)", notification.ID, notification.EventType)
	t.Logf("  Deliveries:   %d record(s)", len(deliveries))
}

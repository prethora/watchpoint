//go:build e2e

// Package e2e provides integration test helpers for end-to-end testing of the
// WatchPoint platform running on the local stack.
//
// The helpers in this file orchestrate the full pipeline:
//
//	API (HTTP) -> Batcher (exec.Command stdin) -> EvalWorker (SQS) -> NotificationWorker (SQS) -> DB
//
// Each helper function encapsulates a discrete integration step (user signup,
// watchpoint creation, forecast generation + batcher invocation, notification
// polling). Tests compose these helpers to validate complete system flows
// described in the architecture's flow simulations (INT-001, INT-002, INT-004).
//
// Prerequisites:
//   - Local stack running (scripts/start-local-stack.sh)
//   - Docker Compose services healthy (postgres, localstack, minio)
//   - APP_ENV=local in environment or .env file
//
// Architecture references:
//   - flow-simulations.md INT-001 (Happy Path)
//   - flow-simulations.md INT-002 (Latency Trace)
//   - flow-simulations.md INT-004 (Monitor Mode Daily Cycle)
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// TestConfig holds addresses and timeouts for the E2E test environment.
type TestConfig struct {
	// APIURL is the base URL of the local API server (e.g., "http://localhost:8080").
	APIURL string

	// DatabaseURL is the PostgreSQL connection string for direct DB access.
	DatabaseURL string

	// ProjectRoot is the absolute path to the project root directory.
	// Used to locate binaries and scripts.
	ProjectRoot string

	// BatcherBinary is the path to the compiled batcher binary.
	// In local mode, the batcher reads S3 event JSON from stdin.
	BatcherBinary string

	// ForecastSeeder is the path to the generate_sample_forecast.py script.
	ForecastSeeder string

	// MinIOEndpoint is the MinIO S3-compatible endpoint URL.
	MinIOEndpoint string

	// LocalStackEndpoint is the LocalStack endpoint for SQS and other services.
	LocalStackEndpoint string

	// ForecastBucket is the S3 bucket name for forecast data.
	ForecastBucket string

	// NotificationPollTimeout is the maximum time to wait for a notification
	// to appear in the database after triggering the pipeline.
	NotificationPollTimeout time.Duration

	// NotificationPollInterval is how often to check for new notifications.
	NotificationPollInterval time.Duration
}

// DefaultTestConfig returns a TestConfig populated from environment variables
// with sensible defaults for the local Docker Compose stack.
func DefaultTestConfig() TestConfig {
	projectRoot := envOrDefault("PROJECT_ROOT", detectProjectRoot())
	return TestConfig{
		APIURL:                   envOrDefault("E2E_API_URL", "http://localhost:8080"),
		DatabaseURL:              envOrDefault("DATABASE_URL", "postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable"),
		ProjectRoot:              projectRoot,
		BatcherBinary:            filepath.Join(projectRoot, "cmd", "batcher"),
		ForecastSeeder:           filepath.Join(projectRoot, "scripts", "generate_sample_forecast.py"),
		MinIOEndpoint:            envOrDefault("MINIO_ENDPOINT", "http://localhost:9000"),
		LocalStackEndpoint:       envOrDefault("LOCALSTACK_ENDPOINT", "http://localhost:4566"),
		ForecastBucket:           envOrDefault("FORECAST_BUCKET", "watchpoint-forecasts"),
		NotificationPollTimeout:  30 * time.Second,
		NotificationPollInterval: 500 * time.Millisecond,
	}
}

// envOrDefault reads an environment variable or returns the fallback value.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// detectProjectRoot walks up from the current source file to find the project
// root (identified by the presence of go.mod).
func detectProjectRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// Test Environment
// ---------------------------------------------------------------------------

// TestEnv encapsulates shared state for E2E tests: database pool, HTTP client,
// and configuration. It is initialized once in TestMain and shared across tests.
type TestEnv struct {
	Config TestConfig
	Pool   *pgxpool.Pool
	Client *http.Client
}

// NewTestEnv creates and validates a new TestEnv. It connects to the database
// and verifies the schema exists. Returns an error if the environment is not
// ready (e.g., database unreachable, API server not running).
func NewTestEnv(cfg TestConfig) (*TestEnv, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create database pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database not reachable at %s: %w", cfg.DatabaseURL, err)
	}

	// Verify the schema is populated by checking for a known table.
	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'organizations')`,
	).Scan(&exists)
	if err != nil || !exists {
		pool.Close()
		return nil, fmt.Errorf("database schema not ready: organizations table not found")
	}

	// Verify the API server is reachable.
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(cfg.APIURL + "/health")
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("API server not reachable at %s: %w", cfg.APIURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		pool.Close()
		return nil, fmt.Errorf("API server health check returned %d", resp.StatusCode)
	}

	return &TestEnv{
		Config: cfg,
		Pool:   pool,
		Client: httpClient,
	}, nil
}

// Close releases resources held by the TestEnv.
func (e *TestEnv) Close() {
	if e.Pool != nil {
		e.Pool.Close()
	}
}

// ---------------------------------------------------------------------------
// Test Data Cleanup
// ---------------------------------------------------------------------------

// CleanupTestData removes all data created during a test run. This is called
// between tests or in test teardown to ensure isolation. It truncates tables
// in dependency order (child tables first) to avoid FK violations.
func (e *TestEnv) CleanupTestData(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Truncate in dependency order. CASCADE handles FK relationships.
	tables := []string{
		"notification_deliveries",
		"notifications",
		"watchpoint_evaluation_state",
		"watchpoints",
		"sessions",
		"security_events",
		"api_keys",
		"users",
		"usage_history",
		"subscription_state",
		"audit_events",
		"organizations",
		"forecast_runs",
	}

	for _, table := range tables {
		_, err := e.Pool.Exec(ctx, fmt.Sprintf("TRUNCATE TABLE %s CASCADE", table))
		if err != nil {
			// Log but don't fail -- the table might not exist in all test envs.
			t.Logf("warning: failed to truncate %s: %v", table, err)
		}
	}
}

// ---------------------------------------------------------------------------
// API Response Types
// ---------------------------------------------------------------------------

// apiResponse is a generic wrapper for the standard API envelope.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// apiErrorResponse is the standard error envelope.
type apiErrorResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// UserAndOrg holds the results from CreateUserAndOrg.
type UserAndOrg struct {
	OrganizationID string
	UserID         string
	SessionCookie  *http.Cookie
	CSRFToken      string
}

// CreatedWatchPoint holds the results from CreateWatchPoint.
type CreatedWatchPoint struct {
	ID     string
	TileID string
	Status string
}

// NotificationResult holds a row found by WaitForNotification.
type NotificationResult struct {
	ID             string
	WatchPointID   string
	OrganizationID string
	EventType      string
	Urgency        string
	TestMode       bool
	CreatedAt      time.Time
}

// ---------------------------------------------------------------------------
// Helper: CreateUserAndOrg
// ---------------------------------------------------------------------------

// CreateUserAndOrg performs the full signup flow via the HTTP API:
//  1. POST /v1/organization to create an org (which also creates the owner user).
//  2. POST /v1/auth/login to obtain a session cookie and CSRF token.
//
// The noop authenticator in local mode returns a stub Actor for all requests,
// so the organization creation succeeds without real authentication. However,
// we also perform a login to obtain a proper session cookie for subsequent
// authenticated requests.
//
// Parameters:
//   - name: Organization name
//   - email: Billing email (also used as the user's email for login)
//   - password: User password (used for login; the org creation in local mode
//     creates a user with this password via the stub auth flow)
//
// Note: In local mode with the noop authenticator, the org Create handler reads
// the Actor from context (which is the stub system actor). The user is created
// as part of the org creation flow. For login, we directly insert a user record
// with a known password hash to avoid depending on the full signup flow.
func CreateUserAndOrg(t *testing.T, env *TestEnv, name, email, password string) UserAndOrg {
	t.Helper()
	ctx := context.Background()

	// Step 1: Create org via API. The noop authenticator provides a stub Actor.
	orgReqBody, err := json.Marshal(map[string]string{
		"name":          name,
		"billing_email": email,
	})
	if err != nil {
		t.Fatalf("CreateUserAndOrg: failed to marshal org request: %v", err)
	}

	resp, err := env.Client.Post(
		env.Config.APIURL+"/v1/organization",
		"application/json",
		bytes.NewReader(orgReqBody),
	)
	if err != nil {
		t.Fatalf("CreateUserAndOrg: POST /v1/organization failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("CreateUserAndOrg: expected 201/200, got %d: %s", resp.StatusCode, string(body))
	}

	// Parse the org ID from the response.
	var orgResp apiResponse
	if err := json.Unmarshal(body, &orgResp); err != nil {
		t.Fatalf("CreateUserAndOrg: failed to parse org response: %v", err)
	}

	var orgData struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(orgResp.Data, &orgData); err != nil {
		t.Fatalf("CreateUserAndOrg: failed to parse org data: %v", err)
	}

	// Step 2: Look up the user that was created as part of org creation.
	// The org creation handler creates an owner user linked to the org.
	var userID string
	err = env.Pool.QueryRow(ctx,
		`SELECT id FROM users WHERE organization_id = $1 AND role = 'owner' LIMIT 1`,
		orgData.ID,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("CreateUserAndOrg: failed to find owner user for org %s: %v", orgData.ID, err)
	}

	// Step 3: Set a known password hash for the user so we can login.
	// The org creation in local mode may use the stub auth, so the user might
	// not have a proper password hash. We set one explicitly.
	passwordHash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("CreateUserAndOrg: failed to hash password: %v", err)
	}
	_, err = env.Pool.Exec(ctx,
		`UPDATE users SET password_hash = $1, email = $2 WHERE id = $3`,
		passwordHash, email, userID,
	)
	if err != nil {
		t.Fatalf("CreateUserAndOrg: failed to update user password: %v", err)
	}

	// Step 4: Login to get a session cookie and CSRF token.
	loginReqBody, err := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	if err != nil {
		t.Fatalf("CreateUserAndOrg: failed to marshal login request: %v", err)
	}

	loginResp, err := env.Client.Post(
		env.Config.APIURL+"/v1/auth/login",
		"application/json",
		bytes.NewReader(loginReqBody),
	)
	if err != nil {
		t.Fatalf("CreateUserAndOrg: POST /v1/auth/login failed: %v", err)
	}
	defer loginResp.Body.Close()

	loginBody, _ := io.ReadAll(loginResp.Body)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("CreateUserAndOrg: login expected 200, got %d: %s", loginResp.StatusCode, string(loginBody))
	}

	// Extract session cookie.
	var sessionCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "session_id" {
			sessionCookie = c
			break
		}
	}

	// Extract CSRF token from response body.
	var loginData apiResponse
	if err := json.Unmarshal(loginBody, &loginData); err != nil {
		t.Fatalf("CreateUserAndOrg: failed to parse login response: %v", err)
	}

	var authData struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(loginData.Data, &authData); err != nil {
		t.Fatalf("CreateUserAndOrg: failed to parse auth data: %v", err)
	}

	return UserAndOrg{
		OrganizationID: orgData.ID,
		UserID:         userID,
		SessionCookie:  sessionCookie,
		CSRFToken:      authData.CSRFToken,
	}
}

// hashPassword produces a bcrypt hash suitable for storage. This is a test-only
// helper to set known passwords for E2E test users.
func hashPassword(password string) (string, error) {
	// Import golang.org/x/crypto/bcrypt at the package level would create a
	// dependency. Instead, we insert password hashes directly via SQL using
	// a pre-computed hash or call bcrypt from the test.
	// For simplicity, we use a fixed cost factor appropriate for testing.
	//
	// We use the same bcrypt package that the auth service uses.
	// This import is already in go.mod via golang.org/x/crypto.
	return hashPasswordBcrypt(password)
}

// ---------------------------------------------------------------------------
// Helper: AuthenticatedRequest
// ---------------------------------------------------------------------------

// AuthenticatedRequest creates an HTTP request with the Authorization header
// and CSRF token from a UserAndOrg result.
//
// In local mode with the noop authenticator, the Authorization header carries
// a Bearer token that the noop authenticator accepts (it accepts any token).
// The session cookie from login is also attached for completeness, though the
// noop authenticator resolves via Bearer token, not cookie.
//
// The CSRF token is set in the X-CSRF-Token header as required by the API's
// CSRF middleware.
func AuthenticatedRequest(t *testing.T, method, url string, body io.Reader, auth UserAndOrg) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("AuthenticatedRequest: failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// The auth middleware requires a Bearer token in the Authorization header.
	// In local mode with the noop authenticator, any token is accepted.
	// We use the session ID if available, otherwise a placeholder.
	bearerToken := "e2e-local-token"
	if auth.SessionCookie != nil {
		bearerToken = auth.SessionCookie.Value
		req.AddCookie(auth.SessionCookie)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	if auth.CSRFToken != "" {
		req.Header.Set("X-CSRF-Token", auth.CSRFToken)
	}
	return req
}

// ---------------------------------------------------------------------------
// Helper: CreateWatchPoint
// ---------------------------------------------------------------------------

// CreateWatchPoint creates a WatchPoint via the HTTP API.
//
// IMPORTANT: In local mode with the noop authenticator, the WatchPoint will be
// created under the stub org ("org_stub") because the auth middleware injects a
// fixed Actor. If you need the WatchPoint to be associated with the real
// organization from CreateUserAndOrg, use CreateWatchPointDirect instead.
//
// Parameters:
//   - auth: Authentication context from CreateUserAndOrg
//   - wpJSON: The raw JSON body for POST /v1/watchpoints
//
// Returns the created WatchPoint's ID, tile_id, and status.
//
// The caller is responsible for constructing the full JSON payload including
// name, location, timezone, time_window or monitor_config, conditions,
// condition_logic, and channels. This keeps the helper flexible for different
// test scenarios (event mode, monitor mode, various conditions).
func CreateWatchPoint(t *testing.T, env *TestEnv, auth UserAndOrg, wpJSON []byte) CreatedWatchPoint {
	t.Helper()

	req := AuthenticatedRequest(t, http.MethodPost, env.Config.APIURL+"/v1/watchpoints", bytes.NewReader(wpJSON), auth)
	resp, err := env.Client.Do(req)
	if err != nil {
		t.Fatalf("CreateWatchPoint: POST /v1/watchpoints failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("CreateWatchPoint: expected 201, got %d: %s", resp.StatusCode, string(body))
	}

	var envelope apiResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("CreateWatchPoint: failed to parse response envelope: %v", err)
	}

	var wp struct {
		ID     string `json:"id"`
		TileID string `json:"tile_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(envelope.Data, &wp); err != nil {
		t.Fatalf("CreateWatchPoint: failed to parse watchpoint data: %v", err)
	}

	return CreatedWatchPoint{
		ID:     wp.ID,
		TileID: wp.TileID,
		Status: wp.Status,
	}
}

// ---------------------------------------------------------------------------
// Helper: CreateWatchPointDirect
// ---------------------------------------------------------------------------

// CreateWatchPointDirect inserts a WatchPoint directly into the database,
// bypassing the API layer. This is the preferred method for E2E tests because
// it correctly associates the WatchPoint with the real organization (avoiding
// the noop authenticator's org_stub limitation).
//
// The tile_id column is GENERATED ALWAYS in PostgreSQL, computed from the
// location coordinates. It does not need to be provided.
//
// Parameters:
//   - orgID: Organization ID (from CreateUserAndOrg)
//   - name: WatchPoint name
//   - lat, lon: Location coordinates
//   - conditions: JSONB conditions array (marshalled to JSON)
//   - conditionLogic: "ANY" or "ALL"
//   - channels: JSONB channels array (marshalled to JSON)
//   - timeWindowStart, timeWindowEnd: Event time window (nil for monitor mode)
//   - monitorConfig: Monitor config JSON (nil for event mode)
func CreateWatchPointDirect(
	t *testing.T,
	env *TestEnv,
	orgID string,
	name string,
	lat, lon float64,
	conditions []map[string]interface{},
	conditionLogic string,
	channels []map[string]interface{},
	timeWindowStart, timeWindowEnd *time.Time,
	monitorConfig map[string]interface{},
) CreatedWatchPoint {
	t.Helper()
	ctx := context.Background()

	wpID := "wp_" + uuid.New().String()
	now := time.Now().UTC()

	condJSON, err := json.Marshal(conditions)
	if err != nil {
		t.Fatalf("CreateWatchPointDirect: failed to marshal conditions: %v", err)
	}

	chanJSON, err := json.Marshal(channels)
	if err != nil {
		t.Fatalf("CreateWatchPointDirect: failed to marshal channels: %v", err)
	}

	var monitorJSON []byte
	if monitorConfig != nil {
		monitorJSON, err = json.Marshal(monitorConfig)
		if err != nil {
			t.Fatalf("CreateWatchPointDirect: failed to marshal monitor_config: %v", err)
		}
	}

	_, err = env.Pool.Exec(ctx,
		`INSERT INTO watchpoints (
			id, organization_id, name,
			location_lat, location_lon, location_display_name,
			timezone,
			time_window_start, time_window_end, monitor_config,
			conditions, condition_logic,
			channels, template_set,
			status, test_mode, tags, config_version, source,
			created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7,
			$8, $9, $10,
			$11, $12,
			$13, $14,
			$15, $16, $17, $18, $19,
			$20, $21
		)`,
		wpID,
		orgID,
		name,
		lat,
		lon,
		"E2E Test Location",
		"America/New_York",
		timeWindowStart,
		timeWindowEnd,
		nilByteSlice(monitorJSON),
		condJSON,
		conditionLogic,
		chanJSON,
		"default",
		"active",
		false, // test_mode
		nil,   // tags
		1,     // config_version
		"e2e_test",
		now,
		now,
	)
	if err != nil {
		t.Fatalf("CreateWatchPointDirect: INSERT failed: %v", err)
	}

	// Read back the generated tile_id.
	var tileID string
	err = env.Pool.QueryRow(ctx,
		`SELECT tile_id FROM watchpoints WHERE id = $1`, wpID,
	).Scan(&tileID)
	if err != nil {
		t.Fatalf("CreateWatchPointDirect: failed to read tile_id: %v", err)
	}

	return CreatedWatchPoint{
		ID:     wpID,
		TileID: tileID,
		Status: "active",
	}
}

// nilByteSlice returns nil if the byte slice is nil or empty, otherwise returns the slice.
// This is used to pass NULL to PostgreSQL for optional JSONB columns.
func nilByteSlice(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Helper: TriggerForecast
// ---------------------------------------------------------------------------

// TriggerForecast executes the forecast seeder script to generate deterministic
// Zarr data in MinIO, then invokes the Batcher via exec.Command with the S3
// event JSON piped to stdin (APP_ENV=local mode).
//
// Parameters:
//   - scenarioPath: Absolute path to the scenario JSON file
//     (e.g., scripts/scenarios/int001_precip_trigger.json)
//   - model: Forecast model type ("medium_range" or "nowcast")
//   - timestamp: The forecast run timestamp in RFC3339 format
//
// The function:
//  1. Runs generate_sample_forecast.py with the scenario to seed MinIO.
//  2. Constructs a manual RunContext JSON payload for the batcher.
//  3. Runs `go run cmd/batcher` with APP_ENV=local, piping the JSON to stdin.
//
// Both steps must succeed for the function to return without calling t.Fatal.
func TriggerForecast(t *testing.T, env *TestEnv, scenarioPath, model, timestamp string) {
	t.Helper()

	outputPath := fmt.Sprintf("s3://%s/%s/%s", env.Config.ForecastBucket, model, timestamp)

	// Step 1: Seed forecast data in MinIO via the scenario.
	t.Logf("TriggerForecast: seeding forecast data at %s", outputPath)
	seederCmd := exec.Command(
		"python3",
		env.Config.ForecastSeeder,
		"--scenario", scenarioPath,
		"--output", outputPath,
		"--endpoint-url", env.Config.MinIOEndpoint,
	)
	seederCmd.Dir = env.Config.ProjectRoot
	seederCmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID=minioadmin",
		"AWS_SECRET_ACCESS_KEY=minioadmin",
		"AWS_DEFAULT_REGION=us-east-1",
	)

	seederOut, err := seederCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TriggerForecast: forecast seeder failed: %v\nOutput: %s", err, string(seederOut))
	}
	t.Logf("TriggerForecast: seeder completed successfully")

	// Step 2: Invoke the batcher with a manual RunContext JSON on stdin.
	// The batcher in local mode (APP_ENV=local) reads JSON from stdin.
	// We use the manual RunContext format: {"model":"...","timestamp":"..."}
	// which the batcher's Handler method also supports (alongside S3 events).
	batcherInput, err := json.Marshal(map[string]string{
		"model":     model,
		"timestamp": timestamp,
	})
	if err != nil {
		t.Fatalf("TriggerForecast: failed to marshal batcher input: %v", err)
	}

	t.Logf("TriggerForecast: invoking batcher with input: %s", string(batcherInput))
	batcherCmd := exec.Command("go", "run", env.Config.BatcherBinary)
	batcherCmd.Dir = env.Config.ProjectRoot
	batcherCmd.Stdin = bytes.NewReader(batcherInput)
	batcherCmd.Env = append(os.Environ(),
		"APP_ENV=local",
		"FORECAST_BUCKET="+env.Config.ForecastBucket,
		fmt.Sprintf("SQS_EVAL_URGENT=%s/000000000000/eval-queue-urgent", env.Config.LocalStackEndpoint),
		fmt.Sprintf("SQS_EVAL_STANDARD=%s/000000000000/eval-queue-standard", env.Config.LocalStackEndpoint),
		fmt.Sprintf("DATABASE_URL=%s", env.Config.DatabaseURL),
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_DEFAULT_REGION=us-east-1",
		fmt.Sprintf("AWS_ENDPOINT_URL=%s", env.Config.LocalStackEndpoint),
	)

	batcherOut, err := batcherCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TriggerForecast: batcher failed: %v\nOutput: %s", err, string(batcherOut))
	}
	t.Logf("TriggerForecast: batcher completed successfully")
}

// ---------------------------------------------------------------------------
// Helper: WaitForNotification
// ---------------------------------------------------------------------------

// WaitForNotification polls the notifications table until at least one
// notification matching the given criteria appears, or the timeout expires.
//
// Parameters:
//   - watchpointID: The WatchPoint ID to filter on
//   - eventType: (optional) If non-empty, filters by event_type
//
// Returns the first matching notification. If the timeout expires before a
// match is found, the test is failed with t.Fatal.
//
// This helper accounts for the asynchronous nature of the notification pipeline:
// EvalWorker -> SQS -> NotificationWorker -> DB insert. The poll interval and
// timeout are configurable via TestConfig.
func WaitForNotification(t *testing.T, env *TestEnv, watchpointID, eventType string) NotificationResult {
	t.Helper()

	deadline := time.Now().Add(env.Config.NotificationPollTimeout)
	query := `SELECT id, watchpoint_id, organization_id, event_type, urgency, test_mode, created_at
		FROM notifications WHERE watchpoint_id = $1`
	args := []interface{}{watchpointID}

	if eventType != "" {
		query += " AND event_type = $2"
		args = append(args, eventType)
	}
	query += " ORDER BY created_at DESC LIMIT 1"

	for time.Now().Before(deadline) {
		var result NotificationResult
		err := env.Pool.QueryRow(context.Background(), query, args...).Scan(
			&result.ID,
			&result.WatchPointID,
			&result.OrganizationID,
			&result.EventType,
			&result.Urgency,
			&result.TestMode,
			&result.CreatedAt,
		)
		if err == nil {
			t.Logf("WaitForNotification: found notification %s (event_type=%s)", result.ID, result.EventType)
			return result
		}

		time.Sleep(env.Config.NotificationPollInterval)
	}

	t.Fatalf("WaitForNotification: timed out after %s waiting for notification (watchpoint_id=%s, event_type=%s)",
		env.Config.NotificationPollTimeout, watchpointID, eventType)
	return NotificationResult{} // unreachable
}

// ---------------------------------------------------------------------------
// Helper: WaitForEvaluationState
// ---------------------------------------------------------------------------

// WaitForEvaluationState polls the watchpoint_evaluation_state table until a
// record for the given WatchPoint appears with the expected trigger state, or
// the timeout expires.
//
// This is useful for verifying that the eval worker processed the forecast data
// and updated the evaluation state, even if no notification was generated (e.g.,
// monitor mode baseline establishment in INT-004).
func WaitForEvaluationState(t *testing.T, env *TestEnv, watchpointID string, expectTriggered *bool) {
	t.Helper()

	deadline := time.Now().Add(env.Config.NotificationPollTimeout)

	for time.Now().Before(deadline) {
		var triggered *bool
		var lastEvaluated *time.Time
		err := env.Pool.QueryRow(context.Background(),
			`SELECT previous_trigger_state, last_evaluated_at
			 FROM watchpoint_evaluation_state
			 WHERE watchpoint_id = $1`,
			watchpointID,
		).Scan(&triggered, &lastEvaluated)

		if err == nil && lastEvaluated != nil {
			if expectTriggered == nil {
				// Just checking that evaluation happened, regardless of trigger state.
				t.Logf("WaitForEvaluationState: evaluation recorded at %s (triggered=%v)", lastEvaluated, triggered)
				return
			}
			if triggered != nil && *triggered == *expectTriggered {
				t.Logf("WaitForEvaluationState: trigger state matches (triggered=%v)", *triggered)
				return
			}
		}

		time.Sleep(env.Config.NotificationPollInterval)
	}

	t.Fatalf("WaitForEvaluationState: timed out after %s waiting for evaluation state (watchpoint_id=%s)",
		env.Config.NotificationPollTimeout, watchpointID)
}

// ---------------------------------------------------------------------------
// Helper: QueryNotificationDeliveries
// ---------------------------------------------------------------------------

// DeliveryResult holds a row from the notification_deliveries table.
type DeliveryResult struct {
	ID             string
	NotificationID string
	ChannelType    string
	Status         string
	AttemptCount   int
}

// QueryNotificationDeliveries returns all delivery records for a given notification ID.
// This is useful for verifying that the notification worker dispatched to the
// correct channels (email, webhook) and tracking delivery status.
func QueryNotificationDeliveries(t *testing.T, env *TestEnv, notificationID string) []DeliveryResult {
	t.Helper()

	rows, err := env.Pool.Query(context.Background(),
		`SELECT id, notification_id, channel_type, status, attempt_count
		 FROM notification_deliveries
		 WHERE notification_id = $1
		 ORDER BY id ASC`,
		notificationID,
	)
	if err != nil {
		t.Fatalf("QueryNotificationDeliveries: query failed: %v", err)
	}
	defer rows.Close()

	var results []DeliveryResult
	for rows.Next() {
		var d DeliveryResult
		if err := rows.Scan(&d.ID, &d.NotificationID, &d.ChannelType, &d.Status, &d.AttemptCount); err != nil {
			t.Fatalf("QueryNotificationDeliveries: scan failed: %v", err)
		}
		results = append(results, d)
	}
	return results
}

// ---------------------------------------------------------------------------
// Helper: QueryDB (generic)
// ---------------------------------------------------------------------------

// QueryDBScalar executes a query and scans a single scalar value. Useful for
// quick assertions like counting rows or checking existence.
func QueryDBScalar[T any](t *testing.T, env *TestEnv, query string, args ...interface{}) T {
	t.Helper()
	var result T
	err := env.Pool.QueryRow(context.Background(), query, args...).Scan(&result)
	if err != nil {
		t.Fatalf("QueryDBScalar: query failed: %v\nQuery: %s", err, query)
	}
	return result
}

// ---------------------------------------------------------------------------
// Helper: BuildWatchPointJSON
// ---------------------------------------------------------------------------

// EventModeWatchPointJSON builds a JSON payload for creating an event-mode WatchPoint.
// This is a convenience function for the common INT-001 scenario.
//
// Parameters:
//   - name: WatchPoint name
//   - lat, lon: Location coordinates
//   - variable: The weather variable to monitor (e.g., "precipitation_probability")
//   - operator: Comparison operator (e.g., ">")
//   - threshold: The threshold value
//   - startTime, endTime: Event time window
//   - channelType: Notification channel type ("email" or "webhook")
//   - channelConfig: Channel configuration map
func EventModeWatchPointJSON(
	name string,
	lat, lon float64,
	variable, operator string,
	threshold float64,
	startTime, endTime time.Time,
	channelType string,
	channelConfig map[string]interface{},
) ([]byte, error) {
	payload := map[string]interface{}{
		"name": name,
		"location": map[string]interface{}{
			"lat":          lat,
			"lon":          lon,
			"display_name": "E2E Test Location",
		},
		"timezone": "America/New_York",
		"time_window": map[string]interface{}{
			"start": startTime.Format(time.RFC3339),
			"end":   endTime.Format(time.RFC3339),
		},
		"conditions": []map[string]interface{}{
			{
				"variable":  variable,
				"operator":  operator,
				"threshold": threshold,
			},
		},
		"condition_logic": "ANY",
		"channels": []map[string]interface{}{
			{
				"type":    channelType,
				"config":  channelConfig,
				"enabled": true,
			},
		},
		"template_set": "default",
	}
	return json.Marshal(payload)
}

// MonitorModeWatchPointJSON builds a JSON payload for creating a monitor-mode WatchPoint.
// This is a convenience function for the INT-004 scenario.
func MonitorModeWatchPointJSON(
	name string,
	lat, lon float64,
	variables []string,
	channelType string,
	channelConfig map[string]interface{},
) ([]byte, error) {
	conditions := make([]map[string]interface{}, len(variables))
	for i, v := range variables {
		conditions[i] = map[string]interface{}{
			"variable":  v,
			"operator":  ">",
			"threshold": 0,
		}
	}

	payload := map[string]interface{}{
		"name": name,
		"location": map[string]interface{}{
			"lat":          lat,
			"lon":          lon,
			"display_name": "E2E Test Location",
		},
		"timezone": "America/New_York",
		"monitor_config": map[string]interface{}{
			"frequency":    "daily",
			"digest_hour":  8,
			"digest_minute": 0,
		},
		"conditions":      conditions,
		"condition_logic": "ANY",
		"channels": []map[string]interface{}{
			{
				"type":    channelType,
				"config":  channelConfig,
				"enabled": true,
			},
		},
		"template_set": "default",
	}
	return json.Marshal(payload)
}

// ---------------------------------------------------------------------------
// Helper: AssertAPIError
// ---------------------------------------------------------------------------

// AssertAPIError verifies that an HTTP response contains an error with the
// expected status code and error code.
func AssertAPIError(t *testing.T, resp *http.Response, expectedStatus int, expectedCode string) {
	t.Helper()
	if resp.StatusCode != expectedStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("AssertAPIError: expected status %d, got %d: %s", expectedStatus, resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	var errResp apiErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("AssertAPIError: failed to parse error response: %v\nBody: %s", err, string(body))
	}

	if expectedCode != "" && errResp.Error.Code != expectedCode {
		t.Fatalf("AssertAPIError: expected error code %q, got %q", expectedCode, errResp.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// Helper: LogSeparator
// ---------------------------------------------------------------------------

// LogSeparator prints a visible separator in test output for readability.
func LogSeparator(t *testing.T, label string) {
	t.Helper()
	t.Logf("\n%s %s %s", strings.Repeat("=", 20), label, strings.Repeat("=", 20))
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ValidationResult holds the outcome of a validation check. It provides
// both a boolean pass/fail signal and a human-readable message suitable
// for display in the bootstrap CLI.
type ValidationResult struct {
	// Valid is true if the input passed all validation checks.
	Valid bool

	// Message is a human-readable description of the result.
	// On success, it describes what was validated (e.g., "Stripe account verified: Acme Corp").
	// On failure, it describes why validation failed.
	Message string
}

// HTTPClient is the interface used by validators that make outbound HTTP calls.
// It enables injecting mock HTTP transports for testing without making real
// network calls.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// DatabaseConnector abstracts the database connection logic for testing.
// In production, the real implementation uses pgx.Connect. Tests inject
// a mock that simulates connection success/failure.
type DatabaseConnector interface {
	// Connect attempts to establish a connection to the database at the
	// given DSN. It returns an error if the connection fails.
	// The implementation MUST close the connection before returning.
	Connect(ctx context.Context, dsn string) error
}

// PgxConnector is the production implementation of DatabaseConnector.
// It uses pgx.Connect to make a real TCP connection to the database.
type PgxConnector struct{}

// Connect establishes a connection to the database using pgx and immediately
// closes it. The purpose is to verify that the DSN is reachable and the
// credentials are valid — not to maintain an open connection.
func (c *PgxConnector) Connect(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

// Validator encapsulates the dependencies needed by input validation functions.
// It is constructed during bootstrap initialization and threaded through
// the validation phases.
type Validator struct {
	httpClient HTTPClient
	dbConn     DatabaseConnector
}

// NewValidator creates a Validator with production dependencies: a real
// HTTP client with a 10-second timeout and a real pgx connector.
func NewValidator() *Validator {
	return &Validator{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		dbConn: &PgxConnector{},
	}
}

// NewValidatorWithDeps creates a Validator with injected dependencies
// for testing.
func NewValidatorWithDeps(httpClient HTTPClient, dbConn DatabaseConnector) *Validator {
	return &Validator{
		httpClient: httpClient,
		dbConn:     dbConn,
	}
}

// validateTimeout is the per-probe timeout for active validation calls.
// This is separate from the HTTP client timeout to serve as an outer bound
// that also covers DNS resolution, TLS handshake, etc.
const validateTimeout = 15 * time.Second

// ---------------------------------------------------------------------------
// ValidateDatabaseURL
// ---------------------------------------------------------------------------

// ValidateDatabaseURL validates a Supabase PostgreSQL connection string.
//
// Validation steps:
//  1. Parse the URL to extract the host and port.
//  2. Verify the port is 6543 (Supabase Transaction Mode via PgBouncer).
//     This is a hard requirement from 13-human-setup.md Section 3.1.
//  3. Attempt an actual connection using pgx to verify the credentials
//     and network reachability.
//
// The connection is immediately closed after verification. This function
// does not maintain a persistent connection.
func (v *Validator) ValidateDatabaseURL(ctx context.Context, rawURL string) ValidationResult {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ValidationResult{Valid: false, Message: "database URL must not be empty"}
	}

	// Parse the URL to extract the port.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("invalid URL format: %v", err),
		}
	}

	// Verify the scheme is postgres or postgresql.
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("expected postgres:// or postgresql:// scheme, got %q", parsed.Scheme),
		}
	}

	// Extract port. url.Parse puts it in parsed.Host as "host:port".
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		// If no port is specified, that's also wrong — we require 6543.
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("could not extract port from host %q: %v (port 6543 is required for Supabase Transaction Mode)", parsed.Host, err),
		}
	}

	if port != "6543" {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("port must be 6543 (Supabase Transaction Mode), got %q", port),
		}
	}

	// Attempt a real connection to verify credentials and reachability.
	connCtx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	if err := v.dbConn.Connect(connCtx, rawURL); err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("connection failed: %v", err),
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: fmt.Sprintf("database connection verified (host=%s, port=%s)", parsed.Hostname(), port),
	}
}

// ---------------------------------------------------------------------------
// ValidateStripeKey
// ---------------------------------------------------------------------------

// stripeKeyRegex validates the format of a Stripe secret key.
// Format: sk_(test|live)_ followed by 24+ alphanumeric characters.
// Reference: 13-human-setup.md Section 3.2
var stripeKeyRegex = regexp.MustCompile(`^sk_(test|live)_[0-9a-zA-Z]{24,}$`)

// ValidateStripeKey validates a Stripe secret key by:
//  1. Checking the key format matches sk_(test|live)_[a-zA-Z0-9]{24+}.
//  2. Making a lightweight GET request to https://api.stripe.com/v1/account
//     to verify the key is functional.
//
// The /v1/account endpoint returns the connected account details and is the
// lightest-weight endpoint that verifies key validity without side effects.
func (v *Validator) ValidateStripeKey(ctx context.Context, key string) ValidationResult {
	key = strings.TrimSpace(key)
	if key == "" {
		return ValidationResult{Valid: false, Message: "Stripe secret key must not be empty"}
	}

	if !stripeKeyRegex.MatchString(key) {
		return ValidationResult{
			Valid:   false,
			Message: "Stripe secret key must match format sk_(test|live)_[alphanumeric 24+ chars]",
		}
	}

	// Active probe: GET /v1/account
	probeCtx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://api.stripe.com/v1/account", nil)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("failed to create request: %v", err),
		}
	}

	// Stripe uses Bearer authentication for API keys.
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "WatchPoint-Bootstrap/1.0")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("Stripe API probe failed: %v", err),
		}
	}
	defer resp.Body.Close()

	// Read and discard the body to allow connection reuse.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusUnauthorized {
		return ValidationResult{
			Valid:   false,
			Message: "Stripe API returned 401 Unauthorized: key is invalid or revoked",
		}
	}

	if resp.StatusCode != http.StatusOK {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("Stripe API returned HTTP %d: %s", resp.StatusCode, truncateBody(body, 200)),
		}
	}

	// Extract the account display name for user feedback.
	var account struct {
		ID              string `json:"id"`
		BusinessProfile struct {
			Name string `json:"name"`
		} `json:"business_profile"`
	}
	displayInfo := ""
	if err := json.Unmarshal(body, &account); err == nil {
		if account.BusinessProfile.Name != "" {
			displayInfo = fmt.Sprintf(" (account: %s, name: %s)", account.ID, account.BusinessProfile.Name)
		} else if account.ID != "" {
			displayInfo = fmt.Sprintf(" (account: %s)", account.ID)
		}
	}

	// Detect test vs live mode from the key prefix.
	mode := "test"
	if strings.HasPrefix(key, "sk_live_") {
		mode = "live"
	}

	return ValidationResult{
		Valid:   true,
		Message: fmt.Sprintf("Stripe key verified [%s mode]%s", mode, displayInfo),
	}
}

// ---------------------------------------------------------------------------
// ValidateSendGridKey
// ---------------------------------------------------------------------------

// ValidateSendGridKey validates a SendGrid API key by making a GET request
// to https://api.sendgrid.com/v3/user/credits.
//
// Reference: 13-human-setup.md Section 3.3 — the validation uses curl to
// check the /v3/user/credits endpoint and looks for "remain" in the response.
func (v *Validator) ValidateSendGridKey(ctx context.Context, key string) ValidationResult {
	key = strings.TrimSpace(key)
	if key == "" {
		return ValidationResult{Valid: false, Message: "SendGrid API key must not be empty"}
	}

	// SendGrid keys typically start with "SG." but the spec only validates
	// via the API call. We do a basic prefix check as an early guard.
	if !strings.HasPrefix(key, "SG.") {
		return ValidationResult{
			Valid:   false,
			Message: "SendGrid API key should start with 'SG.'",
		}
	}

	// Active probe: GET /v3/user/credits
	probeCtx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://api.sendgrid.com/v3/user/credits", nil)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("failed to create request: %v", err),
		}
	}

	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "WatchPoint-Bootstrap/1.0")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("SendGrid API probe failed: %v", err),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("SendGrid API returned HTTP %d: key is invalid or lacks permissions", resp.StatusCode),
		}
	}

	if resp.StatusCode != http.StatusOK {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("SendGrid API returned HTTP %d: %s", resp.StatusCode, truncateBody(body, 200)),
		}
	}

	// Per the spec, validate the response contains "remain" indicating
	// the credits endpoint returned meaningful data.
	if !strings.Contains(string(body), "remain") {
		return ValidationResult{
			Valid:   false,
			Message: "SendGrid API response did not contain expected credit information",
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: "SendGrid API key verified (credits endpoint accessible)",
	}
}

// ---------------------------------------------------------------------------
// ValidateRunPodKey
// ---------------------------------------------------------------------------

// ValidateRunPodKey validates a RunPod API key using a length check only.
//
// Reference: 13-human-setup.md Section 3.4 — the spec states "Length > 20"
// as the only validation. The RunPod API requires specific scopes that
// cannot be verified without invoking an endpoint, so we rely on length.
func (v *Validator) ValidateRunPodKey(_ context.Context, key string) ValidationResult {
	key = strings.TrimSpace(key)
	if key == "" {
		return ValidationResult{Valid: false, Message: "RunPod API key must not be empty"}
	}

	if len(key) <= 20 {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("RunPod API key must be longer than 20 characters (got %d)", len(key)),
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: fmt.Sprintf("RunPod API key accepted (length: %d chars)", len(key)),
	}
}

// ---------------------------------------------------------------------------
// ValidateRegex
// ---------------------------------------------------------------------------

// ValidateRegex is a generic validator that checks whether the input matches
// the given regular expression pattern. It is used as a fallback for inputs
// that cannot be actively probed, such as OAuth Client IDs.
//
// Reference: 13-human-setup.md Section 3.5 — OAuth providers use regex
// validation for Client IDs (active probing requires the secret as well).
func (v *Validator) ValidateRegex(_ context.Context, input, pattern, fieldName string) ValidationResult {
	input = strings.TrimSpace(input)
	if input == "" {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("%s must not be empty", fieldName),
		}
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("invalid regex pattern %q: %v", pattern, err),
		}
	}

	if !re.MatchString(input) {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("%s does not match expected format (pattern: %s)", fieldName, pattern),
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: fmt.Sprintf("%s format validated", fieldName),
	}
}

// ---------------------------------------------------------------------------
// ValidateRunPodEndpointID
// ---------------------------------------------------------------------------

// runPodEndpointRegex validates the RunPod Endpoint ID format.
// Reference: 13-human-setup.md Section 6.3 — Regex `^[a-zA-Z0-9-]{10,}$`
var runPodEndpointRegex = regexp.MustCompile(`^[a-zA-Z0-9-]{10,}$`)

// ValidateRunPodEndpointID validates a RunPod Endpoint ID using the regex
// pattern from 13-human-setup.md Section 6.3.
func (v *Validator) ValidateRunPodEndpointID(_ context.Context, id string) ValidationResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return ValidationResult{Valid: false, Message: "RunPod Endpoint ID must not be empty"}
	}

	if !runPodEndpointRegex.MatchString(id) {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("RunPod Endpoint ID must match pattern [a-zA-Z0-9-]{10,} (got %q)", id),
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: fmt.Sprintf("RunPod Endpoint ID format validated (%s)", id),
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// truncateBody returns the first n bytes of body as a string, appending
// "..." if truncation occurred. This is used for including partial API
// response bodies in error messages without overwhelming the user.
func truncateBody(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "..."
}

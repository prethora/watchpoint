package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"

	"watchpoint/internal/types"
)

// testLogger returns a discard logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// -- Test structs for custom validation tags --

type testSSRFStruct struct {
	WebhookURL string `validate:"ssrf_url"`
}

type testRequiredSSRFStruct struct {
	WebhookURL string `validate:"required,ssrf_url"`
}

type testTimezoneStruct struct {
	Timezone string `validate:"required,is_timezone"`
}

type testCONUSStruct struct {
	Lat float64 `validate:"is_conus"`
	Lon float64
}

type testRequiredStruct struct {
	Name  string `validate:"required"`
	Email string `validate:"required,email"`
}

// -- ValidationResult tests --

func TestValidationResult_IsValid(t *testing.T) {
	t.Run("empty result is valid", func(t *testing.T) {
		r := ValidationResult{}
		if !r.IsValid() {
			t.Error("expected empty ValidationResult to be valid")
		}
	})

	t.Run("result with errors is not valid", func(t *testing.T) {
		r := ValidationResult{
			Errors: []ValidationError{{Field: "name", Code: "required", Message: "required"}},
		}
		if r.IsValid() {
			t.Error("expected ValidationResult with errors to be invalid")
		}
	})

	t.Run("result with only warnings is valid", func(t *testing.T) {
		r := ValidationResult{
			Warnings: []string{"deprecated webhook platform"},
		}
		if !r.IsValid() {
			t.Error("expected ValidationResult with only warnings to be valid")
		}
	})
}

// -- NewValidator tests --

func TestNewValidator(t *testing.T) {
	v := NewValidator(testLogger())
	if v == nil {
		t.Fatal("NewValidator returned nil")
	}
	if v.validate == nil {
		t.Error("expected validate field to be non-nil")
	}
	if v.logger == nil {
		t.Error("expected logger field to be non-nil")
	}
}

// -- ValidateStruct tests --

func TestValidateStruct_Success(t *testing.T) {
	v := NewValidator(testLogger())

	req := testRequiredStruct{
		Name:  "Test",
		Email: "test@example.com",
	}

	err := v.ValidateStruct(req)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

func TestValidateStruct_Failure_ReturnsAppError(t *testing.T) {
	v := NewValidator(testLogger())

	req := testRequiredStruct{
		Name:  "",
		Email: "not-an-email",
	}

	err := v.ValidateStruct(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	// The error code should map to the first validation failure.
	if appErr.Code != types.ErrCodeValidationMissingField {
		t.Errorf("expected code %s, got %s", types.ErrCodeValidationMissingField, appErr.Code)
	}

	// Details should contain validation_errors.
	if appErr.Details == nil {
		t.Fatal("expected non-nil details")
	}
	ve, ok := appErr.Details["validation_errors"]
	if !ok {
		t.Fatal("expected validation_errors key in details")
	}
	errs, ok := ve.([]ValidationError)
	if !ok {
		t.Fatalf("expected []ValidationError, got %T", ve)
	}
	if len(errs) < 2 {
		t.Errorf("expected at least 2 validation errors, got %d", len(errs))
	}
}

// -- ValidateStructWithWarnings tests --

func TestValidateStructWithWarnings_Valid(t *testing.T) {
	v := NewValidator(testLogger())

	req := testRequiredStruct{
		Name:  "Test",
		Email: "test@example.com",
	}

	result := v.ValidateStructWithWarnings(req)
	if !result.IsValid() {
		t.Errorf("expected valid result, got errors: %v", result.Errors)
	}
}

func TestValidateStructWithWarnings_Invalid(t *testing.T) {
	v := NewValidator(testLogger())

	req := testRequiredStruct{
		Name:  "",
		Email: "bad",
	}

	result := v.ValidateStructWithWarnings(req)
	if result.IsValid() {
		t.Error("expected invalid result")
	}
	if len(result.Errors) < 2 {
		t.Errorf("expected at least 2 errors, got %d", len(result.Errors))
	}

	// Check that proper codes are set.
	codeMap := make(map[string]bool)
	for _, e := range result.Errors {
		codeMap[e.Code] = true
	}
	if !codeMap[string(types.ErrCodeValidationMissingField)] {
		t.Error("expected validation_missing_required_field code for empty Name")
	}
	if !codeMap[string(types.ErrCodeValidationInvalidEmail)] {
		t.Error("expected validation_invalid_email code for bad Email")
	}
}

// -- SSRF URL validation tests --

func TestValidateSSRFURL_ValidHTTPS(t *testing.T) {
	// Inject a mock DNS resolver that returns a safe public IP.
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://example.com/webhook"}
	err := v.ValidateStruct(req)
	if err != nil {
		t.Errorf("expected valid HTTPS URL to pass, got: %v", err)
	}
}

func TestValidateSSRFURL_RejectsHTTP(t *testing.T) {
	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "http://example.com/webhook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected HTTP URL to fail SSRF validation")
	}

	var appErr *types.AppError
	if errors.As(err, &appErr) {
		if appErr.Code != types.ErrCodeValidationInvalidWebhook {
			t.Errorf("expected code %s, got %s", types.ErrCodeValidationInvalidWebhook, appErr.Code)
		}
	}
}

func TestValidateSSRFURL_RejectsPrivateIP(t *testing.T) {
	// Inject a mock DNS resolver that returns a private IP (10.0.0.1).
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://evil.internal/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected URL resolving to private IP to fail SSRF validation")
	}

	var appErr *types.AppError
	if errors.As(err, &appErr) {
		if appErr.Code != types.ErrCodeValidationInvalidWebhook {
			t.Errorf("expected code %s, got %s", types.ErrCodeValidationInvalidWebhook, appErr.Code)
		}
	}
}

func TestValidateSSRFURL_RejectsLocalhost(t *testing.T) {
	// Inject a mock DNS resolver that returns localhost.
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://localhost/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected URL resolving to localhost to fail SSRF validation")
	}
}

func TestValidateSSRFURL_RejectsMetadataIP(t *testing.T) {
	// AWS metadata endpoint: 169.254.169.254
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://metadata.aws/latest"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected URL resolving to metadata IP to fail SSRF validation")
	}
}

func TestValidateSSRFURL_RejectsIPLiteral_Private(t *testing.T) {
	v := NewValidator(testLogger())

	// Direct IP literal in URL -- no DNS needed.
	req := testSSRFStruct{WebhookURL: "https://192.168.1.1/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected private IP literal URL to fail SSRF validation")
	}
}

func TestValidateSSRFURL_FailsClosed_DNSError(t *testing.T) {
	// Inject a mock DNS resolver that returns an error (simulating timeout).
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, errors.New("dns resolution timed out")
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://unknown.host/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected DNS failure to cause SSRF validation to fail (fail closed)")
	}

	var appErr *types.AppError
	if errors.As(err, &appErr) {
		if appErr.Code != types.ErrCodeValidationInvalidWebhook {
			t.Errorf("expected code %s, got %s", types.ErrCodeValidationInvalidWebhook, appErr.Code)
		}
	}
}

func TestValidateSSRFURL_FailsClosed_NoAddresses(t *testing.T) {
	// DNS resolves but returns no addresses.
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://no-resolve.host/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected empty DNS result to cause SSRF validation to fail (fail closed)")
	}
}

func TestValidateSSRFURL_EmptyString_SkipsValidation(t *testing.T) {
	v := NewValidator(testLogger())

	// Empty string without required tag should pass.
	req := testSSRFStruct{WebhookURL: ""}
	err := v.ValidateStruct(req)
	if err != nil {
		t.Errorf("expected empty URL without required tag to pass, got: %v", err)
	}
}

func TestValidateSSRFURL_EmptyString_FailsWithRequired(t *testing.T) {
	v := NewValidator(testLogger())

	req := testRequiredSSRFStruct{WebhookURL: ""}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected empty URL with required tag to fail")
	}
}

func TestValidateSSRFURL_RejectsIPv6Localhost(t *testing.T) {
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("::1")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://ipv6-localhost.test/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected IPv6 localhost to fail SSRF validation")
	}
}

func TestValidateSSRFURL_RejectsIPv6Private(t *testing.T) {
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("fd00::1")}}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://ipv6-private.test/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected IPv6 private address to fail SSRF validation")
	}
}

func TestValidateSSRFURL_RejectsMultipleIPs_OneBlocked(t *testing.T) {
	// DNS returns both a safe and a blocked IP. If ANY IP is blocked, reject.
	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()
	dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("93.184.216.34")}, // safe
			{IP: net.ParseIP("10.0.0.1")},      // blocked
		}, nil
	}

	v := NewValidator(testLogger())

	req := testSSRFStruct{WebhookURL: "https://dual-ip.test/hook"}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected URL with any blocked IP to fail SSRF validation")
	}
}

// -- Timezone validation tests --

func TestValidateTimezone_Valid(t *testing.T) {
	v := NewValidator(testLogger())

	validTimezones := []string{
		"America/New_York",
		"America/Los_Angeles",
		"Europe/London",
		"UTC",
		"Asia/Tokyo",
	}

	for _, tz := range validTimezones {
		t.Run(tz, func(t *testing.T) {
			req := testTimezoneStruct{Timezone: tz}
			err := v.ValidateStruct(req)
			if err != nil {
				t.Errorf("expected timezone %q to be valid, got: %v", tz, err)
			}
		})
	}
}

func TestValidateTimezone_Invalid(t *testing.T) {
	v := NewValidator(testLogger())

	invalidTimezones := []string{
		"Not/A/Timezone",
		"fake",
		"US/Fake_City",
		"123",
	}

	for _, tz := range invalidTimezones {
		t.Run(tz, func(t *testing.T) {
			req := testTimezoneStruct{Timezone: tz}
			err := v.ValidateStruct(req)
			if err == nil {
				t.Errorf("expected timezone %q to be invalid", tz)
			}

			var appErr *types.AppError
			if errors.As(err, &appErr) {
				if appErr.Code != types.ErrCodeValidationInvalidTimezone {
					t.Errorf("expected code %s, got %s", types.ErrCodeValidationInvalidTimezone, appErr.Code)
				}
			}
		})
	}
}

func TestValidateTimezone_Empty_FailsWithRequired(t *testing.T) {
	v := NewValidator(testLogger())

	req := testTimezoneStruct{Timezone: ""}
	err := v.ValidateStruct(req)
	if err == nil {
		t.Error("expected empty timezone with required tag to fail")
	}
}

// -- CONUS validation tests --

func TestValidateCONUS_Valid(t *testing.T) {
	v := NewValidator(testLogger())

	// Center of CONUS: roughly Kansas
	req := testCONUSStruct{Lat: 38.0, Lon: -98.0}
	err := v.ValidateStruct(req)
	if err != nil {
		t.Errorf("expected CONUS coordinates to be valid, got: %v", err)
	}
}

func TestValidateCONUS_Boundary_Valid(t *testing.T) {
	v := NewValidator(testLogger())

	// Exact boundary values (inclusive).
	cases := []struct {
		name string
		lat  float64
		lon  float64
	}{
		{"min_lat_min_lon", 24.0, -125.0},
		{"max_lat_max_lon", 50.0, -66.0},
		{"min_lat_max_lon", 24.0, -66.0},
		{"max_lat_min_lon", 50.0, -125.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testCONUSStruct{Lat: tc.lat, Lon: tc.lon}
			err := v.ValidateStruct(req)
			if err != nil {
				t.Errorf("expected boundary CONUS coordinates (%f, %f) to be valid, got: %v", tc.lat, tc.lon, err)
			}
		})
	}
}

func TestValidateCONUS_Invalid_OutsideBounds(t *testing.T) {
	v := NewValidator(testLogger())

	cases := []struct {
		name string
		lat  float64
		lon  float64
	}{
		{"north_of_conus", 51.0, -98.0},
		{"south_of_conus", 23.0, -98.0},
		{"east_of_conus", 38.0, -65.0},
		{"west_of_conus", 38.0, -126.0},
		{"europe", 48.8566, 2.3522},
		{"antarctica", -80.0, 0.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testCONUSStruct{Lat: tc.lat, Lon: tc.lon}
			err := v.ValidateStruct(req)
			if err == nil {
				t.Errorf("expected non-CONUS coordinates (%f, %f) to fail validation", tc.lat, tc.lon)
			}
		})
	}
}

// -- isBlockedIP tests --

func TestIsBlockedIP(t *testing.T) {
	blockedIPs := []string{
		"127.0.0.1",     // localhost
		"10.0.0.1",      // private class A
		"172.16.0.1",    // private class B
		"192.168.1.1",   // private class C
		"169.254.1.1",   // link-local (AWS metadata)
		"0.0.0.1",       // current network
		"224.0.0.1",     // multicast
		"240.0.0.1",     // reserved
		"100.64.0.1",    // CGN
		"198.18.0.1",    // benchmark testing
	}

	for _, ipStr := range blockedIPs {
		t.Run(ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", ipStr)
			}
			if !isBlockedIP(ip) {
				t.Errorf("expected IP %s to be blocked", ipStr)
			}
		})
	}

	safeIPs := []string{
		"93.184.216.34",  // example.com
		"8.8.8.8",        // Google DNS
		"1.1.1.1",        // Cloudflare DNS
		"203.0.113.1",    // TEST-NET-3 (not in our blocklist)
	}

	for _, ipStr := range safeIPs {
		t.Run(ipStr+"_safe", func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", ipStr)
			}
			if isBlockedIP(ip) {
				t.Errorf("expected IP %s to NOT be blocked", ipStr)
			}
		})
	}
}

// -- Tag mapping tests --

func TestTagToErrorCode(t *testing.T) {
	cases := []struct {
		tag      string
		expected types.ErrorCode
	}{
		{"is_conus", types.ErrCodeValidationInvalidLat},
		{"ssrf_url", types.ErrCodeValidationInvalidWebhook},
		{"is_timezone", types.ErrCodeValidationInvalidTimezone},
		{"required", types.ErrCodeValidationMissingField},
		{"email", types.ErrCodeValidationInvalidEmail},
		{"latitude", types.ErrCodeValidationInvalidLat},
		{"longitude", types.ErrCodeValidationInvalidLon},
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			got := tagToErrorCode(tc.tag)
			if got != string(tc.expected) {
				t.Errorf("tagToErrorCode(%q) = %q, want %q", tc.tag, got, tc.expected)
			}
		})
	}
}

// -- Integration test: SSRF tag on structs with bad URLs --

func TestValidateSSRFURL_StructTagIntegration(t *testing.T) {
	// This is the primary definition-of-done test: structs with `ssrf_url` tags
	// fail validation when passed bad URLs.

	origResolver := dnsResolver
	defer func() { dnsResolver = origResolver }()

	tests := []struct {
		name       string
		url        string
		resolvedIP string
		dnsErr     error
		wantErr    bool
	}{
		{
			name:       "valid_https_safe_ip",
			url:        "https://safe.example.com/webhook",
			resolvedIP: "93.184.216.34",
			wantErr:    false,
		},
		{
			name:    "http_scheme_rejected",
			url:     "http://example.com/webhook",
			wantErr: true,
		},
		{
			name:       "private_ip_rejected",
			url:        "https://evil.example.com/webhook",
			resolvedIP: "10.0.0.1",
			wantErr:    true,
		},
		{
			name:       "localhost_rejected",
			url:        "https://localhost.example.com/webhook",
			resolvedIP: "127.0.0.1",
			wantErr:    true,
		},
		{
			name:       "metadata_ip_rejected",
			url:        "https://metadata.example.com/webhook",
			resolvedIP: "169.254.169.254",
			wantErr:    true,
		},
		{
			name:    "dns_failure_rejected",
			url:     "https://timeout.example.com/webhook",
			dnsErr:  errors.New("context deadline exceeded"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dnsResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				if tc.dnsErr != nil {
					return nil, tc.dnsErr
				}
				return []net.IPAddr{{IP: net.ParseIP(tc.resolvedIP)}}, nil
			}

			v := NewValidator(testLogger())
			req := testSSRFStruct{WebhookURL: tc.url}
			err := v.ValidateStruct(req)

			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for URL %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for URL %q, got: %v", tc.url, err)
			}
		})
	}
}

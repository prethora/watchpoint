package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceHMAC computes HMAC-SHA256 independently for test verification.
func referenceHMAC(content, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(content))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignatureManager_SignPayload_BasicSigning(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"threshold_crossed","value":42}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	secret := "whsec_test_secret_123"

	config := map[string]any{
		"secret": secret,
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)

	// Verify header format: t=<unix>,v1=<hex>
	assert.True(t, strings.HasPrefix(header, "t="), "header should start with t=")
	assert.Contains(t, header, ",v1=", "header should contain ,v1=")
	assert.NotContains(t, header, "v1_old", "header should not contain v1_old without previous secret")

	// Parse and verify the signature.
	parts := parseSignatureHeader(header)
	assert.Equal(t, fmt.Sprintf("%d", now.Unix()), parts.timestamp)

	// Independently compute expected HMAC.
	signedContent := fmt.Sprintf("%d.%s", now.Unix(), string(payload))
	expectedV1 := referenceHMAC(signedContent, secret)
	assert.Equal(t, expectedV1, parts.v1, "v1 signature should match independent HMAC computation")
}

func TestSignatureManager_SignPayload_DualValidity(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"threshold_crossed"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	currentSecret := "whsec_new_secret"
	previousSecret := "whsec_old_secret"
	// Grace period expires 24 hours from now - still valid.
	expiresAt := now.Add(24 * time.Hour)

	config := map[string]any{
		"secret":                     currentSecret,
		"previous_secret":            previousSecret,
		"previous_secret_expires_at": expiresAt.Format(time.RFC3339),
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)

	// Should contain both v1 and v1_old.
	assert.Contains(t, header, "v1_old=", "header should contain v1_old during grace period")

	parts := parseSignatureHeader(header)
	signedContent := fmt.Sprintf("%d.%s", now.Unix(), string(payload))

	expectedV1 := referenceHMAC(signedContent, currentSecret)
	expectedV1Old := referenceHMAC(signedContent, previousSecret)

	assert.Equal(t, expectedV1, parts.v1, "v1 should use current secret")
	assert.Equal(t, expectedV1Old, parts.v1Old, "v1_old should use previous secret")
}

func TestSignatureManager_SignPayload_ExpiredPreviousSecret(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"threshold_crossed"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	currentSecret := "whsec_new_secret"
	previousSecret := "whsec_old_secret"
	// Grace period already expired.
	expiresAt := now.Add(-1 * time.Hour)

	config := map[string]any{
		"secret":                     currentSecret,
		"previous_secret":            previousSecret,
		"previous_secret_expires_at": expiresAt.Format(time.RFC3339),
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)

	// CRITICAL: v1_old should be omitted when grace period has expired.
	assert.NotContains(t, header, "v1_old", "v1_old must be omitted after grace period expires")

	parts := parseSignatureHeader(header)
	assert.Empty(t, parts.v1Old, "v1_old should be empty when expired")
}

func TestSignatureManager_SignPayload_ExactlyAtExpiry(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"test"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	// Expires at exactly now - should INCLUDE v1_old (now <= expiresAt).
	expiresAt := now

	config := map[string]any{
		"secret":                     "current",
		"previous_secret":            "previous",
		"previous_secret_expires_at": expiresAt.Format(time.RFC3339),
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)
	assert.Contains(t, header, "v1_old=", "v1_old should be included when now == expiresAt")
}

func TestSignatureManager_SignPayload_MissingSecret(t *testing.T) {
	sm := NewSignatureManager()

	tests := []struct {
		name   string
		config map[string]any
	}{
		{"nil config", nil},
		{"empty config", map[string]any{}},
		{"empty secret string", map[string]any{"secret": ""}},
		{"wrong type for secret", map[string]any{"secret": 12345}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sm.SignPayload([]byte("test"), tt.config, time.Now())
			assert.Error(t, err, "should error when secret is missing or invalid")
			assert.Contains(t, err.Error(), "secret", "error should mention secret")
		})
	}
}

func TestSignatureManager_SignPayload_MalformedExpiry(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"test"}`)
	now := time.Now()

	config := map[string]any{
		"secret":                     "current_secret",
		"previous_secret":            "old_secret",
		"previous_secret_expires_at": "not-a-valid-date",
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err, "malformed expiry should not cause error")
	// v1_old should be omitted for safety when expiry cannot be parsed.
	assert.NotContains(t, header, "v1_old", "v1_old should be omitted with malformed expiry")
}

func TestSignatureManager_SignPayload_PreviousSecretWithoutExpiry(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"test"}`)
	now := time.Now()

	config := map[string]any{
		"secret":          "current_secret",
		"previous_secret": "old_secret",
		// No previous_secret_expires_at.
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)
	// Without expiry, v1_old should be omitted.
	assert.NotContains(t, header, "v1_old", "v1_old should be omitted without expiry timestamp")
}

func TestSignatureManager_VerifySignature_CurrentSecret(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"threshold_crossed"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	secret := "whsec_my_secret"

	config := map[string]any{"secret": secret}
	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)

	secrets := map[string]string{"current": secret}
	assert.True(t, sm.VerifySignature(payload, header, secrets), "should verify with current secret")
}

func TestSignatureManager_VerifySignature_PreviousSecret(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"threshold_crossed"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	currentSecret := "whsec_new"
	previousSecret := "whsec_old"
	expiresAt := now.Add(24 * time.Hour)

	config := map[string]any{
		"secret":                     currentSecret,
		"previous_secret":            previousSecret,
		"previous_secret_expires_at": expiresAt.Format(time.RFC3339),
	}

	header, err := sm.SignPayload(payload, config, now)
	require.NoError(t, err)

	// Verify using previous secret only.
	secrets := map[string]string{"previous": currentSecret}
	assert.True(t, sm.VerifySignature(payload, header, secrets), "should verify via v1 matched against previous key")
}

func TestSignatureManager_VerifySignature_InvalidSignature(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"test"}`)
	header := "t=1706745600,v1=deadbeef1234567890abcdef1234567890abcdef1234567890abcdef12345678"

	secrets := map[string]string{"current": "wrong_secret"}
	assert.False(t, sm.VerifySignature(payload, header, secrets), "should not verify with wrong secret")
}

func TestSignatureManager_VerifySignature_TamperedPayload(t *testing.T) {
	sm := NewSignatureManager()
	originalPayload := []byte(`{"event":"threshold_crossed"}`)
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	secret := "whsec_secret"

	config := map[string]any{"secret": secret}
	header, err := sm.SignPayload(originalPayload, config, now)
	require.NoError(t, err)

	// Tampered payload.
	tamperedPayload := []byte(`{"event":"threshold_crossed","extra":"malicious"}`)
	secrets := map[string]string{"current": secret}
	assert.False(t, sm.VerifySignature(tamperedPayload, header, secrets), "should not verify tampered payload")
}

func TestSignatureManager_VerifySignature_MalformedHeader(t *testing.T) {
	sm := NewSignatureManager()
	payload := []byte(`{"event":"test"}`)
	secrets := map[string]string{"current": "secret"}

	tests := []struct {
		name   string
		header string
	}{
		{"empty", ""},
		{"no timestamp", "v1=abc123"},
		{"no signature", "t=123456"},
		{"garbage", "garbage"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, sm.VerifySignature(payload, tt.header, secrets), "malformed header should not verify")
		})
	}
}

func TestParseSignatureHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   signatureParts
	}{
		{
			name:   "basic",
			header: "t=1706745600,v1=abc123",
			want:   signatureParts{timestamp: "1706745600", v1: "abc123"},
		},
		{
			name:   "with v1_old",
			header: "t=1706745600,v1=abc123,v1_old=def456",
			want:   signatureParts{timestamp: "1706745600", v1: "abc123", v1Old: "def456"},
		},
		{
			name:   "empty",
			header: "",
			want:   signatureParts{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSignatureHeader(tt.header)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeHMAC_Deterministic(t *testing.T) {
	content := "1706745600.{\"event\":\"test\"}"
	key := "secret_key"

	result1 := computeHMAC(content, key)
	result2 := computeHMAC(content, key)

	assert.Equal(t, result1, result2, "HMAC should be deterministic")
	assert.Len(t, result1, 64, "HMAC-SHA256 hex output should be 64 chars")
}

func TestComputeHMAC_DifferentKeysProduceDifferentResults(t *testing.T) {
	content := "1706745600.{\"event\":\"test\"}"

	result1 := computeHMAC(content, "key1")
	result2 := computeHMAC(content, "key2")

	assert.NotEqual(t, result1, result2, "different keys should produce different HMACs")
}

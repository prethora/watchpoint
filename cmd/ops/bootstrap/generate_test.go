package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// GenerateSecureToken tests
// ---------------------------------------------------------------------------

func TestGenerateSecureToken_ProducesCorrectLength(t *testing.T) {
	token, err := GenerateSecureToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 32 bytes hex-encoded = 64 characters.
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64", len(token))
	}
}

func TestGenerateSecureToken_ProducesValidHex(t *testing.T) {
	token, err := GenerateSecureToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The token must be a valid hex string.
	decoded, err := hex.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}

	// Decoded bytes should be exactly 32 bytes.
	if len(decoded) != 32 {
		t.Errorf("decoded length = %d, want 32", len(decoded))
	}
}

func TestGenerateSecureToken_ProducesLowercaseHex(t *testing.T) {
	token, err := GenerateSecureToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// hex.EncodeToString produces lowercase, but verify explicitly.
	if token != strings.ToLower(token) {
		t.Errorf("token should be lowercase hex, got %q", token)
	}
}

func TestGenerateSecureToken_ProducesUniqueTokens(t *testing.T) {
	// Generate multiple tokens and verify they are all distinct.
	// The probability of collision with 256-bit random tokens is negligible.
	const numTokens = 100
	seen := make(map[string]bool, numTokens)

	for i := 0; i < numTokens; i++ {
		token, err := GenerateSecureToken()
		if err != nil {
			t.Fatalf("unexpected error on iteration %d: %v", i, err)
		}
		if seen[token] {
			t.Fatalf("duplicate token detected on iteration %d: %q", i, token)
		}
		seen[token] = true
	}
}

func TestGenerateSecureToken_SufficientEntropy(t *testing.T) {
	// Verify the token is not all zeros, all ones, or other degenerate patterns.
	// This is a basic sanity check -- a proper entropy test is impractical in a
	// unit test, but we can catch obvious failures of the random source.
	token, err := GenerateSecureToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check it's not all zeros.
	allZeros := strings.Repeat("0", 64)
	if token == allZeros {
		t.Fatal("token is all zeros, indicating a failed random source")
	}

	// Check it's not all 'f's.
	allFs := strings.Repeat("f", 64)
	if token == allFs {
		t.Fatal("token is all 0xff, indicating a failed random source")
	}

	// Check it contains some variety of hex digits.
	uniqueChars := make(map[byte]bool)
	for i := 0; i < len(token); i++ {
		uniqueChars[token[i]] = true
	}
	// With 64 hex chars and 16 possible values, we expect many unique chars.
	// A threshold of 4 is extremely conservative -- in practice we'd see 14-16.
	if len(uniqueChars) < 4 {
		t.Errorf("token has only %d unique hex digits, expected more variety: %q", len(uniqueChars), token)
	}
}

func TestGenerateSecureToken_MeetsSessionKeyMinLength(t *testing.T) {
	// AuthConfig.SessionKey has validate:"required,min=32".
	// The hex-encoded 32-byte token is 64 chars, which must be >= 32.
	token, err := GenerateSecureToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(token) < 32 {
		t.Errorf("token length %d is less than SessionKey minimum of 32", len(token))
	}
}

// ---------------------------------------------------------------------------
// GenerateInternalSecrets tests
// ---------------------------------------------------------------------------

func TestGenerateInternalSecrets_ReturnsExpectedKeys(t *testing.T) {
	secrets, err := GenerateInternalSecrets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedKeys := []string{
		"auth/session_key",
		"security/admin_api_key",
	}

	if len(secrets) != len(expectedKeys) {
		t.Fatalf("secrets count = %d, want %d", len(secrets), len(expectedKeys))
	}

	for _, key := range expectedKeys {
		value, ok := secrets[key]
		if !ok {
			t.Errorf("missing expected key %q", key)
			continue
		}
		if value == "" {
			t.Errorf("value for key %q is empty", key)
		}
	}
}

func TestGenerateInternalSecrets_ValuesAreValidTokens(t *testing.T) {
	secrets, err := GenerateInternalSecrets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for key, value := range secrets {
		// Each value should be a valid 64-char hex string.
		if len(value) != 64 {
			t.Errorf("secret %q: length = %d, want 64", key, len(value))
		}

		if _, err := hex.DecodeString(value); err != nil {
			t.Errorf("secret %q: not valid hex: %v", key, err)
		}
	}
}

func TestGenerateInternalSecrets_ValuesAreDistinct(t *testing.T) {
	secrets, err := GenerateInternalSecrets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessionKey := secrets["auth/session_key"]
	adminKey := secrets["security/admin_api_key"]

	if sessionKey == adminKey {
		t.Error("session key and admin API key should be different (each independently generated)")
	}
}

func TestGenerateInternalSecrets_KeyPathsMatchSSMInventory(t *testing.T) {
	// Verify the returned key paths match what SSMManager.SSMPath expects.
	// When combined with SSMPath("auth/session_key"), this should produce
	// "/{env}/watchpoint/auth/session_key".
	secrets, err := GenerateInternalSecrets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// These paths must match the Secret Inventory Table in 13-human-setup.md:
	// - Session Key -> auth/session_key
	// - Admin API Key -> security/admin_api_key
	if _, ok := secrets["auth/session_key"]; !ok {
		t.Error("missing auth/session_key (should match SSM path: /{env}/watchpoint/auth/session_key)")
	}
	if _, ok := secrets["security/admin_api_key"]; !ok {
		t.Error("missing security/admin_api_key (should match SSM path: /{env}/watchpoint/security/admin_api_key)")
	}
}

func TestGenerateInternalSecrets_DeterministicKeySet(t *testing.T) {
	// Call multiple times and verify the key set is always the same
	// (values will differ, but the map keys should be stable).
	for i := 0; i < 5; i++ {
		secrets, err := GenerateInternalSecrets()
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}

		if len(secrets) != 2 {
			t.Fatalf("iteration %d: expected 2 secrets, got %d", i, len(secrets))
		}

		if _, ok := secrets["auth/session_key"]; !ok {
			t.Fatalf("iteration %d: missing auth/session_key", i)
		}
		if _, ok := secrets["security/admin_api_key"]; !ok {
			t.Fatalf("iteration %d: missing security/admin_api_key", i)
		}
	}
}

// ---------------------------------------------------------------------------
// tokenByteLength constant test
// ---------------------------------------------------------------------------

func TestTokenByteLength(t *testing.T) {
	// Verify the constant matches the spec: 32 bytes (256 bits).
	if tokenByteLength != 32 {
		t.Errorf("tokenByteLength = %d, want 32", tokenByteLength)
	}
}

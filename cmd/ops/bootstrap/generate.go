package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// tokenByteLength is the number of random bytes generated for internal secrets.
// 32 bytes = 256 bits of entropy, hex-encoded to a 64-character string.
//
// This satisfies:
// - AuthConfig.SessionKey validate:"required,min=32" (64 hex chars > 32)
// - SecurityConfig.AdminAPIKey validate:"required"
//
// Reference: 13-human-setup.md Section 4 (Secret Inventory Table) specifies
// "Generate Random 32b" for Session Key and Admin API Key.
const tokenByteLength = 32

// GenerateSecureToken produces a cryptographically secure random token
// suitable for use as a session signing key, admin API key, or other
// high-privilege internal secret.
//
// The token is generated using crypto/rand (OS entropy source) and encoded
// as a lowercase hex string. The result is 64 characters long (32 bytes
// hex-encoded), providing 256 bits of entropy.
//
// This function is used during the bootstrap process to automatically
// populate SESSION_KEY and ADMIN_API_KEY without requiring human input,
// adhering to the security requirement in 13-human-setup.md Section 1:
// "NEVER echo the user's input back to the console or logs."
// Since these are generated internally, they are never displayed to the
// operator at all.
//
// Returns an error only if the system's cryptographic random number generator
// fails, which indicates a severe system-level problem.
func GenerateSecureToken() (string, error) {
	buf := make([]byte, tokenByteLength)
	n, err := rand.Read(buf)
	if err != nil {
		return "", fmt.Errorf("generating secure token: crypto/rand failed: %w", err)
	}
	if n != tokenByteLength {
		return "", fmt.Errorf("generating secure token: expected %d random bytes, got %d", tokenByteLength, n)
	}

	return hex.EncodeToString(buf), nil
}

// GenerateInternalSecrets generates all internally-created secrets required
// by the bootstrap process. These are secrets that do not come from external
// vendors but are created locally using cryptographic randomness.
//
// Currently generates:
// - Session Key (auth/session_key): Used for session signing and CSRF tokens.
// - Admin API Key (security/admin_api_key): Used for internal admin operations.
//
// Returns a map of SSM category/key paths to their generated values.
// The caller is responsible for writing these to SSM via SSMManager.PutSecret.
//
// The generated values are never logged or displayed to the operator.
// The SSMManager.PutSecret method logs only the path and value length,
// not the value itself.
//
// Reference: 13-human-setup.md Section 4 (Secret Inventory Table)
func GenerateInternalSecrets() (map[string]string, error) {
	secrets := make(map[string]string, 2)

	sessionKey, err := GenerateSecureToken()
	if err != nil {
		return nil, fmt.Errorf("generating session key: %w", err)
	}
	secrets["auth/session_key"] = sessionKey

	adminAPIKey, err := GenerateSecureToken()
	if err != nil {
		return nil, fmt.Errorf("generating admin API key: %w", err)
	}
	secrets["security/admin_api_key"] = adminAPIKey

	return secrets, nil
}

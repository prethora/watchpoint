// Package webhook implements the Webhook notification delivery channel.
//
// It handles platform auto-detection (Slack, Teams, Discord, Google Chat),
// payload formatting using platform-specific JSON schemas, HMAC signing
// for security (dual-validity rotation support), and strict SSRF protection.
//
// Architecture reference: 08c-webhook-worker.md
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"watchpoint/internal/notifications/core"
)

// Compile-time assertion that SignatureManager implements core.SignatureManager.
var _ core.SignatureManager = (*SignatureManager)(nil)

// SignatureManager handles HMAC-SHA256 payload signing with dual-validity
// support for zero-downtime secret rotation. It implements the signing
// scheme described in 08a-notification-core.md Section 8 and flow SEC-006.
//
// Header format: X-Watchpoint-Signature: t=<unix>,v1=<hmac>[,v1_old=<hmac>]
type SignatureManager struct{}

// NewSignatureManager creates a new SignatureManager instance.
func NewSignatureManager() *SignatureManager {
	return &SignatureManager{}
}

// SignPayload generates the signature header value for a webhook payload.
//
// The secretConfig map is expected to contain:
//   - "secret" (string): The current signing secret (required).
//   - "previous_secret" (string): The old secret during rotation (optional).
//   - "previous_secret_expires_at" (string, RFC3339): Expiration time for
//     the previous secret (optional, only used if previous_secret is present).
//
// The signed content is "{unix_timestamp}.{payload}" using HMAC-SHA256.
//
// If previous_secret exists and now <= previous_secret_expires_at, the header
// includes a v1_old signature. If the previous_secret has expired (now >
// previous_secret_expires_at), v1_old is omitted even if previous_secret
// is present in config.
//
// Returns the formatted header value: "t=...,v1=..."  or "t=...,v1=...,v1_old=..."
func (sm *SignatureManager) SignPayload(payload []byte, secretConfig map[string]any, now time.Time) (string, error) {
	// Extract the current secret (required).
	secret, ok := secretConfig["secret"].(string)
	if !ok || secret == "" {
		return "", fmt.Errorf("webhook signature: missing or empty 'secret' in config")
	}

	// Generate timestamp component.
	timestamp := now.Unix()

	// Build the signed content: "{timestamp}.{payload}"
	signedContent := fmt.Sprintf("%d.%s", timestamp, string(payload))

	// Compute v1 signature with current secret.
	v1 := computeHMAC(signedContent, secret)

	// Start building the header.
	header := fmt.Sprintf("t=%d,v1=%s", timestamp, v1)

	// Check for previous secret (dual-validity / rotation support).
	prevSecret, hasPrevSecret := secretConfig["previous_secret"].(string)
	if hasPrevSecret && prevSecret != "" {
		// Check expiration of previous secret.
		expiresAtStr, hasExpiry := secretConfig["previous_secret_expires_at"].(string)
		if hasExpiry && expiresAtStr != "" {
			expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
			if err != nil {
				// If we cannot parse the expiration, omit v1_old for safety.
				// This is a defensive choice: a malformed expiration should not
				// extend the validity of an old secret.
				return header, nil
			}

			// Only include v1_old if the grace period has NOT expired.
			// CRITICAL: now must be before or equal to expiresAt.
			if !now.After(expiresAt) {
				v1Old := computeHMAC(signedContent, prevSecret)
				header = fmt.Sprintf("%s,v1_old=%s", header, v1Old)
			}
		}
		// If no expiry is specified but previous_secret exists,
		// we omit v1_old because we cannot determine validity.
	}

	return header, nil
}

// VerifySignature checks a payload against a signature header using the
// provided secrets. It supports both current and previous secrets for
// dual-validity verification.
//
// The secrets map should contain:
//   - "current": The current signing secret.
//   - "previous" (optional): The previous secret for rotation support.
//
// Returns true if the payload matches either the v1 or v1_old signature
// in the header using the corresponding secret.
func (sm *SignatureManager) VerifySignature(payload []byte, header string, secrets map[string]string) bool {
	// Parse header components.
	parts := parseSignatureHeader(header)
	if parts.timestamp == "" || parts.v1 == "" {
		return false
	}

	// Reconstruct signed content.
	signedContent := fmt.Sprintf("%s.%s", parts.timestamp, string(payload))

	// Check v1 against current secret.
	if currentSecret, ok := secrets["current"]; ok && currentSecret != "" {
		expected := computeHMAC(signedContent, currentSecret)
		if hmac.Equal([]byte(parts.v1), []byte(expected)) {
			return true
		}
	}

	// Check v1_old against previous secret (if present).
	if parts.v1Old != "" {
		if prevSecret, ok := secrets["previous"]; ok && prevSecret != "" {
			expected := computeHMAC(signedContent, prevSecret)
			if hmac.Equal([]byte(parts.v1Old), []byte(expected)) {
				return true
			}
		}
	}

	// Also check v1 against previous secret for the case where the
	// verifier has the old secret and the sender has rotated.
	if prevSecret, ok := secrets["previous"]; ok && prevSecret != "" {
		expected := computeHMAC(signedContent, prevSecret)
		if hmac.Equal([]byte(parts.v1), []byte(expected)) {
			return true
		}
	}

	return false
}

// signatureParts holds the parsed components of a signature header.
type signatureParts struct {
	timestamp string
	v1        string
	v1Old     string
}

// parseSignatureHeader breaks a signature header into its component parts.
// Expected format: "t=<unix>,v1=<hex>[,v1_old=<hex>]"
func parseSignatureHeader(header string) signatureParts {
	var parts signatureParts
	for _, segment := range strings.Split(header, ",") {
		kv := strings.SplitN(segment, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		switch key {
		case "t":
			parts.timestamp = value
		case "v1":
			parts.v1 = value
		case "v1_old":
			parts.v1Old = value
		}
	}
	return parts
}

// computeHMAC computes the HMAC-SHA256 of content using the given key
// and returns it as a lowercase hex string.
func computeHMAC(content, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(content))
	return hex.EncodeToString(mac.Sum(nil))
}

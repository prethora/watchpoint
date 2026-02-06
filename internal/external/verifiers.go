package external

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// ---------------------------------------------------------------------------
// SendGrid Webhook Verification (ECDSA) — Section 5.2
// ---------------------------------------------------------------------------

// SendGridVerifier implements EmailVerifier using ECDSA signature verification.
// SendGrid signs Event Webhook payloads using ECDSA with P-256 (prime256v1).
// The verification process:
//  1. Decode the base64-encoded public key (Elliptic Curve public key)
//  2. Decode the base64-encoded signature
//  3. Compute SHA-256 hash of (timestamp + payload)
//  4. Verify the ECDSA signature against the hash
type SendGridVerifier struct{}

// Verify checks the ECDSA signature from X-Twilio-Email-Event-Webhook-Signature.
// Parameters:
//   - payload: the raw webhook request body
//   - signature: base64-encoded ECDSA signature from
//     X-Twilio-Email-Event-Webhook-Signature header
//   - timestamp: value from X-Twilio-Email-Event-Webhook-Timestamp header
//   - publicKey: base64-encoded Elliptic Curve public key from SendGrid settings
//
// Returns (true, nil) if the signature is valid, (false, nil) if invalid,
// or (false, err) if verification could not be performed (e.g., malformed inputs).
func (v *SendGridVerifier) Verify(payload []byte, signature string, timestamp string, publicKey string) (bool, error) {
	// Parse the public key.
	ecdsaKey, err := parseECPublicKey(publicKey)
	if err != nil {
		return false, fmt.Errorf("failed to parse public key: %w", err)
	}

	// Decode the base64-encoded signature.
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	// Parse the ASN.1 DER-encoded ECDSA signature into (r, s) components.
	r, s, err := parseECDSASignature(sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to parse ECDSA signature: %w", err)
	}

	// Compute SHA-256 hash of timestamp + payload.
	// SendGrid concatenates the timestamp string with the raw payload bytes.
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write(payload)
	digest := h.Sum(nil)

	// Verify the ECDSA signature.
	valid := ecdsa.Verify(ecdsaKey, digest, r, s)
	return valid, nil
}

// parseECPublicKey parses an Elliptic Curve public key from a base64-encoded
// string. It supports both raw base64-encoded DER format and PEM-wrapped keys.
func parseECPublicKey(publicKeyStr string) (*ecdsa.PublicKey, error) {
	if publicKeyStr == "" {
		return nil, errors.New("public key is empty")
	}

	// First, try to decode as PEM.
	block, _ := pem.Decode([]byte(publicKeyStr))
	var derBytes []byte
	if block != nil {
		derBytes = block.Bytes
	} else {
		// Assume raw base64-encoded DER.
		var err error
		derBytes, err = base64.StdEncoding.DecodeString(publicKeyStr)
		if err != nil {
			return nil, fmt.Errorf("failed to base64-decode public key: %w", err)
		}
	}

	// Parse the DER-encoded public key.
	pub, err := x509.ParsePKIXPublicKey(derBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PKIX public key: %w", err)
	}

	ecdsaKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not ECDSA (got %T)", pub)
	}

	return ecdsaKey, nil
}

// ecdsaSignature represents an ASN.1 DER-encoded ECDSA signature.
type ecdsaSignature struct {
	R, S *big.Int
}

// parseECDSASignature decodes an ASN.1 DER-encoded ECDSA signature into
// its (r, s) components.
func parseECDSASignature(sigBytes []byte) (*big.Int, *big.Int, error) {
	var sig ecdsaSignature
	rest, err := asn1.Unmarshal(sigBytes, &sig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal ASN.1 signature: %w", err)
	}
	if len(rest) > 0 {
		return nil, nil, errors.New("trailing data after ASN.1 signature")
	}
	if sig.R == nil || sig.S == nil {
		return nil, nil, errors.New("signature contains nil R or S value")
	}
	return sig.R, sig.S, nil
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertion that SendGridVerifier satisfies EmailVerifier.
var _ EmailVerifier = (*SendGridVerifier)(nil)

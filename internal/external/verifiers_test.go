package external

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
)

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// generateTestECDSAKey generates a P-256 ECDSA key pair for testing.
func generateTestECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ECDSA key: %v", err)
	}
	return key
}

// marshalPublicKeyBase64 encodes an ECDSA public key as a base64 string
// (DER/PKIX format), matching the format SendGrid provides.
func marshalPublicKeyBase64(t *testing.T, pub *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// marshalPublicKeyPEM encodes an ECDSA public key as a PEM string.
func marshalPublicKeyPEM(t *testing.T, pub *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	block := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}
	return string(pem.EncodeToMemory(block))
}

// signPayload creates a valid SendGrid-style ECDSA signature over
// timestamp+payload. Returns the base64-encoded ASN.1 DER signature.
func signPayload(t *testing.T, key *ecdsa.PrivateKey, timestamp string, payload []byte) string {
	t.Helper()

	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write(payload)
	digest := h.Sum(nil)

	r, s, err := ecdsa.Sign(rand.Reader, key, digest)
	if err != nil {
		t.Fatalf("failed to sign payload: %v", err)
	}

	sigBytes, err := asn1.Marshal(ecdsaSignature{R: r, S: s})
	if err != nil {
		t.Fatalf("failed to marshal signature: %v", err)
	}

	return base64.StdEncoding.EncodeToString(sigBytes)
}

// ---------------------------------------------------------------------------
// SendGridVerifier Tests
// ---------------------------------------------------------------------------

func TestSendGridVerifier_ValidSignature(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	payload := []byte(`[{"email":"bounce@example.com","event":"bounce","type":"hard"}]`)
	timestamp := "1614556800"

	signature := signPayload(t, key, timestamp, payload)

	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !valid {
		t.Error("expected signature to be valid, got invalid")
	}
}

func TestSendGridVerifier_ValidSignatureWithPEMKey(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyPEM := marshalPublicKeyPEM(t, &key.PublicKey)

	payload := []byte(`[{"email":"test@example.com","event":"bounce"}]`)
	timestamp := "1614556800"

	signature := signPayload(t, key, timestamp, payload)

	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, pubKeyPEM)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !valid {
		t.Error("expected signature to be valid with PEM key, got invalid")
	}
}

func TestSendGridVerifier_InvalidSignature_WrongPayload(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	originalPayload := []byte(`[{"email":"bounce@example.com","event":"bounce"}]`)
	tamperedPayload := []byte(`[{"email":"tampered@example.com","event":"bounce"}]`)
	timestamp := "1614556800"

	// Sign the original payload.
	signature := signPayload(t, key, timestamp, originalPayload)

	// Verify with the tampered payload -- should be invalid.
	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(tamperedPayload, signature, timestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error (invalid signature is not an error), got: %v", err)
	}
	if valid {
		t.Error("expected signature to be invalid for tampered payload, got valid")
	}
}

func TestSendGridVerifier_InvalidSignature_WrongTimestamp(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	payload := []byte(`[{"email":"bounce@example.com","event":"bounce"}]`)
	correctTimestamp := "1614556800"
	wrongTimestamp := "1614556899"

	// Sign with the correct timestamp.
	signature := signPayload(t, key, correctTimestamp, payload)

	// Verify with the wrong timestamp.
	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, wrongTimestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if valid {
		t.Error("expected signature to be invalid for wrong timestamp, got valid")
	}
}

func TestSendGridVerifier_InvalidSignature_WrongKey(t *testing.T) {
	signingKey := generateTestECDSAKey(t)
	wrongKey := generateTestECDSAKey(t)
	wrongPubKeyB64 := marshalPublicKeyBase64(t, &wrongKey.PublicKey)

	payload := []byte(`[{"email":"bounce@example.com","event":"bounce"}]`)
	timestamp := "1614556800"

	// Sign with the signing key.
	signature := signPayload(t, signingKey, timestamp, payload)

	// Verify with a different key's public key.
	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, wrongPubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if valid {
		t.Error("expected signature to be invalid for wrong key, got valid")
	}
}

func TestSendGridVerifier_MalformedSignature(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	payload := []byte(`[{"event":"bounce"}]`)
	timestamp := "1614556800"

	verifier := &SendGridVerifier{}

	// Test with non-base64 signature.
	_, err := verifier.Verify(payload, "not-valid-base64!!!", timestamp, pubKeyB64)
	if err == nil {
		t.Error("expected error for non-base64 signature, got nil")
	}

	// Test with valid base64 but invalid ASN.1.
	invalidASN1 := base64.StdEncoding.EncodeToString([]byte("not-asn1-data"))
	_, err = verifier.Verify(payload, invalidASN1, timestamp, pubKeyB64)
	if err == nil {
		t.Error("expected error for invalid ASN.1 signature, got nil")
	}
}

func TestSendGridVerifier_MalformedPublicKey(t *testing.T) {
	payload := []byte(`[{"event":"bounce"}]`)
	timestamp := "1614556800"
	signature := base64.StdEncoding.EncodeToString([]byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01})

	verifier := &SendGridVerifier{}

	// Test with non-base64 public key.
	_, err := verifier.Verify(payload, signature, timestamp, "not-valid-base64!!!")
	if err == nil {
		t.Error("expected error for non-base64 public key, got nil")
	}

	// Test with empty public key.
	_, err = verifier.Verify(payload, signature, timestamp, "")
	if err == nil {
		t.Error("expected error for empty public key, got nil")
	}

	// Test with valid base64 but invalid DER.
	invalidDER := base64.StdEncoding.EncodeToString([]byte("not-a-real-key"))
	_, err = verifier.Verify(payload, signature, timestamp, invalidDER)
	if err == nil {
		t.Error("expected error for invalid DER public key, got nil")
	}
}

func TestSendGridVerifier_EmptyPayload(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	// Sign an empty payload -- this should work.
	payload := []byte{}
	timestamp := "1614556800"

	signature := signPayload(t, key, timestamp, payload)

	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !valid {
		t.Error("expected valid signature for empty payload")
	}
}

func TestSendGridVerifier_EmptyTimestamp(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	payload := []byte(`[{"event":"bounce"}]`)
	timestamp := ""

	// Sign with empty timestamp.
	signature := signPayload(t, key, timestamp, payload)

	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !valid {
		t.Error("expected valid signature for empty timestamp")
	}
}

func TestSendGridVerifier_LargePayload(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	// Simulate a large webhook payload with many events.
	payload := make([]byte, 64*1024) // 64KB
	for i := range payload {
		payload[i] = byte('a' + (i % 26))
	}
	timestamp := "1614556800"

	signature := signPayload(t, key, timestamp, payload)

	verifier := &SendGridVerifier{}
	valid, err := verifier.Verify(payload, signature, timestamp, pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !valid {
		t.Error("expected valid signature for large payload")
	}
}

func TestSendGridVerifier_InterfaceCompliance(t *testing.T) {
	// This test verifies at compile time that SendGridVerifier satisfies EmailVerifier.
	// The compile-time assertion is in verifiers.go, but we double-check here.
	var _ EmailVerifier = (*SendGridVerifier)(nil)
}

// ---------------------------------------------------------------------------
// parseECDSASignature edge cases
// ---------------------------------------------------------------------------

func TestParseECDSASignature_TrailingData(t *testing.T) {
	// Create a valid ASN.1 signature and append trailing bytes.
	sig := ecdsaSignature{R: big.NewInt(42), S: big.NewInt(43)}
	sigBytes, err := asn1.Marshal(sig)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Append trailing data.
	sigBytes = append(sigBytes, 0x00, 0x01)

	_, _, err = parseECDSASignature(sigBytes)
	if err == nil {
		t.Error("expected error for signature with trailing data, got nil")
	}
}

func TestParseECDSASignature_EmptyInput(t *testing.T) {
	_, _, err := parseECDSASignature([]byte{})
	if err == nil {
		t.Error("expected error for empty input, got nil")
	}
}

// ---------------------------------------------------------------------------
// parseECPublicKey edge cases
// ---------------------------------------------------------------------------

func TestParseECPublicKey_EmptyString(t *testing.T) {
	_, err := parseECPublicKey("")
	if err == nil {
		t.Error("expected error for empty public key string, got nil")
	}
}

func TestParseECPublicKey_ValidBase64(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyB64 := marshalPublicKeyBase64(t, &key.PublicKey)

	ecKey, err := parseECPublicKey(pubKeyB64)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ecKey == nil {
		t.Fatal("expected non-nil key")
	}
	if ecKey.Curve != elliptic.P256() {
		t.Error("expected P-256 curve")
	}
}

func TestParseECPublicKey_ValidPEM(t *testing.T) {
	key := generateTestECDSAKey(t)
	pubKeyPEM := marshalPublicKeyPEM(t, &key.PublicKey)

	ecKey, err := parseECPublicKey(pubKeyPEM)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ecKey == nil {
		t.Fatal("expected non-nil key")
	}
}

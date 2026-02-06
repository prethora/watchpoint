package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const testSecret = "super-secret-api-key-12345"

func TestSecretString_String(t *testing.T) {
	s := SecretString(testSecret)

	result := s.String()

	if result != redactedPlaceholder {
		t.Errorf("String() = %q, want %q", result, redactedPlaceholder)
	}
	if strings.Contains(result, testSecret) {
		t.Errorf("String() leaked the raw secret value")
	}
}

func TestSecretString_Sprintf(t *testing.T) {
	s := SecretString(testSecret)

	// %s uses the String() method via the fmt.Stringer interface.
	result := fmt.Sprintf("key=%s", s)

	if strings.Contains(result, testSecret) {
		t.Errorf("fmt.Sprintf(%%s) leaked the raw secret: %s", result)
	}
	expected := "key=" + redactedPlaceholder
	if result != expected {
		t.Errorf("fmt.Sprintf(%%s) = %q, want %q", result, expected)
	}
}

func TestSecretString_SprintfV(t *testing.T) {
	s := SecretString(testSecret)

	// %v also uses the String() method when the Stringer interface is implemented.
	result := fmt.Sprintf("key=%v", s)

	if strings.Contains(result, testSecret) {
		t.Errorf("fmt.Sprintf(%%v) leaked the raw secret: %s", result)
	}
	expected := "key=" + redactedPlaceholder
	if result != expected {
		t.Errorf("fmt.Sprintf(%%v) = %q, want %q", result, expected)
	}
}

func TestSecretString_MarshalJSON(t *testing.T) {
	s := SecretString(testSecret)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON returned error: %v", err)
	}

	result := string(data)
	if strings.Contains(result, testSecret) {
		t.Errorf("MarshalJSON leaked the raw secret: %s", result)
	}

	expected := `"` + redactedPlaceholder + `"`
	if result != expected {
		t.Errorf("MarshalJSON = %q, want %q", result, expected)
	}
}

func TestSecretString_MarshalJSON_InStruct(t *testing.T) {
	type Config struct {
		APIKey SecretString `json:"api_key"`
		Name   string       `json:"name"`
	}

	cfg := Config{
		APIKey: SecretString(testSecret),
		Name:   "test",
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	result := string(data)
	if strings.Contains(result, testSecret) {
		t.Errorf("json.Marshal of struct leaked the raw secret: %s", result)
	}
	if !strings.Contains(result, redactedPlaceholder) {
		t.Errorf("json.Marshal of struct did not contain redacted placeholder: %s", result)
	}
}

func TestSecretString_Unmask(t *testing.T) {
	s := SecretString(testSecret)

	result := s.Unmask()

	if result != testSecret {
		t.Errorf("Unmask() = %q, want %q", result, testSecret)
	}
}

func TestSecretString_EmptyValue(t *testing.T) {
	s := SecretString("")

	if s.String() != redactedPlaceholder {
		t.Errorf("String() on empty SecretString = %q, want %q", s.String(), redactedPlaceholder)
	}

	if s.Unmask() != "" {
		t.Errorf("Unmask() on empty SecretString = %q, want empty string", s.Unmask())
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON on empty SecretString returned error: %v", err)
	}
	expected := `"` + redactedPlaceholder + `"`
	if string(data) != expected {
		t.Errorf("MarshalJSON on empty SecretString = %q, want %q", string(data), expected)
	}
}

func TestSecretString_GoStringer(t *testing.T) {
	s := SecretString(testSecret)

	// %+v should also not leak the value because the Stringer interface takes precedence.
	result := fmt.Sprintf("%+v", s)
	if strings.Contains(result, testSecret) {
		t.Errorf("fmt.Sprintf(%%+v) leaked the raw secret: %s", result)
	}
}

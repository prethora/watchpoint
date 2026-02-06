package types

// redactedPlaceholder is the string used to replace secret values in logs and serialization.
const redactedPlaceholder = "***REDACTED***"

// redactedJSON is the pre-computed JSON encoding of the redacted placeholder.
var redactedJSON = []byte(`"***REDACTED***"`)

// SecretString is a string type that prevents accidental logging or serialization
// of sensitive values. It overrides String() and MarshalJSON() to return a redacted
// placeholder, ensuring secrets are never leaked through fmt functions or JSON output.
//
// Use Unmask() to retrieve the raw plaintext value when it is genuinely needed
// (e.g., passing to an HTTP client or database driver).
type SecretString string

// String returns a redacted placeholder instead of the raw value.
// This is invoked by fmt.Sprintf, fmt.Println, and any other function
// that uses the fmt.Stringer interface.
func (s SecretString) String() string {
	return redactedPlaceholder
}

// MarshalJSON returns the redacted placeholder as a JSON string.
// This prevents secret values from being included in JSON-serialized
// config dumps, API responses, or structured log entries.
func (s SecretString) MarshalJSON() ([]byte, error) {
	return redactedJSON, nil
}

// Unmask returns the raw plaintext value of the secret.
// Usage of this method should be strictly audited and limited to cases
// where the actual secret value is required (e.g., constructing HTTP
// Authorization headers, database connection strings).
func (s SecretString) Unmask() string {
	return string(s)
}

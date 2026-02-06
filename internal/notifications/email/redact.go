package email

import "strings"

// RedactEmail masks an email address for safe logging by replacing all but
// the first character of the local part with asterisks. For example,
// "john@gmail.com" becomes "j***@gmail.com".
//
// If the email does not contain an "@" symbol, the entire string is masked
// to prevent accidental PII exposure in logs.
func RedactEmail(email string) string {
	if email == "" {
		return ""
	}

	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		// No @ sign - mask the entire string.
		return "***"
	}

	local := parts[0]
	domain := parts[1]

	if len(local) == 0 {
		return "***@" + domain
	}

	// Keep the first character, replace the rest with "***".
	return string(local[0]) + "***@" + domain
}

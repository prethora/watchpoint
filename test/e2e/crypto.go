//go:build e2e

package e2e

import (
	"golang.org/x/crypto/bcrypt"
)

// hashPasswordBcrypt produces a bcrypt hash of the password using a low cost
// factor suitable for testing (fast hashing, not production security).
func hashPasswordBcrypt(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

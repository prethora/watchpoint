// Package email implements the Email Worker notification channel. It handles
// template rendering with soft-fail fallback, data preparation with timezone
// localization, and delivery via an external EmailProvider (AWS SES).
//
// Architecture reference: 08b-email-worker.md
package email

import (
	"errors"

	"watchpoint/internal/types"
)

// ErrRecipientBlocked indicates the email provider has the recipient on a
// suppression list or has blocked delivery. This is treated as a terminal
// (non-retryable) failure.
var ErrRecipientBlocked = errors.New("recipient blocked by provider")

// IsBlocklistError checks whether an error indicates the recipient is blocked
// by the email provider. It checks both the sentinel ErrRecipientBlocked and
// the AppError code ErrCodeEmailBlocked (returned by the email provider).
func IsBlocklistError(err error) bool {
	if errors.Is(err, ErrRecipientBlocked) {
		return true
	}
	// Also check for the AppError code returned by the email provider client,
	// which maps rejection errors to ErrCodeEmailBlocked.
	var appErr *types.AppError
	if errors.As(err, &appErr) {
		return appErr.Code == types.ErrCodeEmailBlocked
	}
	return false
}

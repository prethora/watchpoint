package email

import (
	"errors"
	"fmt"
	"testing"

	"watchpoint/internal/types"
)

func TestIsBlocklistError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "sentinel ErrRecipientBlocked",
			err:  ErrRecipientBlocked,
			want: true,
		},
		{
			name: "wrapped sentinel ErrRecipientBlocked",
			err:  fmt.Errorf("send failed: %w", ErrRecipientBlocked),
			want: true,
		},
		{
			name: "AppError with ErrCodeEmailBlocked",
			err: types.NewAppError(
				types.ErrCodeEmailBlocked,
				"recipient on suppression list",
				nil,
			),
			want: true,
		},
		{
			name: "wrapped AppError with ErrCodeEmailBlocked",
			err: fmt.Errorf("delivery failed: %w", types.NewAppError(
				types.ErrCodeEmailBlocked,
				"blocked",
				nil,
			)),
			want: true,
		},
		{
			name: "generic error",
			err:  errors.New("network timeout"),
			want: false,
		},
		{
			name: "AppError with different code",
			err: types.NewAppError(
				types.ErrCodeUpstreamUnavailable,
				"server down",
				nil,
			),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBlocklistError(tt.err)
			if got != tt.want {
				t.Errorf("IsBlocklistError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrRecipientBlockedIsDistinct(t *testing.T) {
	// Verify ErrRecipientBlocked is its own sentinel, not an AppError.
	if errors.Is(ErrRecipientBlocked, nil) {
		t.Error("ErrRecipientBlocked should not be nil")
	}

	var appErr *types.AppError
	if errors.As(ErrRecipientBlocked, &appErr) {
		t.Error("ErrRecipientBlocked should not be an AppError")
	}
}

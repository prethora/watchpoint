package core

import "log/slog"

// Validator wraps go-playground/validator to register domain-specific rules.
// The full implementation (custom tags, ValidateStruct, etc.) is provided
// in a separate task; this file defines the type and constructor stub so
// that the Server struct compiles and NewServer can initialize it.
type Validator struct {
	logger *slog.Logger
}

// NewValidator creates a new Validator and registers custom validation tags.
// Stub: the full implementation is provided in a subsequent task.
func NewValidator(logger *slog.Logger) *Validator {
	return &Validator{
		logger: logger,
	}
}

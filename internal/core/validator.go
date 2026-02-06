package core

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"

	"watchpoint/internal/types"
)

// ValidationResult contains both blocking errors and non-blocking warnings.
// Allows the API to accept valid but deprecated inputs while signaling migration needs.
type ValidationResult struct {
	// Errors are blocking validation failures that prevent request processing.
	Errors []ValidationError `json:"errors,omitempty"`

	// Warnings are non-blocking advisory messages (e.g., deprecated webhook URLs).
	// The request proceeds but warnings should be surfaced to the client.
	Warnings []string `json:"warnings,omitempty"`
}

// ValidationError represents a single field-level validation failure.
type ValidationError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IsValid returns true if there are no blocking errors.
func (v ValidationResult) IsValid() bool {
	return len(v.Errors) == 0
}

// Validator wraps go-playground/validator to register domain-specific rules.
// It provides struct-level validation with custom tags for CONUS bounds checking,
// SSRF-safe URL validation, and IANA timezone validation.
type Validator struct {
	validate *validator.Validate
	logger   *slog.Logger
}

// dnsResolverFunc abstracts DNS resolution to allow testing with mock resolvers.
// In production this is net.DefaultResolver.LookupIPAddr; tests inject a fake.
type dnsResolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// dnsResolver is the package-level DNS resolution function. It defaults to
// the standard library resolver but can be overridden in tests.
var dnsResolver dnsResolverFunc = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// ssrfDNSTimeout is the maximum time allowed for DNS resolution during
// SSRF URL validation. The architecture specifies a strict 500ms timeout.
const ssrfDNSTimeout = 500 * time.Millisecond

// parsedSSRFBlockedCIDRs holds the pre-parsed CIDR networks from
// types.SSRFBlockedCIDRs. These are parsed once at init time to avoid
// repeated parsing during validation.
var parsedSSRFBlockedCIDRs []*net.IPNet

func init() {
	for _, cidr := range types.SSRFBlockedCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			// This should never happen with the well-known CIDR list.
			// Panic at init is acceptable because a misconfigured blocklist
			// is a critical security failure.
			panic(fmt.Sprintf("invalid SSRF blocked CIDR %q: %v", cidr, err))
		}
		parsedSSRFBlockedCIDRs = append(parsedSSRFBlockedCIDRs, network)
	}
}

// NewValidator creates a new Validator and registers custom validation tags.
// Custom tags registered:
//   - "is_conus": validates that Lat/Lon fields are within the US CONUS bounding box
//   - "ssrf_url": validates webhook URLs against private IP blocklists using SSRF protection
//   - "is_timezone": validates IANA timezone strings
func NewValidator(logger *slog.Logger) *Validator {
	v := validator.New(validator.WithRequiredStructEnabled())

	val := &Validator{
		validate: v,
		logger:   logger,
	}

	// Register custom validation tags by binding them to the pure logic
	// functions from internal/types/validation.go.
	_ = v.RegisterValidation("is_conus", val.validateIsCONUS)
	_ = v.RegisterValidation("ssrf_url", val.validateSSRFURL)
	_ = v.RegisterValidation("is_timezone", val.validateIsTimezone)

	return val
}

// ValidateStruct validates a struct using registered validation tags and
// returns a structured types.AppError on failure.
//
// DEPRECATED: Use ValidateStructWithWarnings for new code paths.
func (v *Validator) ValidateStruct(i interface{}) error {
	result := v.ValidateStructWithWarnings(i)
	if !result.IsValid() {
		return v.toAppError(result.Errors)
	}
	return nil
}

// ValidateStructWithWarnings performs validation and returns both errors and warnings.
// This method supports the ValidationResult pattern that separates blocking errors
// from non-blocking warnings (e.g., deprecated webhook platforms).
//
// Usage:
//
//	result := v.ValidateStructWithWarnings(req)
//	if !result.IsValid() {
//	    return types.NewValidationError(result.Errors)
//	}
//	// Proceed with request, append result.Warnings to response meta
func (v *Validator) ValidateStructWithWarnings(i interface{}) ValidationResult {
	err := v.validate.Struct(i)
	if err == nil {
		return ValidationResult{}
	}

	var validationErrors validator.ValidationErrors
	var ok bool
	if validationErrors, ok = err.(validator.ValidationErrors); !ok {
		// Non-validation error (e.g., invalid input type). Treat as a single error.
		return ValidationResult{
			Errors: []ValidationError{
				{
					Field:   "",
					Code:    string(types.ErrCodeValidationMissingField),
					Message: err.Error(),
				},
			},
		}
	}

	result := ValidationResult{}
	for _, fe := range validationErrors {
		result.Errors = append(result.Errors, mapFieldError(fe))
	}
	return result
}

// toAppError converts a slice of ValidationErrors into a single *types.AppError.
// Uses the first error's code as the primary error code and includes all errors
// in the details.
func (v *Validator) toAppError(errs []ValidationError) *types.AppError {
	if len(errs) == 0 {
		return nil
	}

	code := types.ErrorCode(errs[0].Code)

	// Build a summary message from all errors.
	messages := make([]string, len(errs))
	for i, e := range errs {
		messages[i] = fmt.Sprintf("%s: %s", e.Field, e.Message)
	}
	message := strings.Join(messages, "; ")

	// Include individual errors in details for structured client consumption.
	details := map[string]any{
		"validation_errors": errs,
	}

	return types.NewAppErrorWithDetails(code, message, nil, details)
}

// mapFieldError converts a go-playground/validator FieldError into our
// domain ValidationError, mapping tag names to appropriate error codes.
func mapFieldError(fe validator.FieldError) ValidationError {
	field := fe.Field()
	code := tagToErrorCode(fe.Tag())
	message := fieldErrorMessage(fe)

	return ValidationError{
		Field:   field,
		Code:    code,
		Message: message,
	}
}

// tagToErrorCode maps a validation tag name to the corresponding
// domain error code string.
func tagToErrorCode(tag string) string {
	switch tag {
	case "is_conus":
		return string(types.ErrCodeValidationInvalidLat) // CONUS is a lat/lon constraint
	case "ssrf_url":
		return string(types.ErrCodeValidationInvalidWebhook)
	case "is_timezone":
		return string(types.ErrCodeValidationInvalidTimezone)
	case "required":
		return string(types.ErrCodeValidationMissingField)
	case "email":
		return string(types.ErrCodeValidationInvalidEmail)
	case "max":
		return string(types.ErrCodeValidationMissingField)
	case "latitude":
		return string(types.ErrCodeValidationInvalidLat)
	case "longitude":
		return string(types.ErrCodeValidationInvalidLon)
	case "excluded_with":
		return string(types.ErrCodeValidationInvalidConditions)
	default:
		return string(types.ErrCodeValidationMissingField)
	}
}

// fieldErrorMessage produces a human-readable message for a field validation error.
func fieldErrorMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "this field is required"
	case "email":
		return "must be a valid email address"
	case "is_conus":
		return "coordinates must be within the Continental US (CONUS) bounding box"
	case "ssrf_url":
		return "webhook URL failed SSRF validation"
	case "is_timezone":
		return "must be a valid IANA timezone (e.g., America/New_York)"
	case "max":
		return fmt.Sprintf("must be at most %s", fe.Param())
	case "min":
		return fmt.Sprintf("must be at least %s", fe.Param())
	case "latitude":
		return "must be a valid latitude (-90 to 90)"
	case "longitude":
		return "must be a valid longitude (-180 to 180)"
	case "excluded_with":
		return fmt.Sprintf("cannot be specified together with %s", fe.Param())
	case "url":
		return "must be a valid URL"
	case "https":
		return "must use HTTPS scheme"
	default:
		return fmt.Sprintf("failed validation: %s", fe.Tag())
	}
}

// validateIsCONUS is the custom validator for the "is_conus" tag.
// It expects to be used on a struct that has Lat and Lon float64 fields.
// When applied to a Lat field, it checks the parent struct for a Lon field
// (and vice versa) and validates both coordinates are within CONUS bounds.
func (v *Validator) validateIsCONUS(fl validator.FieldLevel) bool {
	// Get the field value as float64.
	lat := fl.Field().Float()

	// Get the parent struct to find the companion coordinate.
	parent := fl.Parent()

	// Determine which field this is applied to and get the other coordinate.
	fieldName := fl.FieldName()

	var lon float64
	if strings.EqualFold(fieldName, "Lat") || strings.EqualFold(fieldName, "Latitude") {
		lonField := parent.FieldByName("Lon")
		if !lonField.IsValid() {
			lonField = parent.FieldByName("Longitude")
		}
		if !lonField.IsValid() {
			return false
		}
		lon = lonField.Float()
		return types.IsCONUS(lat, lon)
	}

	// If applied to the Lon field, get Lat from parent.
	if strings.EqualFold(fieldName, "Lon") || strings.EqualFold(fieldName, "Longitude") {
		latField := parent.FieldByName("Lat")
		if !latField.IsValid() {
			latField = parent.FieldByName("Latitude")
		}
		if !latField.IsValid() {
			return false
		}
		lon = lat
		lat = latField.Float()
		return types.IsCONUS(lat, lon)
	}

	// If not on a recognized field name, just check as latitude with lon=0
	// (which will fail CONUS check since lon=0 is not in CONUS range).
	return false
}

// validateSSRFURL is the custom validator for the "ssrf_url" tag.
// It validates webhook URLs against SSRF protection:
//  1. Invokes types.ValidateWebhookURL for HTTPS scheme check.
//  2. Performs DNS resolution within a context with a strict 500ms timeout.
//  3. Checks resolved IPs against SSRFBlockedCIDRs.
//  4. Fail Closed: If DNS resolution times out, validation MUST fail.
func (v *Validator) validateSSRFURL(fl validator.FieldLevel) bool {
	urlStr := fl.Field().String()
	if urlStr == "" {
		// Empty strings should be caught by "required" tag if mandatory.
		return true
	}

	// Step 1: Basic URL validation (HTTPS scheme check).
	if err := types.ValidateWebhookURL(urlStr); err != nil {
		return false
	}

	// Step 2: Parse URL to extract host.
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	if host == "" {
		return false
	}

	// Check if the host is an IP literal (skip DNS resolution).
	if ip := net.ParseIP(host); ip != nil {
		return !isBlockedIP(ip)
	}

	// Step 3: DNS resolution with strict 500ms timeout (Fail Closed).
	ctx, cancel := context.WithTimeout(context.Background(), ssrfDNSTimeout)
	defer cancel()

	addrs, err := dnsResolver(ctx, host)
	if err != nil {
		// Fail Closed: DNS timeout or failure means we cannot verify the URL.
		v.logger.Warn("SSRF validation DNS resolution failed",
			"host", host,
			"error", err,
		)
		return false
	}

	if len(addrs) == 0 {
		// No resolved addresses: fail closed.
		return false
	}

	// Step 4: Check all resolved IPs against blocked CIDRs.
	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			v.logger.Warn("SSRF validation blocked IP",
				"host", host,
				"ip", addr.IP.String(),
			)
			return false
		}
	}

	return true
}

// isBlockedIP checks if the given IP falls within any of the pre-parsed
// SSRF blocked CIDR ranges.
func isBlockedIP(ip net.IP) bool {
	for _, network := range parsedSSRFBlockedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// validateIsTimezone is the custom validator for the "is_timezone" tag.
// It validates that the field value is a valid IANA timezone string by
// attempting to load the timezone location.
func (v *Validator) validateIsTimezone(fl validator.FieldLevel) bool {
	tz := fl.Field().String()
	if tz == "" {
		// Empty strings should be caught by "required" tag if mandatory.
		return true
	}

	_, err := time.LoadLocation(tz)
	return err == nil
}

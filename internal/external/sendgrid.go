package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// sendGridAPIBase is the default SendGrid API base URL.
// Overridable in tests via SendGridClientConfig.BaseURL.
const sendGridAPIBase = "https://api.sendgrid.com"

// SendGridClientConfig holds the configuration for creating a SendGridClient.
type SendGridClientConfig struct {
	APIKey  string
	BaseURL string // Override for testing; defaults to sendGridAPIBase
	Logger  *slog.Logger
}

// SendGridClient implements EmailProvider by making direct HTTP calls to the
// SendGrid v3 Mail Send API through BaseClient. This approach routes all
// requests through the platform's resilience infrastructure (circuit breaker,
// retries, error mapping) and makes testing with httptest straightforward.
type SendGridClient struct {
	base    *BaseClient
	apiKey  string
	baseURL string
	logger  *slog.Logger
}

// NewSendGridClient creates a new SendGridClient. The httpClient timeout should
// be set to 10 seconds as specified in the architecture (Section 5.4).
func NewSendGridClient(
	httpClient *http.Client,
	cfg SendGridClientConfig,
) *SendGridClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = sendGridAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	base := NewBaseClient(
		httpClient,
		"sendgrid",
		RetryPolicy{
			MaxRetries: 2,
			MinWait:    500 * time.Millisecond,
			MaxWait:    5 * time.Second,
		},
		"WatchPoint/1.0",
		WithSleepFunc(time.Sleep),
	)

	return &SendGridClient{
		base:    base,
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		logger:  logger,
	}
}

// NewSendGridClientWithBase creates a SendGridClient with a pre-configured
// BaseClient. This is useful for testing when you want to control the
// BaseClient configuration (e.g., disable retries).
func NewSendGridClientWithBase(
	base *BaseClient,
	cfg SendGridClientConfig,
) *SendGridClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = sendGridAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &SendGridClient{
		base:    base,
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		logger:  logger,
	}
}

// ---------------------------------------------------------------------------
// EmailProvider Implementation
// ---------------------------------------------------------------------------

// Send transmits an email using SendGrid's v3 Mail Send API with Dynamic
// Templates. It maps the domain types.SendInput to the SendGrid mail/send
// JSON payload and returns the provider message ID (from the
// X-Message-Id response header) on success.
//
// Error mapping:
//   - 403 Forbidden -> types.ErrCodeEmailBlocked (recipient on suppression list)
//   - 429 Too Many Requests -> handled by BaseClient (retry + ErrCodeUpstreamRateLimited)
//   - 5xx -> handled by BaseClient (retry + ErrCodeUpstreamUnavailable)
//   - Other 4xx -> types.ErrCodeUpstreamEmailProvider
func (s *SendGridClient) Send(ctx context.Context, input types.SendInput) (string, error) {
	// Build the SendGrid v3 mail send payload.
	payload := s.buildMailPayload(input)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to marshal SendGrid mail payload",
			err,
		)
	}

	reqURL := s.baseURL + "/v3/mail/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create SendGrid mail send request",
			err,
		)
	}

	req.Header.Set("Content-Type", "application/json")
	s.setAuthHeaders(req)

	resp, err := s.base.Do(req)
	if err != nil {
		return "", s.wrapSendGridError("Send", err)
	}
	defer resp.Body.Close()

	// SendGrid returns 202 Accepted on success.
	if resp.StatusCode == http.StatusAccepted {
		msgID := resp.Header.Get("X-Message-Id")
		return msgID, nil
	}

	// Map specific HTTP status codes to domain errors.
	return "", s.handleErrorResponse(resp, "Send")
}

// ---------------------------------------------------------------------------
// Payload Construction
// ---------------------------------------------------------------------------

// sendGridMailPayload represents the SendGrid v3 mail/send JSON request body
// using Dynamic Templates.
type sendGridMailPayload struct {
	Personalizations []sendGridPersonalization `json:"personalizations"`
	From             sendGridAddress           `json:"from"`
	TemplateID       string                    `json:"template_id"`
	// custom_args allows correlation with internal notification IDs
	CustomArgs map[string]string `json:"custom_args,omitempty"`
}

type sendGridPersonalization struct {
	To              []sendGridAddress      `json:"to"`
	DynamicData     map[string]interface{} `json:"dynamic_template_data,omitempty"`
}

type sendGridAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// buildMailPayload maps a domain types.SendInput to the SendGrid v3 payload.
func (s *SendGridClient) buildMailPayload(input types.SendInput) sendGridMailPayload {
	personalization := sendGridPersonalization{
		To: []sendGridAddress{
			{Email: input.To},
		},
		DynamicData: input.TemplateData,
	}

	payload := sendGridMailPayload{
		Personalizations: []sendGridPersonalization{personalization},
		From: sendGridAddress{
			Email: input.From.Address,
			Name:  input.From.Name,
		},
		TemplateID: input.TemplateID,
	}

	// Set custom_args for correlation with internal notification tracking.
	if input.ReferenceID != "" {
		payload.CustomArgs = map[string]string{
			"reference_id": input.ReferenceID,
		}
	}

	return payload
}

// ---------------------------------------------------------------------------
// HTTP Helpers
// ---------------------------------------------------------------------------

// setAuthHeaders sets the SendGrid API authentication headers.
func (s *SendGridClient) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
}

// ---------------------------------------------------------------------------
// Error Handling
// ---------------------------------------------------------------------------

// sendGridErrorResponse represents the JSON error body returned by SendGrid.
type sendGridErrorResponse struct {
	Errors []sendGridErrorDetail `json:"errors"`
}

type sendGridErrorDetail struct {
	Message string `json:"message"`
	Field   string `json:"field"`
	Help    string `json:"help"`
}

// handleErrorResponse reads a SendGrid error response and maps it to a
// types.AppError. The key mapping per architecture spec (Section 3.2):
//   - 403 Forbidden -> types.ErrCodeEmailBlocked
//   - Other -> types.ErrCodeUpstreamEmailProvider
func (s *SendGridClient) handleErrorResponse(resp *http.Response, operation string) error {
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return types.NewAppError(
			types.ErrCodeUpstreamEmailProvider,
			fmt.Sprintf("%s: SendGrid returned status %d and response body was unreadable", operation, resp.StatusCode),
			readErr,
		)
	}

	var sgErr sendGridErrorResponse
	errMsg := ""
	if jsonErr := json.Unmarshal(body, &sgErr); jsonErr == nil && len(sgErr.Errors) > 0 {
		errMsg = sgErr.Errors[0].Message
	} else {
		errMsg = string(body)
	}

	return s.mapSendGridError(operation, resp.StatusCode, errMsg)
}

// mapSendGridError translates a SendGrid HTTP error into a types.AppError.
func (s *SendGridClient) mapSendGridError(operation string, statusCode int, message string) error {
	switch {
	case statusCode == http.StatusForbidden:
		// 403: Recipient is on suppression list / blocked.
		return types.NewAppError(
			types.ErrCodeEmailBlocked,
			fmt.Sprintf("%s: SendGrid blocked delivery: %s", operation, message),
			nil,
		)
	case statusCode == http.StatusTooManyRequests:
		return types.NewAppError(
			types.ErrCodeUpstreamRateLimited,
			fmt.Sprintf("%s: SendGrid rate limit exceeded", operation),
			nil,
		)
	case statusCode >= 500:
		return types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			fmt.Sprintf("%s: SendGrid server error: %s", operation, message),
			nil,
		)
	default:
		return types.NewAppError(
			types.ErrCodeUpstreamEmailProvider,
			fmt.Sprintf("%s: SendGrid error (%d): %s", operation, statusCode, message),
			nil,
		)
	}
}

// wrapSendGridError wraps a BaseClient transport error with context.
func (s *SendGridClient) wrapSendGridError(operation string, err error) error {
	// If it's already an AppError from BaseClient (circuit breaker, retries exhausted),
	// return it as-is since it already has the right error code.
	if _, ok := err.(*types.AppError); ok {
		return err
	}
	return types.NewAppError(
		types.ErrCodeUpstreamEmailProvider,
		fmt.Sprintf("%s: SendGrid request failed: %v", operation, err),
		err,
	)
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertion that SendGridClient satisfies EmailProvider.
var _ EmailProvider = (*SendGridClient)(nil)

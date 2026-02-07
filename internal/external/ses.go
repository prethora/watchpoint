package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"watchpoint/internal/types"
)

// SESAPI defines the subset of the SES v2 client used by SESClient.
// Extracted for testability — tests can provide a mock implementation.
type SESAPI interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SESClientConfig holds the configuration for creating an SESClient.
type SESClientConfig struct {
	// ConfigSetName is the SES configuration set name for tracking.
	// Optional; if empty, no configuration set is used.
	ConfigSetName string
	// Logger for SES operations.
	Logger *slog.Logger
}

// SESClient implements EmailProvider using AWS SES v2.
// Authentication is handled via IAM roles (no API key required).
// The AWS SDK provides built-in retry logic, so no BaseClient wrapper is needed.
type SESClient struct {
	api           SESAPI
	configSetName string
	logger        *slog.Logger
}

// NewSESClient creates a new SESClient from an AWS config.
func NewSESClient(awsCfg aws.Config, cfg SESClientConfig) *SESClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &SESClient{
		api:           sesv2.NewFromConfig(awsCfg),
		configSetName: cfg.ConfigSetName,
		logger:        logger,
	}
}

// NewSESClientWithAPI creates an SESClient with a pre-configured SESAPI.
// Useful for testing with a mock SES interface.
func NewSESClientWithAPI(api SESAPI, cfg SESClientConfig) *SESClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &SESClient{
		api:           api,
		configSetName: cfg.ConfigSetName,
		logger:        logger,
	}
}

// Send transmits an email using AWS SES v2 SendEmail with simple content
// (Subject, Body.Html, Body.Text). The input carries pre-rendered content
// — no server-side templates.
//
// Error mapping:
//   - MessageRejected → ErrCodeEmailBlocked
//   - TooManyRequestsException → ErrCodeUpstreamRateLimited
//   - SendingPausedException → ErrCodeUpstreamUnavailable
//   - Other → ErrCodeUpstreamEmailProvider
func (s *SESClient) Send(ctx context.Context, input types.SendInput) (string, error) {
	fromAddr := fmt.Sprintf("%s <%s>", input.From.Name, input.From.Address)
	if input.From.Name == "" {
		fromAddr = input.From.Address
	}

	emailInput := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(fromAddr),
		Destination: &sestypes.Destination{
			ToAddresses: []string{input.To},
		},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{
					Data:    aws.String(input.Subject),
					Charset: aws.String("UTF-8"),
				},
				Body: &sestypes.Body{},
			},
		},
	}

	// Set HTML body if provided.
	if input.BodyHTML != "" {
		emailInput.Content.Simple.Body.Html = &sestypes.Content{
			Data:    aws.String(input.BodyHTML),
			Charset: aws.String("UTF-8"),
		}
	}

	// Set plaintext body if provided.
	if input.BodyText != "" {
		emailInput.Content.Simple.Body.Text = &sestypes.Content{
			Data:    aws.String(input.BodyText),
			Charset: aws.String("UTF-8"),
		}
	}

	// Set configuration set for tracking if configured.
	if s.configSetName != "" {
		emailInput.ConfigurationSetName = aws.String(s.configSetName)
	}

	// Tag the message with ReferenceID for correlation.
	if input.ReferenceID != "" {
		emailInput.EmailTags = []sestypes.MessageTag{
			{
				Name:  aws.String("ReferenceID"),
				Value: aws.String(input.ReferenceID),
			},
		}
	}

	result, err := s.api.SendEmail(ctx, emailInput)
	if err != nil {
		return "", mapSESError(err)
	}

	msgID := ""
	if result.MessageId != nil {
		msgID = *result.MessageId
	}

	return msgID, nil
}

// mapSESError translates AWS SES errors into domain AppErrors.
func mapSESError(err error) error {
	var msgRejected *sestypes.MessageRejected
	if errors.As(err, &msgRejected) {
		return types.NewAppError(
			types.ErrCodeEmailBlocked,
			fmt.Sprintf("SES rejected message: %v", err),
			err,
		)
	}

	var tooManyReqs *sestypes.TooManyRequestsException
	if errors.As(err, &tooManyReqs) {
		return types.NewAppError(
			types.ErrCodeUpstreamRateLimited,
			fmt.Sprintf("SES rate limit exceeded: %v", err),
			err,
		)
	}

	var sendingPaused *sestypes.SendingPausedException
	if errors.As(err, &sendingPaused) {
		return types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			fmt.Sprintf("SES account sending paused: %v", err),
			err,
		)
	}

	return types.NewAppError(
		types.ErrCodeUpstreamEmailProvider,
		fmt.Sprintf("SES error: %v", err),
		err,
	)
}

// Compile-time assertion that SESClient satisfies EmailProvider.
var _ EmailProvider = (*SESClient)(nil)

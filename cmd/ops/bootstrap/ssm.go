package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMClient defines the subset of the AWS SSM API required by the bootstrap
// tool. This interface enables unit testing with mocks without requiring
// a live AWS or LocalStack connection.
type SSMClient interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

// GetParameterValue retrieves the value of an SSM parameter. If decrypt is
// true, SecureString parameters are decrypted before being returned. This
// is used by the --export-env flag to read back secrets for local development.
//
// The path parameter must be an absolute SSM path (e.g., "/dev/watchpoint/database/url").
// Use SSMPath() to construct it from a category/key.
//
// Security note: The decrypted value is returned in plaintext. The caller
// is responsible for handling it securely (e.g., writing to a file with
// restricted permissions, not logging it).
func (m *SSMManager) GetParameterValue(ctx context.Context, path string, decrypt bool) (string, error) {
	opCtx, cancel := context.WithTimeout(ctx, ssmOperationTimeout)
	defer cancel()

	output, err := m.client.GetParameter(opCtx, &ssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(decrypt),
	})
	if err != nil {
		return "", fmt.Errorf("reading SSM parameter %q: %w", path, err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return "", fmt.Errorf("SSM parameter %q has no value", path)
	}

	value := aws.ToString(output.Parameter.Value)

	// Log retrieval without exposing the value for SecureString parameters.
	if decrypt {
		m.logger.Info("SSM parameter read",
			"path", path,
			"value_length", len(value),
		)
	} else {
		m.logger.Info("SSM parameter read",
			"path", path,
			"value", value,
		)
	}

	return value, nil
}

// SSMManager provides operations for managing AWS SSM Parameter Store
// parameters during the bootstrap process. It wraps the SSM client with
// environment-aware path construction, logging, and error handling.
//
// Architecture reference: 13-human-setup.md (Section 4 - SSM Injection)
// Config reference: 03-config.md (Section 4.1 - SSM Path Hierarchy)
type SSMManager struct {
	client SSMClient
	env    string
	logger *slog.Logger
}

// ssmOperationTimeout is the per-operation timeout for SSM API calls.
// This is intentionally generous to accommodate cross-region latency
// and IAM permission propagation delays during initial setup.
const ssmOperationTimeout = 15 * time.Second

// NewSSMManager creates a new SSMManager from the BootstrapContext.
// It initializes the SSM client using the AWS config established
// during session initialization.
func NewSSMManager(bctx *BootstrapContext) *SSMManager {
	client := ssm.NewFromConfig(bctx.AWSConfig)
	return &SSMManager{
		client: client,
		env:    bctx.Environment,
		logger: bctx.Logger,
	}
}

// NewSSMManagerWithClient creates a new SSMManager with an injected SSM
// client. This constructor is intended for testing.
func NewSSMManagerWithClient(client SSMClient, env string, logger *slog.Logger) *SSMManager {
	return &SSMManager{
		client: client,
		env:    env,
		logger: logger,
	}
}

// SSMPath constructs the full SSM parameter path by interpolating the
// environment into the path template.
//
// The path format follows the convention defined in 03-config.md Section 4.1:
//
//	/{environment}/watchpoint/{category}/{key}
//
// The category and key portion is provided by the caller. For example,
// passing "database/url" with env "dev" produces "/dev/watchpoint/database/url".
func (m *SSMManager) SSMPath(categoryAndKey string) string {
	return fmt.Sprintf("/%s/watchpoint/%s", m.env, categoryAndKey)
}

// ParameterExists checks whether a parameter already exists in SSM at the
// given absolute path. It returns true if the parameter is found, false if
// it does not exist, and an error for any unexpected failures.
//
// This implements the idempotency check described in 13-human-setup.md
// Section 1 (Agent Directives, point 2): "Before asking for a value, always
// probe AWS SSM to see if it already exists."
//
// The path parameter must be an absolute SSM path (e.g., "/dev/watchpoint/database/url").
// Use SSMPath() to construct it from a category/key.
func (m *SSMManager) ParameterExists(ctx context.Context, path string) (bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, ssmOperationTimeout)
	defer cancel()

	_, err := m.client.GetParameter(opCtx, &ssm.GetParameterInput{
		Name: aws.String(path),
		// We do not need the decrypted value for an existence check.
		// WithDecryption=false avoids needing kms:Decrypt permissions
		// just to probe for existence.
		WithDecryption: aws.Bool(false),
	})
	if err != nil {
		// Check if the error indicates the parameter does not exist.
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("checking SSM parameter %q: %w", path, err)
	}

	return true, nil
}

// PutSecret writes a SecureString parameter to SSM Parameter Store.
// SecureString parameters are encrypted at rest using the default AWS KMS key.
//
// If overwrite is false and the parameter already exists, the operation will
// return an error. If overwrite is true, the existing value is replaced.
//
// This corresponds to the Secret Inventory entries with Type "SecureString"
// in 13-human-setup.md Section 4.
//
// Security note: The value is never logged. Only the path and a length
// indicator are included in log output, following the Agent Directive
// in Section 1: "NEVER echo the user's input back to the console or logs."
func (m *SSMManager) PutSecret(ctx context.Context, path string, value string, overwrite bool) error {
	return m.putParameter(ctx, path, value, ssmtypes.ParameterTypeSecureString, overwrite)
}

// PutString writes a standard String parameter to SSM Parameter Store.
// String parameters are stored in plaintext and are appropriate for
// non-sensitive configuration values like the RunPod Endpoint ID or
// the Stripe Publishable Key.
//
// String parameters are always written with overwrite=true since they
// hold non-sensitive values that may need updating (e.g., the RunPod
// Endpoint ID is initially set to "pending_setup" and later updated).
func (m *SSMManager) PutString(ctx context.Context, path string, value string) error {
	return m.putParameter(ctx, path, value, ssmtypes.ParameterTypeString, true)
}

// putParameter is the shared implementation for writing parameters to SSM.
// It handles timeout, logging, and error classification.
func (m *SSMManager) putParameter(ctx context.Context, path, value string, paramType ssmtypes.ParameterType, overwrite bool) error {
	if path == "" {
		return fmt.Errorf("SSM parameter path must not be empty")
	}
	if value == "" {
		return fmt.Errorf("SSM parameter value must not be empty for path %q", path)
	}

	opCtx, cancel := context.WithTimeout(ctx, ssmOperationTimeout)
	defer cancel()

	input := &ssm.PutParameterInput{
		Name:      aws.String(path),
		Value:     aws.String(value),
		Type:      paramType,
		Overwrite: aws.Bool(overwrite),
	}

	_, err := m.client.PutParameter(opCtx, input)
	if err != nil {
		// Check for the specific case where the parameter already exists
		// and overwrite was not requested.
		var alreadyExists *ssmtypes.ParameterAlreadyExists
		if errors.As(err, &alreadyExists) {
			m.logger.Warn("SSM parameter already exists (use overwrite to replace)",
				"path", path,
				"type", string(paramType),
			)
			return fmt.Errorf("SSM parameter %q already exists: %w", path, err)
		}
		return fmt.Errorf("writing SSM parameter %q: %w", path, err)
	}

	// Log success with the value length (not the value itself) for
	// SecureString types, and the actual value for String types.
	if paramType == ssmtypes.ParameterTypeSecureString {
		m.logger.Info("SSM parameter written",
			"path", path,
			"type", string(paramType),
			"value_length", len(value),
		)
	} else {
		m.logger.Info("SSM parameter written",
			"path", path,
			"type", string(paramType),
			"value", value,
		)
	}

	return nil
}

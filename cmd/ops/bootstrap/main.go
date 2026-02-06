// Package main implements the bootstrap CLI tool for the WatchPoint platform.
//
// This tool guides a human operator through the manual bootstrapping process:
// creating external accounts, collecting secrets, and populating AWS SSM
// Parameter Store with the configuration values required before the first
// Infrastructure-as-Code deployment.
//
// Usage:
//
//	go run ./cmd/ops/bootstrap --env=dev
//	go run ./cmd/ops/bootstrap --env=dev --export-env
//	go run ./cmd/ops/bootstrap --env=prod --profile=watchpoint-prod --region=us-east-1
//
// The tool performs the following:
//  1. Parses --env, --profile, --region, --export-env, and --export-env-path flags.
//  2. Initializes the AWS SDK v2 session with the specified profile/region.
//  3. Calls STS GetCallerIdentity to verify the active AWS identity.
//  4. If --env=prod, requires explicit interactive confirmation ("yes").
//  5. Prints a summary banner with account ID, environment, and region.
//  6. Walks through the bootstrap protocol: collecting external secrets,
//     generating internal keys, and writing all parameters to SSM.
//  7. If --export-env is set, reads SSM parameters back and writes a .env
//     file for local development use (bridges bootstrap to Phase 9).
//
// Architecture reference: architecture/13-human-setup.md (Sections 1, 2)
// Operations reference: architecture/12-operations.md (Section 3.1)
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Supported environments for the bootstrap tool.
var validEnvironments = map[string]bool{
	"dev":     true,
	"staging": true,
	"prod":    true,
}

// BootstrapContext holds the session-wide context established during
// initialization. It is threaded through subsequent bootstrap phases.
type BootstrapContext struct {
	// Environment is the target deployment environment (dev/staging/prod).
	Environment string

	// AWSProfile is the AWS CLI profile used for authentication.
	AWSProfile string

	// AWSRegion is the target AWS region.
	AWSRegion string

	// AccountID is the AWS account ID resolved via STS GetCallerIdentity.
	AccountID string

	// CallerARN is the full ARN of the authenticated identity.
	CallerARN string

	// AWSConfig is the resolved AWS SDK configuration for reuse by
	// subsequent bootstrap phases (SSM, etc.).
	AWSConfig aws.Config

	// Logger is the structured logger for the session.
	Logger *slog.Logger
}

func main() {
	// Parse command-line flags.
	envFlag := flag.String("env", "", "Target environment (dev/staging/prod) [required]")
	profileFlag := flag.String("profile", "", "AWS CLI profile (default: uses default credential chain)")
	regionFlag := flag.String("region", "us-east-1", "AWS region")
	exportEnvFlag := flag.Bool("export-env", false, "After bootstrap, export SSM parameters to a .env file for local development")
	exportEnvPath := flag.String("export-env-path", ".env", "Path for the exported .env file (default: .env in project root)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "WatchPoint Bootstrap Tool\n\n")
		fmt.Fprintf(os.Stderr, "Guides the setup of external accounts and AWS SSM parameters\n")
		fmt.Fprintf(os.Stderr, "required before the first SAM deployment.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  bootstrap --env=dev [--profile=NAME] [--region=REGION] [--export-env]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Validate --env is provided and recognized.
	if *envFlag == "" {
		fmt.Fprintf(os.Stderr, "error: --env is required\n\n")
		flag.Usage()
		os.Exit(1)
	}
	if !validEnvironments[*envFlag] {
		fmt.Fprintf(os.Stderr, "error: invalid environment %q (must be dev, staging, or prod)\n", *envFlag)
		os.Exit(1)
	}

	// Initialize structured logger.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Set up cancellation context with signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Initialize the bootstrap context.
	bctx, err := initializeSession(ctx, *envFlag, *profileFlag, *regionFlag, logger)
	if err != nil {
		logger.Error("initialization failed", "error", err)
		os.Exit(1)
	}

	// Production safety gate: require explicit confirmation.
	if bctx.Environment == "prod" {
		if !confirmProduction(bctx) {
			fmt.Fprintln(os.Stderr, "Aborted. No changes were made.")
			os.Exit(0)
		}
	}

	// Print the session banner.
	printBanner(bctx)

	// Run the main bootstrap loop.
	runner := NewBootstrapRunner(bctx)
	if err := runner.Run(ctx); err != nil {
		logger.Error("bootstrap failed", "error", err)
		os.Exit(1)
	}

	logger.Info("bootstrap completed successfully",
		"env", bctx.Environment,
		"account", bctx.AccountID,
		"region", bctx.AWSRegion,
	)

	// Export SSM parameters to a .env file if requested.
	if *exportEnvFlag {
		logger.Info("exporting SSM parameters to .env file",
			"path", *exportEnvPath,
		)

		exportCfg := ExportEnvConfig{
			OutputPath:           *exportEnvPath,
			Environment:          bctx.Environment,
			SSM:                  runner.SSM,
			Stderr:               os.Stderr,
			IncludeLocalDefaults: true,
		}

		if err := ExportEnvFile(ctx, exportCfg); err != nil {
			logger.Error("failed to export .env file", "error", err)
			os.Exit(1)
		}

		logger.Info(".env file exported successfully",
			"path", *exportEnvPath,
		)
	}
}

// initializeSession verifies prerequisite tooling, configures the AWS SDK
// session, and calls STS GetCallerIdentity to confirm the active identity.
//
// This corresponds to Section 2 (Session Initialization) of 13-human-setup.md.
func initializeSession(ctx context.Context, env, profile, region string, logger *slog.Logger) (*BootstrapContext, error) {
	// Build AWS config options.
	var opts []func(*awsconfig.LoadOptions) error

	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}

	// Load the AWS SDK configuration. This resolves credentials from the
	// default chain: environment -> shared credentials -> EC2 IMDS.
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	// Verify the active identity by calling STS GetCallerIdentity.
	// This also validates that credentials are functional before proceeding.
	stsClient := sts.NewFromConfig(cfg)

	// Use a short timeout for the identity check to fail fast on bad credentials.
	identityCtx, identityCancel := context.WithTimeout(ctx, 10*time.Second)
	defer identityCancel()

	identity, err := stsClient.GetCallerIdentity(identityCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("verifying AWS identity (STS GetCallerIdentity): %w\n"+
			"  Check that your AWS credentials are configured correctly.\n"+
			"  Profile: %q, Region: %q", err, profile, region)
	}

	accountID := aws.ToString(identity.Account)
	callerARN := aws.ToString(identity.Arn)

	logger.Info("AWS identity verified",
		"account_id", accountID,
		"arn", callerARN,
		"region", region,
	)

	return &BootstrapContext{
		Environment: env,
		AWSProfile:  profile,
		AWSRegion:   region,
		AccountID:   accountID,
		CallerARN:   callerARN,
		AWSConfig:   cfg,
		Logger:      logger,
	}, nil
}

// confirmProduction prompts the operator for explicit confirmation when
// targeting the production environment. This prevents accidental writes
// to production SSM parameters.
//
// Returns true if the operator types "yes" (case-insensitive), false otherwise.
func confirmProduction(bctx *BootstrapContext) bool {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintln(os.Stderr, "  WARNING: You are targeting the PRODUCTION environment")
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintf(os.Stderr, "  Account: %s\n", bctx.AccountID)
	fmt.Fprintf(os.Stderr, "  Region:  %s\n", bctx.AWSRegion)
	fmt.Fprintf(os.Stderr, "  ARN:     %s\n", bctx.CallerARN)
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Type 'yes' to continue: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}

	response := strings.TrimSpace(scanner.Text())
	return strings.EqualFold(response, "yes")
}

// printBanner displays a summary of the bootstrap session configuration.
func printBanner(bctx *BootstrapContext) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "------------------------------------------------------------")
	fmt.Fprintln(os.Stderr, "  WatchPoint Bootstrap")
	fmt.Fprintln(os.Stderr, "------------------------------------------------------------")
	fmt.Fprintf(os.Stderr, "  Environment:  %s\n", bctx.Environment)
	fmt.Fprintf(os.Stderr, "  AWS Account:  %s\n", bctx.AccountID)
	fmt.Fprintf(os.Stderr, "  AWS Region:   %s\n", bctx.AWSRegion)
	fmt.Fprintf(os.Stderr, "  Identity:     %s\n", bctx.CallerARN)
	if bctx.AWSProfile != "" {
		fmt.Fprintf(os.Stderr, "  Profile:      %s\n", bctx.AWSProfile)
	}
	fmt.Fprintf(os.Stderr, "  SSM Prefix:   /%s/watchpoint/\n", bctx.Environment)
	fmt.Fprintln(os.Stderr, "------------------------------------------------------------")
	fmt.Fprintln(os.Stderr)
}

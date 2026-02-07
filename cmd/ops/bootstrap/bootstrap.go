package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ParameterType indicates whether an SSM parameter is stored as a
// SecureString (encrypted) or a plain String.
type ParameterType int

const (
	// ParamSecureString corresponds to SSM SecureString type (encrypted at rest).
	ParamSecureString ParameterType = iota
	// ParamString corresponds to SSM String type (plaintext).
	ParamString
)

// InputSource describes how the value for a bootstrap step is obtained.
type InputSource int

const (
	// SourcePrompt means the operator must provide the value interactively.
	SourcePrompt InputSource = iota
	// SourceGenerated means the value is auto-generated internally.
	SourceGenerated
	// SourceFixed means the value is a hardcoded constant (e.g., "pending_setup").
	SourceFixed
)

// BootstrapStep defines a single parameter to be populated during the
// bootstrap process. Each step maps to one row in the Secret Inventory
// Table from 13-human-setup.md Section 4.
type BootstrapStep struct {
	// HumanLabel is the display name shown to the operator.
	// Example: "Database URL", "Stripe Secret Key"
	HumanLabel string

	// SSMCategoryKey is the category/key portion of the SSM path.
	// Example: "database/url" which becomes "/{env}/watchpoint/database/url".
	SSMCategoryKey string

	// ParamType determines whether the parameter is stored as SecureString
	// or String in SSM.
	ParamType ParameterType

	// Source determines how the value is obtained.
	Source InputSource

	// FixedValue is used when Source is SourceFixed.
	FixedValue string

	// Prompt is the instructional text shown to the operator when Source is SourcePrompt.
	Prompt string

	// ValidateFn is called to validate user input. It receives the context
	// and the raw user input, and returns a ValidationResult.
	// Nil means no validation is performed (the value is accepted as-is).
	ValidateFn func(ctx context.Context, input string) ValidationResult

	// IsSecret controls whether the input is masked during entry.
	// When true, the input is read without echoing to the terminal.
	IsSecret bool

	// Optional marks this step as skippable without user confirmation.
	// When SkipOptional is true in the runner, these steps are auto-skipped.
	Optional bool

	// Phase groups the step for display purposes (e.g., "External Accounts", "Internal Secrets").
	Phase string
}

// maxRetries is the maximum number of times the operator can retry entering
// a value before the bootstrap process aborts for that step.
const maxRetries = 5

// errSkipped is a sentinel error returned by promptAndValidate when the
// operator chooses to skip a parameter by entering empty input and then
// confirming the skip. This allows processStep to record the parameter
// as "skipped" without writing to SSM.
var errSkipped = errors.New("parameter skipped by operator")

// BuildInventory constructs the ordered list of bootstrap steps matching
// the Secret Inventory Table in 13-human-setup.md Section 4.
// The validator is injected to enable testing with mock HTTP/DB clients.
func BuildInventory(v *Validator) []BootstrapStep {
	return []BootstrapStep{
		// -----------------------------------------------------------------
		// Phase 1: External Accounts (13-human-setup.md Section 3)
		// -----------------------------------------------------------------
		{
			HumanLabel:     "Database URL",
			SSMCategoryKey: "database/url",
			ParamType:      ParamSecureString,
			Source:         SourcePrompt,
			Prompt: `1. Create a new Supabase Project.
   2. Go to Settings > Database.
   3. Under Connection Pooling, set Mode to Transaction.
   4. Copy the Connection String (port 6543) and replace the password.
   5. Paste the full postgres://... string here:`,
			ValidateFn: v.ValidateDatabaseURL,
			IsSecret:   true,
			Phase:      "External Accounts",
		},
		{
			HumanLabel:     "Stripe Secret Key",
			SSMCategoryKey: "billing/stripe_secret_key",
			ParamType:      ParamSecureString,
			Source:         SourcePrompt,
			Prompt: `1. Go to Stripe Dashboard > Developers > API Keys.
   2. Copy the Secret Key (sk_...).
   3. Paste it here:`,
			ValidateFn: v.ValidateStripeKey,
			IsSecret:   true,
			Phase:      "External Accounts",
		},
		{
			HumanLabel:     "Stripe Publishable Key",
			SSMCategoryKey: "billing/stripe_publishable_key",
			ParamType:      ParamString,
			Source:         SourcePrompt,
			Prompt:         `Now paste the Stripe Publishable Key (pk_...):`,
			ValidateFn: func(ctx context.Context, input string) ValidationResult {
				return v.ValidateRegex(ctx, input, `^pk_(test|live)_[0-9a-zA-Z]{24,}$`, "Stripe Publishable Key")
			},
			IsSecret: false,
			Phase:    "External Accounts",
		},
		{
			HumanLabel:     "RunPod API Key",
			SSMCategoryKey: "forecast/runpod_api_key",
			ParamType:      ParamSecureString,
			Source:         SourcePrompt,
			Prompt: `1. Go to RunPod > Settings > API Keys.
   2. Create and paste a new API Key:`,
			ValidateFn: v.ValidateRunPodKey,
			IsSecret:   true,
			Phase:      "External Accounts",
		},
		{
			HumanLabel:     "Google Client ID (optional)",
			SSMCategoryKey: "auth/google_client_id",
			ParamType:      ParamString,
			Source:         SourcePrompt,
			Prompt: `Use https://placeholder.watchpoint.io as the Authorized Redirect URI for now.
   Paste Google Client ID (or press Enter to skip):`,
			ValidateFn: func(ctx context.Context, input string) ValidationResult {
				return v.ValidateRegex(ctx, input, `.+\.apps\.googleusercontent\.com$`, "Google Client ID")
			},
			IsSecret: false,
			Optional: true,
			Phase:    "External Accounts",
		},
		{
			HumanLabel:     "Google Client Secret (optional)",
			SSMCategoryKey: "auth/google_secret",
			ParamType:      ParamSecureString,
			Source:         SourcePrompt,
			Prompt:         `Paste Google Client Secret (or press Enter to skip):`,
			ValidateFn: func(ctx context.Context, input string) ValidationResult {
				return v.ValidateRegex(ctx, input, `^.{10,}$`, "Google Client Secret")
			},
			IsSecret: true,
			Optional: true,
			Phase:    "External Accounts",
		},
		{
			HumanLabel:     "GitHub Client ID (optional)",
			SSMCategoryKey: "auth/github_client_id",
			ParamType:      ParamString,
			Source:         SourcePrompt,
			Prompt:         `Paste GitHub Client ID (or press Enter to skip):`,
			ValidateFn: func(ctx context.Context, input string) ValidationResult {
				return v.ValidateRegex(ctx, input, `^.{10,}$`, "GitHub Client ID")
			},
			IsSecret: false,
			Optional: true,
			Phase:    "External Accounts",
		},
		{
			HumanLabel:     "GitHub Client Secret (optional)",
			SSMCategoryKey: "auth/github_secret",
			ParamType:      ParamSecureString,
			Source:         SourcePrompt,
			Prompt:         `Paste GitHub Client Secret (or press Enter to skip):`,
			ValidateFn: func(ctx context.Context, input string) ValidationResult {
				return v.ValidateRegex(ctx, input, `^.{10,}$`, "GitHub Client Secret")
			},
			IsSecret: true,
			Optional: true,
			Phase:    "External Accounts",
		},

		// -----------------------------------------------------------------
		// Phase 2: SSM Injection - Auto-generated secrets (Section 4)
		// -----------------------------------------------------------------
		{
			HumanLabel:     "Session Key",
			SSMCategoryKey: "auth/session_key",
			ParamType:      ParamSecureString,
			Source:         SourceGenerated,
			Phase:          "Internal Secrets",
		},
		{
			HumanLabel:     "Admin API Key",
			SSMCategoryKey: "security/admin_api_key",
			ParamType:      ParamSecureString,
			Source:         SourceGenerated,
			Phase:          "Internal Secrets",
		},

		// -----------------------------------------------------------------
		// RunPod Endpoint ID - initially "pending_setup" (Section 6.3, 6.4)
		// -----------------------------------------------------------------
		{
			HumanLabel:     "RunPod Endpoint ID",
			SSMCategoryKey: "forecast/runpod_endpoint_id",
			ParamType:      ParamString,
			Source:         SourceFixed,
			FixedValue:     "pending_setup",
			Phase:          "Infrastructure Placeholders",
		},
	}
}

// BootstrapRunner orchestrates the main bootstrap loop. It is separated from
// main() to allow testing with injected dependencies.
type BootstrapRunner struct {
	SSM       *SSMManager
	Validator *Validator
	Stdin     io.Reader
	Stderr    io.Writer

	// SkipOptional causes all steps marked Optional to be auto-skipped
	// without prompting. Controlled by the --skip-oauth flag.
	SkipOptional bool

	// scanner is the shared line scanner for reading stdin throughout the
	// bootstrap session. It is lazily initialized on first use. Using a
	// single scanner avoids the problem where multiple bufio.Scanner instances
	// consume ahead and lose data from the underlying reader.
	scanner *bufio.Scanner

	// inventoryOverride allows tests to inject a modified inventory
	// (e.g., with simplified validators). If nil, BuildInventory is used.
	inventoryOverride []BootstrapStep
}

// NewBootstrapRunner creates a BootstrapRunner with production dependencies.
func NewBootstrapRunner(bctx *BootstrapContext) *BootstrapRunner {
	return &BootstrapRunner{
		SSM:       NewSSMManager(bctx),
		Validator: NewValidator(),
		Stdin:     os.Stdin,
		Stderr:    os.Stderr,
	}
}

// Run executes the full bootstrap protocol. It iterates through the ordered
// inventory, checking SSM for existing values, prompting for input, validating,
// and writing to SSM.
//
// The function prints phase headers as it transitions between groups of steps,
// and provides a final summary of all actions taken.
//
// Architecture reference: 13-human-setup.md Sections 3-4
func (r *BootstrapRunner) Run(ctx context.Context) error {
	inventory := r.inventoryOverride
	if inventory == nil {
		inventory = BuildInventory(r.Validator)
	}

	var currentPhase string
	var results []stepResult

	for i, step := range inventory {
		// Print phase header when transitioning to a new phase.
		if step.Phase != currentPhase {
			currentPhase = step.Phase
			r.printPhaseHeader(currentPhase)
		}

		fmt.Fprintf(r.Stderr, "\n[%d/%d] %s\n", i+1, len(inventory), step.HumanLabel)

		result, err := r.processStep(ctx, step)
		if err != nil {
			return fmt.Errorf("step %q failed: %w", step.HumanLabel, err)
		}
		results = append(results, result)
	}

	// Print summary.
	r.printSummary(results)
	return nil
}

// stepResult records the outcome of processing a single bootstrap step.
type stepResult struct {
	Label  string
	Action string // "written", "skipped", "overwritten", "generated"
	Path   string
}

// processStep handles a single BootstrapStep: checks existence, obtains value,
// validates, and writes to SSM.
func (r *BootstrapRunner) processStep(ctx context.Context, step BootstrapStep) (stepResult, error) {
	path := r.SSM.SSMPath(step.SSMCategoryKey)

	result := stepResult{
		Label: step.HumanLabel,
		Path:  path,
	}

	// Auto-skip optional steps when --skip-oauth is set.
	if step.Optional && r.SkipOptional {
		fmt.Fprintf(r.Stderr, "  Skipped (--skip-oauth)\n")
		result.Action = "skipped"
		return result, nil
	}

	// Check if parameter already exists (idempotency per Section 1).
	exists, err := r.SSM.ParameterExists(ctx, path)
	if err != nil {
		return result, fmt.Errorf("checking existence of %s: %w", path, err)
	}

	if exists {
		fmt.Fprintf(r.Stderr, "  Parameter already exists: %s\n", path)

		choice, err := r.promptSkipOrOverwrite()
		if err != nil {
			return result, fmt.Errorf("reading skip/overwrite choice: %w", err)
		}

		if choice == "skip" {
			fmt.Fprintf(r.Stderr, "  Skipped.\n")
			result.Action = "skipped"
			return result, nil
		}
		// choice == "overwrite": continue to obtain and write the value.
	}

	overwrite := exists // only set overwrite=true if we're replacing an existing value

	// Obtain the value based on the source type.
	var value string
	switch step.Source {
	case SourcePrompt:
		value, err = r.promptAndValidate(ctx, step)
		if errors.Is(err, errSkipped) {
			fmt.Fprintf(r.Stderr, "  Skipped.\n")
			result.Action = "skipped"
			return result, nil
		}
		if err != nil {
			return result, err
		}

	case SourceGenerated:
		value, err = GenerateSecureToken()
		if err != nil {
			return result, fmt.Errorf("generating token for %s: %w", step.HumanLabel, err)
		}
		fmt.Fprintf(r.Stderr, "  Auto-generated (%d chars)\n", len(value))

	case SourceFixed:
		value = step.FixedValue
		fmt.Fprintf(r.Stderr, "  Using fixed value: %s\n", value)
	}

	// Write to SSM.
	if step.ParamType == ParamSecureString {
		err = r.SSM.PutSecret(ctx, path, value, overwrite)
	} else {
		// PutString always uses overwrite=true internally.
		err = r.SSM.PutString(ctx, path, value)
	}
	if err != nil {
		return result, fmt.Errorf("writing SSM parameter %s: %w", path, err)
	}

	if overwrite {
		result.Action = "overwritten"
	} else if step.Source == SourceGenerated {
		result.Action = "generated"
	} else {
		result.Action = "written"
	}

	fmt.Fprintf(r.Stderr, "  Stored: %s\n", path)
	return result, nil
}

// promptAndValidate prompts the operator for input, validates it, and retries
// up to maxRetries times on validation failure. Secret inputs are masked.
func (r *BootstrapRunner) promptAndValidate(ctx context.Context, step BootstrapStep) (string, error) {
	fmt.Fprintf(r.Stderr, "\n  %s\n\n", step.Prompt)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		var input string
		var err error

		if step.IsSecret {
			input, err = r.readSecretInput("  > ")
		} else {
			input, err = r.readInput("  > ")
		}
		if err != nil {
			return "", fmt.Errorf("reading input for %s: %w", step.HumanLabel, err)
		}

		input = strings.TrimSpace(input)
		if input == "" {
			// Optional steps skip immediately on empty input without confirmation.
			if step.Optional {
				return "", errSkipped
			}
			choice, choiceErr := r.promptSkipOrRetry()
			if choiceErr != nil {
				return "", fmt.Errorf("reading skip/retry choice for %s: %w", step.HumanLabel, choiceErr)
			}
			if choice == "skip" {
				return "", errSkipped
			}
			// choice == "retry": re-prompt without consuming an attempt.
			attempt--
			continue
		}

		// If we have secret input, acknowledge receipt with length only
		// (per Agent Directive: NEVER echo secrets).
		if step.IsSecret {
			fmt.Fprintf(r.Stderr, "  Received %d chars.\n", len(input))
		}

		// Run validation if a validator is provided.
		if step.ValidateFn != nil {
			vr := step.ValidateFn(ctx, input)
			if !vr.Valid {
				fmt.Fprintf(r.Stderr, "  Validation failed: %s\n", vr.Message)
				if attempt < maxRetries {
					fmt.Fprintf(r.Stderr, "  Try again (%d/%d).\n", attempt, maxRetries)
				}
				continue
			}
			fmt.Fprintf(r.Stderr, "  Validated: %s\n", vr.Message)
		}

		return input, nil
	}

	return "", fmt.Errorf("maximum retries (%d) exceeded for %s", maxRetries, step.HumanLabel)
}

// getScanner returns the shared line scanner, initializing it on first use.
func (r *BootstrapRunner) getScanner() *bufio.Scanner {
	if r.scanner == nil {
		r.scanner = bufio.NewScanner(r.Stdin)
	}
	return r.scanner
}

// scanLine reads a single line from the shared scanner. Returns io.EOF
// when input is exhausted.
func (r *BootstrapRunner) scanLine() (string, error) {
	s := r.getScanner()
	if !s.Scan() {
		if err := s.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return s.Text(), nil
}

// readInput reads a line of plaintext input from stdin.
func (r *BootstrapRunner) readInput(prompt string) (string, error) {
	fmt.Fprint(r.Stderr, prompt)
	return r.scanLine()
}

// readSecretInput reads input without echoing it to the terminal.
// If stdin is a terminal, it uses golang.org/x/term to disable echo.
// If stdin is not a terminal (e.g., piped input), it falls back to
// regular line reading.
func (r *BootstrapRunner) readSecretInput(prompt string) (string, error) {
	fmt.Fprint(r.Stderr, prompt)

	// Check if stdin is a real terminal for secure reading.
	if f, ok := r.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		password, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(r.Stderr) // newline after hidden input
		if err != nil {
			return "", fmt.Errorf("reading secret input: %w", err)
		}
		return string(password), nil
	}

	// Fallback for non-terminal input (testing, piped input).
	return r.scanLine()
}

// promptSkipOrOverwrite asks the operator whether to skip or overwrite
// an existing SSM parameter. Returns "skip" or "overwrite".
func (r *BootstrapRunner) promptSkipOrOverwrite() (string, error) {
	for {
		fmt.Fprint(r.Stderr, "  [S]kip or [O]verwrite? ")

		line, err := r.scanLine()
		if err != nil {
			return "", err
		}

		choice := strings.TrimSpace(strings.ToLower(line))
		switch choice {
		case "s", "skip":
			return "skip", nil
		case "o", "overwrite":
			return "overwrite", nil
		default:
			fmt.Fprintf(r.Stderr, "  Please enter 'S' to skip or 'O' to overwrite.\n")
		}
	}
}

// promptSkipOrRetry asks the operator whether to skip the current parameter
// or retry entering a value. This is invoked when the operator provides
// empty input for a prompted parameter, allowing them to skip optional
// parameters (e.g., OAuth credentials not needed for API-key-only access).
// Returns "skip" or "retry".
func (r *BootstrapRunner) promptSkipOrRetry() (string, error) {
	for {
		fmt.Fprint(r.Stderr, "  No input received. [S]kip this parameter or [R]etry? ")

		line, err := r.scanLine()
		if err != nil {
			return "", err
		}

		choice := strings.TrimSpace(strings.ToLower(line))
		switch choice {
		case "s", "skip":
			return "skip", nil
		case "r", "retry":
			return "retry", nil
		default:
			fmt.Fprintf(r.Stderr, "  Please enter 'S' to skip or 'R' to retry.\n")
		}
	}
}

// printPhaseHeader displays a section header for a group of related steps.
func (r *BootstrapRunner) printPhaseHeader(phase string) {
	fmt.Fprintf(r.Stderr, "\n============================================================\n")
	fmt.Fprintf(r.Stderr, "  Phase: %s\n", phase)
	fmt.Fprintf(r.Stderr, "============================================================\n")
}

// printSummary displays a table of all actions taken during the bootstrap run.
func (r *BootstrapRunner) printSummary(results []stepResult) {
	fmt.Fprintf(r.Stderr, "\n")
	fmt.Fprintf(r.Stderr, "============================================================\n")
	fmt.Fprintf(r.Stderr, "  Bootstrap Summary\n")
	fmt.Fprintf(r.Stderr, "============================================================\n")

	written := 0
	skipped := 0
	generated := 0
	overwritten := 0

	for _, res := range results {
		status := ""
		switch res.Action {
		case "written":
			status = "[WRITTEN]"
			written++
		case "skipped":
			status = "[SKIPPED]"
			skipped++
		case "generated":
			status = "[GENERATED]"
			generated++
		case "overwritten":
			status = "[OVERWRITTEN]"
			overwritten++
		}
		fmt.Fprintf(r.Stderr, "  %-12s %s\n", status, res.Label)
	}

	fmt.Fprintf(r.Stderr, "------------------------------------------------------------\n")
	fmt.Fprintf(r.Stderr, "  Total: %d parameters\n", len(results))
	fmt.Fprintf(r.Stderr, "  Written: %d | Generated: %d | Overwritten: %d | Skipped: %d\n",
		written, generated, overwritten, skipped)
	fmt.Fprintf(r.Stderr, "============================================================\n")
	fmt.Fprintf(r.Stderr, "\n")
	fmt.Fprintf(r.Stderr, "  Next step: Deploy infrastructure with SAM.\n")
	fmt.Fprintf(r.Stderr, "  Run: sam build && sam deploy --guided\n")
	fmt.Fprintf(r.Stderr, "\n")
}

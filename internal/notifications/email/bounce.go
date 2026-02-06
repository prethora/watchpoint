package email

import (
	"context"
	"fmt"
	"time"

	"watchpoint/internal/external"
	"watchpoint/internal/types"
)

// FeedbackType classifies the kind of asynchronous email feedback received
// from the provider (e.g., SendGrid bounce webhook).
//
// Architecture reference: 08b-email-worker.md Section 6.1
type FeedbackType string

const (
	// FeedbackBounce indicates a hard bounce (undeliverable address).
	FeedbackBounce FeedbackType = "bounce"

	// FeedbackComplaint indicates a spam complaint from the recipient.
	FeedbackComplaint FeedbackType = "complaint"
)

// BounceEvent normalizes provider-specific bounce/complaint payloads into a
// common structure for processing by the BounceProcessor.
//
// Architecture reference: 08b-email-worker.md Section 6.1
type BounceEvent struct {
	// ProviderMessageID is the provider's message identifier (e.g., SendGrid message ID).
	// Used to correlate the bounce with the original delivery record.
	ProviderMessageID string

	// EmailAddress is the recipient address that bounced or filed a complaint.
	EmailAddress string

	// Reason is the human-readable bounce reason from the provider.
	Reason string

	// Type classifies the feedback as a bounce or spam complaint.
	Type FeedbackType

	// Timestamp is when the provider recorded the event.
	Timestamp time.Time
}

// ChannelHealthRepository abstracts the database operations needed to track
// and manage channel health. The implementation performs optimistic-locking
// updates on the watchpoints JSONB channels array.
//
// Architecture reference: 08a-notification-core.md Section 7.3
type ChannelHealthRepository interface {
	// IncrementChannelFailure atomically increments the failure_count for a
	// channel identified by WatchPoint ID and channel index within the channels
	// JSONB array. Returns the new failure count after increment.
	//
	// Implementation uses optimistic locking:
	//   1. Fetch WatchPoint with config_version
	//   2. Modify channel in memory (increment failure_count, set last_failed_at)
	//   3. UPDATE with WHERE config_version = $version
	//   4. If 0 rows affected (concurrent modification), refetch and retry
	IncrementChannelFailure(ctx context.Context, wpID string, channelIdx int) (newCount int, err error)

	// DisableChannel sets enabled=false and disabled_reason on a channel within
	// the watchpoints JSONB channels array. Uses the same optimistic locking
	// strategy as IncrementChannelFailure.
	DisableChannel(ctx context.Context, wpID string, channelIdx int, reason string) error

	// ResetChannelFailureCount resets failure_count to 0 for a channel.
	// Used by the Lazy Reset Strategy on successful delivery.
	ResetChannelFailureCount(ctx context.Context, wpID string, channelID string) error
}

// DeliveryLookup abstracts the operation of finding a delivery record by its
// provider-assigned message ID and updating its status. This is a narrow
// interface extracted from DeliveryManager to keep BounceProcessor dependencies
// minimal and testable.
type DeliveryLookup interface {
	// FindDeliveryByProviderMsgID locates a delivery record using the provider's
	// message identifier. Returns the delivery ID, notification ID, watchpoint ID,
	// organization ID, and channel index. Returns an error if not found.
	FindDeliveryByProviderMsgID(ctx context.Context, providerMsgID string) (*DeliveryInfo, error)

	// UpdateDeliveryBounced marks a delivery record as bounced with the given reason.
	UpdateDeliveryBounced(ctx context.Context, deliveryID string, reason string) error
}

// DeliveryInfo holds the metadata extracted from a delivery record needed
// by the BounceProcessor to correlate bounces with WatchPoints and channels.
type DeliveryInfo struct {
	DeliveryID     string
	NotificationID string
	WatchPointID   string
	OrganizationID string
	ChannelIndex   int
}

// UserEmailLookup abstracts the operation of looking up an organization
// owner's email address. Extracted from db.UserRepository for minimal coupling.
type UserEmailLookup interface {
	// GetOwnerEmail returns the email address of the owner-role user for the
	// given organization. Used for system-level alerts.
	GetOwnerEmail(ctx context.Context, orgID string) (string, error)
}

// BounceProcessor handles asynchronous email feedback (bounces and complaints)
// from the email provider. It coordinates delivery status updates, channel
// health tracking, and owner notification when channels are disabled.
//
// Architecture reference: 08b-email-worker.md Section 6.2
// Flow coverage: NOTIF-006 (Bounce Handling), USER-014 (Notify Owner)
type BounceProcessor struct {
	deliveryLookup DeliveryLookup
	healthRepo     ChannelHealthRepository
	userLookup     UserEmailLookup
	emailProvider  external.EmailProvider
	templates      TemplateService
	logger         types.Logger
}

// BounceProcessorConfig holds the dependencies for creating a BounceProcessor.
type BounceProcessorConfig struct {
	DeliveryLookup DeliveryLookup
	HealthRepo     ChannelHealthRepository
	UserLookup     UserEmailLookup
	EmailProvider  external.EmailProvider
	Templates      TemplateService
	Logger         types.Logger
}

// NewBounceProcessor creates a new BounceProcessor with the given dependencies.
func NewBounceProcessor(cfg BounceProcessorConfig) *BounceProcessor {
	return &BounceProcessor{
		deliveryLookup: cfg.DeliveryLookup,
		healthRepo:     cfg.HealthRepo,
		userLookup:     cfg.UserLookup,
		emailProvider:  cfg.EmailProvider,
		templates:      cfg.Templates,
		logger:         cfg.Logger,
	}
}

// maxBounceFailures is the threshold at which a channel is automatically
// disabled due to repeated hard bounces.
const maxBounceFailures = 3

// Process handles a single BounceEvent. The processing logic follows the
// architecture specification in 08b-email-worker.md Section 6.2:
//
//  1. Update Delivery Status: Find the delivery by ProviderMessageID and mark
//     it as 'bounced' with the bounce reason.
//
//  2. Manage Channel Health:
//     - If Type == Complaint (spam report): IMMEDIATELY disable the channel.
//     - If Type == Bounce (hard bounce): Increment failure count. If count >= 3,
//     disable the channel.
//
//  3. Notify Owner: If a channel was disabled, look up the organization owner's
//     email and send a system alert (USER-014 flow).
func (b *BounceProcessor) Process(ctx context.Context, event BounceEvent) error {
	b.logger.Info("processing bounce event",
		"provider_message_id", event.ProviderMessageID,
		"email", RedactEmail(event.EmailAddress),
		"type", string(event.Type),
		"reason", event.Reason,
	)

	// Step 1: Update Delivery Status.
	info, err := b.deliveryLookup.FindDeliveryByProviderMsgID(ctx, event.ProviderMessageID)
	if err != nil {
		return fmt.Errorf("bounce processor: find delivery: %w", err)
	}

	if err := b.deliveryLookup.UpdateDeliveryBounced(ctx, info.DeliveryID, event.Reason); err != nil {
		return fmt.Errorf("bounce processor: update delivery status: %w", err)
	}

	// Step 2: Manage Channel Health.
	var channelDisabled bool

	switch event.Type {
	case FeedbackComplaint:
		// Spam complaints immediately disable the channel.
		b.logger.Warn("spam complaint received, disabling channel immediately",
			"watchpoint_id", info.WatchPointID,
			"channel_index", info.ChannelIndex,
			"email", RedactEmail(event.EmailAddress),
		)
		if err := b.healthRepo.DisableChannel(ctx, info.WatchPointID, info.ChannelIndex, "spam_complaint"); err != nil {
			return fmt.Errorf("bounce processor: disable channel on complaint: %w", err)
		}
		channelDisabled = true

	case FeedbackBounce:
		// Hard bounces increment the failure count.
		newCount, err := b.healthRepo.IncrementChannelFailure(ctx, info.WatchPointID, info.ChannelIndex)
		if err != nil {
			return fmt.Errorf("bounce processor: increment failure count: %w", err)
		}

		b.logger.Info("channel failure count incremented",
			"watchpoint_id", info.WatchPointID,
			"channel_index", info.ChannelIndex,
			"new_count", newCount,
		)

		// If the failure count has reached the threshold, disable the channel.
		if newCount >= maxBounceFailures {
			b.logger.Warn("channel failure threshold reached, disabling channel",
				"watchpoint_id", info.WatchPointID,
				"channel_index", info.ChannelIndex,
				"failure_count", newCount,
			)
			if err := b.healthRepo.DisableChannel(ctx, info.WatchPointID, info.ChannelIndex, "hard_bounce"); err != nil {
				return fmt.Errorf("bounce processor: disable channel on bounce threshold: %w", err)
			}
			channelDisabled = true
		}

	default:
		b.logger.Warn("unknown feedback type, treating as bounce",
			"type", string(event.Type),
			"provider_message_id", event.ProviderMessageID,
		)
	}

	// Step 3: Notify Owner if channel was disabled (USER-014 flow).
	if channelDisabled {
		if err := b.notifyOwner(ctx, info.OrganizationID, event.EmailAddress); err != nil {
			// Log but do not fail the overall bounce processing.
			// The channel is already disabled; the owner notification is best-effort.
			b.logger.Error("failed to notify owner of channel disablement",
				"organization_id", info.OrganizationID,
				"error", err.Error(),
			)
		}
	}

	return nil
}

// notifyOwner looks up the organization owner's email and sends a system alert
// about a disabled email channel.
//
// Architecture reference: USER-014 flow simulation steps 2-5.
func (b *BounceProcessor) notifyOwner(ctx context.Context, orgID string, bouncedAddress string) error {
	// Step 2 (USER-014): Look up the owner's email.
	ownerEmail, err := b.userLookup.GetOwnerEmail(ctx, orgID)
	if err != nil {
		return fmt.Errorf("notify owner: get owner email: %w", err)
	}

	// Step 3 (USER-014): Resolve the system alert template.
	tmplID, err := b.templates.Resolve("system", types.EventSystemAlert)
	if err != nil {
		return fmt.Errorf("notify owner: resolve system alert template: %w", err)
	}

	// Step 4 (USER-014): Send the alert email to the owner.
	_, err = b.emailProvider.Send(ctx, types.SendInput{
		To: ownerEmail,
		From: types.SenderIdentity{
			Name:    "WatchPoint System",
			Address: "system@watchpoint.io",
		},
		TemplateID: tmplID,
		TemplateData: map[string]interface{}{
			"bounced_address":   bouncedAddress,
			"organization_id":   orgID,
			"event_type":        string(types.EventSystemAlert),
			"disabled_reason":   "Email channel disabled due to repeated delivery failures",
		},
	})
	if err != nil {
		return fmt.Errorf("notify owner: send system alert: %w", err)
	}

	b.logger.Info("owner notified of channel disablement",
		"organization_id", orgID,
		"owner_email", RedactEmail(ownerEmail),
	)

	return nil
}

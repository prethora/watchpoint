// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the DigestScheduler service from
// architecture/09-scheduled-jobs.md Section 7.1. It triggers daily digest
// generation for organizations whose next_digest_at has passed, enqueues a
// "monitor_digest" notification message to SQS, and calculates the next
// timezone-aware run time based on the organization's notification preferences.
//
// The scheduler is invoked hourly by the Archiver multiplexer and processes
// organizations in batches to prevent Lambda timeouts.
//
// Flow: NOTIF-003
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// DigestBatchLimit is the maximum number of organizations processed per
// invocation to prevent Lambda timeouts. Matches the flow simulation which
// uses LIMIT 100 in the query.
const DigestBatchLimit = 100

// DefaultDigestDeliveryTime is the fallback delivery time if not configured
// in the organization's notification preferences. Format: "HH:MM" in 24h.
const DefaultDigestDeliveryTime = "08:00"

// DefaultDigestTimezone is the fallback timezone when the organization has
// no timezone configured in their notification preferences.
const DefaultDigestTimezone = "UTC"

// DigestOrgRow represents a single row from the digest scheduling query.
// This is a local type scoped to the scheduler package; it holds only the
// fields needed for digest scheduling logic.
type DigestOrgRow struct {
	ID                      string
	NotificationPreferences types.NotificationPreferences
	Timezone                string // Resolved from notification_preferences.quiet_hours.timezone
}

// DigestSchedulerDB defines the database operations needed by the DigestScheduler.
// Using an interface allows clean testing without database dependencies.
type DigestSchedulerDB interface {
	// ListDueDigestOrgs returns organizations where next_digest_at <= now.
	//
	// SQL: SELECT id, notification_preferences
	//      FROM organizations
	//      WHERE next_digest_at <= $1 AND deleted_at IS NULL
	//      LIMIT $2
	//
	// The timezone is extracted from the notification_preferences JSONB at the
	// application layer since it is nested within the preferences structure.
	ListDueDigestOrgs(ctx context.Context, now time.Time, limit int) ([]DigestOrgRow, error)

	// UpdateNextDigestAt sets the next_digest_at for the given organization.
	//
	// SQL: UPDATE organizations SET next_digest_at = $1, updated_at = NOW()
	//      WHERE id = $2
	UpdateNextDigestAt(ctx context.Context, orgID string, nextDigestAt time.Time) error
}

// DigestSQSPublisher abstracts the SQS message publishing for digest notifications.
// The digest scheduler enqueues messages to the notification queue.
type DigestSQSPublisher interface {
	// PublishDigest sends a digest trigger message to the notification SQS queue.
	// The message body is a JSON-serialized DigestMessage.
	PublishDigest(ctx context.Context, msg DigestMessage) error
}

// DigestMessage represents the SQS message payload sent to the notification
// queue to trigger digest generation. It contains the minimum information
// the notification worker needs to generate and deliver the digest.
type DigestMessage struct {
	OrganizationID string          `json:"organization_id"`
	EventType      types.EventType `json:"event_type"`
	TriggeredAt    time.Time       `json:"triggered_at"`
}

// digestScheduler implements the DigestScheduler interface from
// architecture/09-scheduled-jobs.md Section 7.1.
type digestScheduler struct {
	db        DigestSchedulerDB
	publisher DigestSQSPublisher
	logger    *slog.Logger
}

// NewDigestScheduler creates a new DigestScheduler service.
func NewDigestScheduler(db DigestSchedulerDB, publisher DigestSQSPublisher, logger *slog.Logger) *digestScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &digestScheduler{
		db:        db,
		publisher: publisher,
		logger:    logger,
	}
}

// TriggerDigests implements the DigestScheduler interface.
//
// It queries organizations due for digest generation (next_digest_at <= currentUTC),
// enqueues a "monitor_digest" message to the notification queue for each, and
// calculates the next timezone-aware run time based on the organization's
// notification preferences.
//
// Returns the number of organizations for which digests were triggered.
//
// Flow: NOTIF-003
func (d *digestScheduler) TriggerDigests(ctx context.Context, currentUTC time.Time) (int, error) {
	totalTriggered := 0

	for {
		orgs, err := d.db.ListDueDigestOrgs(ctx, currentUTC, DigestBatchLimit)
		if err != nil {
			return totalTriggered, fmt.Errorf("listing due digest orgs: %w", err)
		}

		if len(orgs) == 0 {
			break
		}

		d.logger.InfoContext(ctx, "processing digest batch",
			"batch_size", len(orgs),
			"total_so_far", totalTriggered,
		)

		batchSuccesses := 0
		for _, org := range orgs {
			if err := d.processOrg(ctx, org, currentUTC); err != nil {
				d.logger.ErrorContext(ctx, "failed to process digest for org",
					"org_id", org.ID,
					"error", err,
				)
				// Continue processing other orgs; don't let one failure block all.
				// The failed org will be retried on the next hourly run since its
				// next_digest_at remains in the past.
				continue
			}
			totalTriggered++
			batchSuccesses++
		}

		// If we got fewer than the batch limit, we've processed all due orgs.
		if len(orgs) < DigestBatchLimit {
			break
		}

		// Safety: if no orgs in this batch were successfully processed, break
		// to prevent an infinite loop. Failed orgs remain with next_digest_at
		// in the past and will be retried on the next scheduled invocation.
		if batchSuccesses == 0 {
			d.logger.WarnContext(ctx, "no progress in digest batch, breaking to prevent infinite loop",
				"batch_size", len(orgs),
			)
			break
		}
	}

	d.logger.InfoContext(ctx, "digest trigger cycle complete",
		"total_triggered", totalTriggered,
	)

	return totalTriggered, nil
}

// processOrg handles a single organization's digest: enqueue the SQS message,
// calculate the next run time, and update the database.
func (d *digestScheduler) processOrg(ctx context.Context, org DigestOrgRow, now time.Time) error {
	// Step 1: Enqueue "Generate Digest" message to the Notification Queue.
	msg := DigestMessage{
		OrganizationID: org.ID,
		EventType:      types.EventDigest,
		TriggeredAt:    now,
	}

	if err := d.publisher.PublishDigest(ctx, msg); err != nil {
		return fmt.Errorf("publishing digest message for org %s: %w", org.ID, err)
	}

	// Step 2: Calculate the next digest run time.
	nextRun, err := CalculateNextDigestRun(now, org.NotificationPreferences, org.Timezone)
	if err != nil {
		// If we can't calculate the next run time (bad timezone, etc.), log the
		// error but still update to a safe fallback (next day at 08:00 UTC).
		d.logger.ErrorContext(ctx, "failed to calculate next digest run, using UTC fallback",
			"org_id", org.ID,
			"timezone", org.Timezone,
			"error", err,
		)
		nextRun = computeNextDayAtTime(now, 8, 0, time.UTC)
	}

	// Step 3: Update next_digest_at.
	if err := d.db.UpdateNextDigestAt(ctx, org.ID, nextRun); err != nil {
		return fmt.Errorf("updating next_digest_at for org %s: %w", org.ID, err)
	}

	d.logger.InfoContext(ctx, "digest triggered for org",
		"org_id", org.ID,
		"next_digest_at", nextRun.Format(time.RFC3339),
	)

	return nil
}

// CalculateNextDigestRun computes the next UTC time at which a digest should be
// triggered for an organization, based on its notification preferences and timezone.
//
// The logic:
//  1. Parse the organization's timezone.
//  2. Read the delivery_time from the digest config (defaults to "08:00").
//  3. Convert 'now' to the org's local timezone.
//  4. Find the next occurrence of the delivery_time in the local timezone.
//  5. Handle the digest frequency (currently only "daily" is supported).
//  6. Convert back to UTC for storage.
//
// This function is exported to allow direct unit testing of the calculation logic.
func CalculateNextDigestRun(now time.Time, prefs types.NotificationPreferences, tz string) (time.Time, error) {
	// Resolve timezone.
	if tz == "" {
		tz = DefaultDigestTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}

	// Resolve delivery time from preferences.
	deliveryHour, deliveryMin := 8, 0 // Default: 08:00
	if prefs.Digest != nil && prefs.Digest.DeliveryTime != "" {
		h, m, parseErr := parseTimeOfDay(prefs.Digest.DeliveryTime)
		if parseErr != nil {
			return time.Time{}, fmt.Errorf("invalid delivery_time %q: %w", prefs.Digest.DeliveryTime, parseErr)
		}
		deliveryHour, deliveryMin = h, m
	}

	// Resolve frequency. Default is daily.
	frequency := "daily"
	if prefs.Digest != nil && prefs.Digest.Frequency != "" {
		frequency = prefs.Digest.Frequency
	}

	// Convert now to local time.
	localNow := now.In(loc)

	// Calculate the next occurrence in local time.
	var nextLocal time.Time
	switch frequency {
	case "daily":
		nextLocal = computeNextDayAtTime(localNow, deliveryHour, deliveryMin, loc)
	case "weekly":
		// Weekly digests run on the same day of the week as the current local day,
		// or the next occurrence 7 days from the next daily time.
		candidate := computeNextDayAtTime(localNow, deliveryHour, deliveryMin, loc)
		// If the candidate is only 1 day ahead (the normal daily case), add 6 more days
		// to get a full week interval. If the candidate is the same day (shouldn't happen
		// since computeNextDayAtTime advances), handle that too.
		nextLocal = candidate.AddDate(0, 0, 6)
	default:
		// Unknown frequency; fall back to daily.
		nextLocal = computeNextDayAtTime(localNow, deliveryHour, deliveryMin, loc)
	}

	// Convert back to UTC.
	return nextLocal.UTC(), nil
}

// computeNextDayAtTime returns the next occurrence of the given hour:minute in
// the specified location after the given time. If the target time has already
// passed today, it returns tomorrow at that time.
//
// This correctly handles DST transitions because time.Date in a specific
// Location will automatically adjust for DST.
func computeNextDayAtTime(now time.Time, hour, minute int, loc *time.Location) time.Time {
	// Construct today's target time in the specified location.
	today := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)

	if today.After(now) {
		return today
	}

	// Target time has passed today; advance to tomorrow.
	tomorrow := today.AddDate(0, 0, 1)
	return tomorrow
}

// parseTimeOfDay parses a "HH:MM" string into hour and minute components.
// The input must be exactly in HH:MM format (5 characters). Trailing content
// is rejected to prevent ambiguity.
func parseTimeOfDay(s string) (int, int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("expected format HH:MM, got %q", s)
	}
	var hour, minute int
	n, err := fmt.Sscanf(s, "%d:%d", &hour, &minute)
	if err != nil || n != 2 {
		return 0, 0, fmt.Errorf("expected format HH:MM, got %q", s)
	}
	if hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("hour %d out of range [0,23]", hour)
	}
	if minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("minute %d out of range [0,59]", minute)
	}
	return hour, minute, nil
}

// ResolveOrgTimezone extracts the timezone from an organization's notification
// preferences. The timezone is stored in the quiet_hours configuration.
// Returns DefaultDigestTimezone if no timezone is configured.
func ResolveOrgTimezone(prefs types.NotificationPreferences) string {
	if prefs.QuietHours != nil && prefs.QuietHours.Timezone != "" {
		return prefs.QuietHours.Timezone
	}
	return DefaultDigestTimezone
}

// MarshalDigestMessage serializes a DigestMessage to JSON for SQS transport.
func MarshalDigestMessage(msg DigestMessage) ([]byte, error) {
	return json.Marshal(msg)
}

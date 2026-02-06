package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// Compile-time assertion that PolicyEngineImpl implements PolicyEngine.
var _ PolicyEngine = (*PolicyEngineImpl)(nil)

// PolicyEngineImpl is the production implementation of PolicyEngine.
// It evaluates Quiet Hours, urgency overrides, and event type exceptions
// to determine whether a notification should be delivered immediately,
// deferred, or suppressed.
type PolicyEngineImpl struct {
	clock  types.Clock
	logger types.Logger
}

// NewPolicyEngine creates a new PolicyEngineImpl with the given clock and logger.
// The clock abstraction allows deterministic testing of time-dependent logic.
func NewPolicyEngine(clock types.Clock, logger types.Logger) *PolicyEngineImpl {
	return &PolicyEngineImpl{
		clock:  clock,
		logger: logger,
	}
}

// Evaluate checks Quiet Hours, frequency caps, and urgency overrides to
// determine the delivery policy for a notification.
//
// Decision logic (in order of precedence):
//  1. Critical urgency -> always deliver immediately (bypass all suppression)
//  2. threshold_cleared event type -> always deliver immediately (time-sensitive)
//  3. Quiet Hours enabled and currently active -> defer until end of quiet period
//  4. Otherwise -> deliver immediately
//
// Timezone Resolution: The user's local time is derived from the
// QuietHoursConfig.Timezone field in the org's NotificationPreferences,
// NOT from the WatchPoint's location timezone.
func (e *PolicyEngineImpl) Evaluate(ctx context.Context, n *types.Notification, org *types.Organization, prefs types.NotificationPreferences) (PolicyResult, error) {
	// 1. Critical urgency bypasses all suppression.
	if n.Urgency == types.UrgencyCritical {
		return PolicyResult{
			Decision: PolicyDeliverImmediately,
			Reason:   "critical urgency bypasses all suppression",
		}, nil
	}

	// 2. Clearance notifications are time-sensitive and must always be delivered.
	if n.EventType == types.EventThresholdCleared {
		return PolicyResult{
			Decision: PolicyDeliverImmediately,
			Reason:   "threshold_cleared bypasses cooldown and quiet hours",
		}, nil
	}

	// 3. Check Quiet Hours.
	if prefs.QuietHours != nil && prefs.QuietHours.Enabled {
		result, err := e.evaluateQuietHours(prefs.QuietHours)
		if err != nil {
			// Log the error but don't suppress the notification -- fail open.
			e.logger.Error("quiet hours evaluation failed, delivering anyway",
				"error", err.Error(),
				"notification_id", n.ID,
			)
			return PolicyResult{
				Decision: PolicyDeliverImmediately,
				Reason:   "quiet hours evaluation failed, fail-open",
			}, nil
		}
		if result != nil {
			return *result, nil
		}
	}

	// 4. No suppression applies.
	return PolicyResult{
		Decision: PolicyDeliverImmediately,
		Reason:   "no policy restrictions apply",
	}, nil
}

// evaluateQuietHours checks if the current time falls within any configured
// Quiet Hours period. Returns a PolicyResult if in quiet hours (defer), or
// nil if not in quiet hours.
func (e *PolicyEngineImpl) evaluateQuietHours(config *types.QuietHoursConfig) (*PolicyResult, error) {
	if len(config.Schedule) == 0 {
		return nil, nil
	}

	// Load the user's timezone.
	tz := config.Timezone
	if tz == "" {
		tz = "UTC"
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}

	now := e.clock.Now().In(loc)
	currentDay := strings.ToLower(now.Weekday().String())

	for _, period := range config.Schedule {
		if !dayMatches(period.Days, currentDay) {
			continue
		}

		startTime, err := parseTimeOfDay(period.Start)
		if err != nil {
			return nil, fmt.Errorf("invalid quiet hours start %q: %w", period.Start, err)
		}

		endTime, err := parseTimeOfDay(period.End)
		if err != nil {
			return nil, fmt.Errorf("invalid quiet hours end %q: %w", period.End, err)
		}

		inQuiet, resumeAt := isInQuietPeriod(now, startTime, endTime)
		if inQuiet {
			return &PolicyResult{
				Decision: PolicyDefer,
				Reason:   fmt.Sprintf("quiet hours active (%s-%s %s)", period.Start, period.End, tz),
				ResumeAt: &resumeAt,
			}, nil
		}
	}

	return nil, nil
}

// timeOfDay represents a wall-clock time with hour and minute components.
type timeOfDay struct {
	hour   int
	minute int
}

// toMinutes converts a timeOfDay to minutes since midnight for comparison.
func (t timeOfDay) toMinutes() int {
	return t.hour*60 + t.minute
}

// parseTimeOfDay parses a "HH:MM" string into a timeOfDay.
func parseTimeOfDay(s string) (timeOfDay, error) {
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err != nil || n != 2 {
		return timeOfDay{}, fmt.Errorf("expected HH:MM format, got %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return timeOfDay{}, fmt.Errorf("time out of range: %q", s)
	}
	return timeOfDay{hour: h, minute: m}, nil
}

// dayMatches checks if the current day name appears in the day list.
// Day names are compared case-insensitively. An empty days list matches all days.
func dayMatches(days []string, currentDay string) bool {
	if len(days) == 0 {
		// No day restriction means every day.
		return true
	}
	for _, d := range days {
		if strings.EqualFold(d, currentDay) {
			return true
		}
	}
	return false
}

// isInQuietPeriod checks if the given time falls within the quiet period
// defined by start and end times. Handles overnight periods (e.g., 22:00-08:00).
// Returns whether the time is in the quiet period and the resumeAt time
// (when the quiet period ends in the same timezone).
func isInQuietPeriod(now time.Time, start, end timeOfDay) (bool, time.Time) {
	nowMinutes := now.Hour()*60 + now.Minute()
	startMinutes := start.toMinutes()
	endMinutes := end.toMinutes()

	if startMinutes <= endMinutes {
		// Same-day period (e.g., 09:00-17:00)
		if nowMinutes >= startMinutes && nowMinutes < endMinutes {
			resumeAt := time.Date(
				now.Year(), now.Month(), now.Day(),
				end.hour, end.minute, 0, 0, now.Location(),
			)
			return true, resumeAt
		}
	} else {
		// Overnight period (e.g., 22:00-08:00)
		if nowMinutes >= startMinutes || nowMinutes < endMinutes {
			// Determine the resumeAt: if we're past midnight (< endMinutes),
			// resume is today at end time. If we're before midnight (>= startMinutes),
			// resume is tomorrow at end time.
			if nowMinutes >= startMinutes {
				// Before midnight - resume is tomorrow at end time.
				tomorrow := now.AddDate(0, 0, 1)
				resumeAt := time.Date(
					tomorrow.Year(), tomorrow.Month(), tomorrow.Day(),
					end.hour, end.minute, 0, 0, now.Location(),
				)
				return true, resumeAt
			}
			// After midnight - resume is today at end time.
			resumeAt := time.Date(
				now.Year(), now.Month(), now.Day(),
				end.hour, end.minute, 0, 0, now.Location(),
			)
			return true, resumeAt
		}
	}

	return false, time.Time{}
}

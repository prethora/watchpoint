// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file defines the shared types for the maintenance multiplexer from
// architecture/09-scheduled-jobs.md Section 6.1. These types are used by both
// the internal service routing logic and the cmd/archiver Lambda handler.
//
// The MaintenancePayload is the JSON structure sent by EventBridge rules to the
// ArchiverFunction. The TaskType constant determines which service method handles
// the request.
package scheduler

import "time"

// TaskType identifies which maintenance service should handle an EventBridge event.
// Each constant maps to a specific service method in the maintenance multiplexer.
// Defined per architecture/09-scheduled-jobs.md Section 6.1.
type TaskType string

const (
	TaskArchiveWatchPoints     TaskType = "archive_watchpoints"
	TaskRequeueDeferredNotifs  TaskType = "requeue_deferred"
	TaskCleanupSoftDeletes     TaskType = "cleanup_soft_deletes"
	TaskCleanupIdempotencyKeys TaskType = "cleanup_idempotency_keys"
	TaskCleanupSecurityEvents  TaskType = "cleanup_security_events"
	TaskTriggerDigests         TaskType = "trigger_digests"
	TaskSyncStripe             TaskType = "sync_stripe"
	TaskAggregateUsage         TaskType = "aggregate_usage"
	TaskVerification           TaskType = "verification"
	TaskReconcileForecasts     TaskType = "reconcile_forecasts"
	TaskForecastTier           TaskType = "transition_forecasts"
)

// MaintenancePayload is the JSON payload sent by EventBridge to the Archiver
// Lambda function. It identifies the task to execute and optionally overrides
// the reference time for manual invocation or backfilling.
//
// Per architecture/09-scheduled-jobs.md Section 6.1:
//
//	{
//	  "task": "archive_watchpoints",
//	  "reference_time": "2026-02-06T03:00:00Z"  // optional
//	}
type MaintenancePayload struct {
	Task TaskType `json:"task"`
	// ReferenceTime allows manual invocation to specify a different "now" for
	// deterministic execution and backfilling. If nil, time.Now().UTC() is used.
	ReferenceTime *time.Time `json:"reference_time,omitempty"`
}

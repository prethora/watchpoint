# Architectural Suggestion: DigestGenerator Interface Signature Mismatch

## Context
Working on Phase 8 task "Implement Digest Generator". While implementing the DigestGenerator interface from 08a-notification-core.md Section 6.2.

## The Issue
The architecture spec (Section 6.2) defines the DigestGenerator interface with `currentForecast *types.EvaluationResult` as the third parameter:

```go
type DigestGenerator interface {
    Generate(ctx context.Context,
             wp *types.WatchPoint,
             currentForecast *types.EvaluationResult,
             previousDigest *DigestContent,
             digestConfig *types.DigestConfig) (*DigestContent, error)
}
```

However, the flow simulation (NOTIF-003) specifies that the generator reads `last_forecast_summary` (MonitorSummary) and `last_digest_content` (DigestContent) for each WatchPoint. The `EvaluationResult` type does not contain the `MaxValues map[string]float64` or `TriggeredPeriods []TimeRange` fields that the digest comparison logic (NOTIF-003b) requires. These fields exist in `MonitorSummary`.

## Why It Matters
The `EvaluationResult` struct contains:
- Timestamp, Triggered, MatchedConditions, ForecastSnapshot, ModelUsed

The digest comparison logic (NOTIF-003b) needs:
- MaxValues (map of variable name to peak value)
- TriggeredPeriods (time ranges where conditions were met)
- WindowStart/WindowEnd

These are in `MonitorSummary`, not `EvaluationResult`. Using `EvaluationResult` in the interface would require either:
1. Adding MonitorSummary fields to EvaluationResult (incorrect -- they serve different purposes)
2. Passing both EvaluationResult and MonitorSummary (unnecessary complexity)

## Which Architecture Files Are Affected
- `08a-notification-core.md` Section 6.2 (DigestGenerator interface signature)

## Suggested Resolution
Update the interface signature in 08a-notification-core.md Section 6.2 to use `*types.MonitorSummary`:

```go
type DigestGenerator interface {
    Generate(ctx context.Context,
             wp *types.WatchPoint,
             current *types.MonitorSummary,
             previousDigest *DigestContent,
             digestConfig *types.DigestConfig) (*DigestContent, error)
}
```

## Impact on Current Task
Did not prevent completion. I implemented the interface using `*types.MonitorSummary` which matches the actual data flow described in the NOTIF-003 flow simulation. The implementation is fully functional and tested.

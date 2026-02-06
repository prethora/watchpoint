// Package billing provides plan management and billing domain logic.
package billing

import "watchpoint/internal/types"

// PlanRegistry defines the authoritative limits for each tier.
// This is the single source of truth for what each plan allows.
type PlanRegistry interface {
	// GetLimits returns the resource limits for the given plan tier.
	// For unknown tiers, returns the most restrictive (Free) limits
	// to fail safely.
	GetLimits(tier types.PlanTier) types.PlanLimits
}

// staticPlanRegistry is a compile-time plan registry backed by an in-memory map.
// It implements PlanRegistry and is the standard implementation for production use.
type staticPlanRegistry struct {
	limits map[types.PlanTier]types.PlanLimits
}

// planDefaults defines the hardcoded plan limits as specified in the platform design.
// These values come from the Plan Tiers table in watchpoint-platform-design-final.md:
//
//	| Plan       | WatchPoints | API Calls/Day | Nowcast |
//	|------------|-------------|---------------|---------|
//	| Free       | 3           | 0 (None)      | No      |
//	| Starter    | 25          | 100           | Yes     |
//	| Pro        | 100         | 1,000         | Yes     |
//	| Business   | 500         | 10,000        | Yes     |
//	| Enterprise | 0 (unlimited)| 0 (unlimited) | Yes    |
//
// Enterprise uses 0 to represent "unlimited" -- enforcement code must treat 0 as no limit.
var planDefaults = map[types.PlanTier]types.PlanLimits{
	types.PlanFree: {
		MaxWatchPoints:   3,
		MaxAPICallsDaily: 0,
		AllowNowcast:     false,
	},
	types.PlanStarter: {
		MaxWatchPoints:   25,
		MaxAPICallsDaily: 100,
		AllowNowcast:     true,
	},
	types.PlanPro: {
		MaxWatchPoints:   100,
		MaxAPICallsDaily: 1000,
		AllowNowcast:     true,
	},
	types.PlanBusiness: {
		MaxWatchPoints:   500,
		MaxAPICallsDaily: 10000,
		AllowNowcast:     true,
	},
	types.PlanEnterprise: {
		MaxWatchPoints:   0, // Unlimited -- enforcement treats 0 as no limit
		MaxAPICallsDaily: 0, // Unlimited -- enforcement treats 0 as no limit
		AllowNowcast:     true,
	},
}

// freeLimits is cached to avoid map lookups on the fallback path.
var freeLimits = planDefaults[types.PlanFree]

// NewStaticPlanRegistry returns a PlanRegistry backed by the hardcoded plan
// limits from the platform design document. This is the standard production
// implementation; no database or external service is required.
func NewStaticPlanRegistry() PlanRegistry {
	// Copy the defaults into a new map so callers cannot mutate the package-level variable.
	m := make(map[types.PlanTier]types.PlanLimits, len(planDefaults))
	for k, v := range planDefaults {
		m[k] = v
	}
	return &staticPlanRegistry{limits: m}
}

// GetLimits returns the resource limits for the given plan tier.
// If the tier is unknown, it returns the Free tier limits as a safe default.
func (r *staticPlanRegistry) GetLimits(tier types.PlanTier) types.PlanLimits {
	if limits, ok := r.limits[tier]; ok {
		return limits
	}
	return freeLimits
}

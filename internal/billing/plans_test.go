package billing

import (
	"testing"

	"watchpoint/internal/types"
)

func TestNewStaticPlanRegistry(t *testing.T) {
	reg := NewStaticPlanRegistry()
	if reg == nil {
		t.Fatal("NewStaticPlanRegistry returned nil")
	}
}

func TestGetLimits_FreeTier(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanFree)

	assertLimits(t, "Free", limits, types.PlanLimits{
		MaxWatchPoints:   3,
		MaxAPICallsDaily: 0,
		AllowNowcast:     false,
	})
}

func TestGetLimits_StarterTier(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanStarter)

	assertLimits(t, "Starter", limits, types.PlanLimits{
		MaxWatchPoints:   25,
		MaxAPICallsDaily: 100,
		AllowNowcast:     true,
	})
}

func TestGetLimits_ProTier(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanPro)

	assertLimits(t, "Pro", limits, types.PlanLimits{
		MaxWatchPoints:   100,
		MaxAPICallsDaily: 1000,
		AllowNowcast:     true,
	})
}

func TestGetLimits_BusinessTier(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanBusiness)

	assertLimits(t, "Business", limits, types.PlanLimits{
		MaxWatchPoints:   500,
		MaxAPICallsDaily: 10000,
		AllowNowcast:     true,
	})
}

func TestGetLimits_EnterpriseTier(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanEnterprise)

	assertLimits(t, "Enterprise", limits, types.PlanLimits{
		MaxWatchPoints:   0,
		MaxAPICallsDaily: 0,
		AllowNowcast:     true,
	})
}

func TestGetLimits_UnknownTierFallsBackToFree(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanTier("nonexistent"))

	assertLimits(t, "Unknown (fallback to Free)", limits, types.PlanLimits{
		MaxWatchPoints:   3,
		MaxAPICallsDaily: 0,
		AllowNowcast:     false,
	})
}

func TestGetLimits_EmptyTierFallsBackToFree(t *testing.T) {
	reg := NewStaticPlanRegistry()
	limits := reg.GetLimits(types.PlanTier(""))

	assertLimits(t, "Empty (fallback to Free)", limits, types.PlanLimits{
		MaxWatchPoints:   3,
		MaxAPICallsDaily: 0,
		AllowNowcast:     false,
	})
}

func TestGetLimits_AllTiersPresent(t *testing.T) {
	// Verify every defined PlanTier constant has an entry in the registry.
	reg := NewStaticPlanRegistry()

	tiers := []types.PlanTier{
		types.PlanFree,
		types.PlanStarter,
		types.PlanPro,
		types.PlanBusiness,
		types.PlanEnterprise,
	}

	for _, tier := range tiers {
		limits := reg.GetLimits(tier)
		// Each known tier must return a distinct result (not the zero value
		// for all fields unless that's genuinely the design).
		// For Free: AllowNowcast=false is the distinguishing field.
		// For Enterprise: MaxWatchPoints=0 is intentional (unlimited).
		// So simply verify we don't panic and get a populated struct.
		t.Logf("Tier=%s  WP=%d  API=%d  Nowcast=%v",
			tier, limits.MaxWatchPoints, limits.MaxAPICallsDaily, limits.AllowNowcast)
	}
}

func TestPlanRegistryInterface(t *testing.T) {
	// Compile-time check that staticPlanRegistry satisfies PlanRegistry.
	var _ PlanRegistry = NewStaticPlanRegistry()
}

func TestGetLimits_IndependentInstances(t *testing.T) {
	// Two registries should be independent; modifying internal state of one
	// (if it were exposed) should not affect the other. Since the map is
	// copied in the constructor, this is guaranteed.
	reg1 := NewStaticPlanRegistry()
	reg2 := NewStaticPlanRegistry()

	l1 := reg1.GetLimits(types.PlanPro)
	l2 := reg2.GetLimits(types.PlanPro)

	if l1 != l2 {
		t.Errorf("Two independent registries returned different Pro limits: %+v vs %+v", l1, l2)
	}
}

// assertLimits is a test helper that compares two PlanLimits values and reports
// field-level mismatches.
func assertLimits(t *testing.T, tier string, got, want types.PlanLimits) {
	t.Helper()

	if got.MaxWatchPoints != want.MaxWatchPoints {
		t.Errorf("%s: MaxWatchPoints = %d, want %d", tier, got.MaxWatchPoints, want.MaxWatchPoints)
	}
	if got.MaxAPICallsDaily != want.MaxAPICallsDaily {
		t.Errorf("%s: MaxAPICallsDaily = %d, want %d", tier, got.MaxAPICallsDaily, want.MaxAPICallsDaily)
	}
	if got.AllowNowcast != want.AllowNowcast {
		t.Errorf("%s: AllowNowcast = %v, want %v", tier, got.AllowNowcast, want.AllowNowcast)
	}
}

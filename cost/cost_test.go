package cost_test

import (
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/cost"
)

func TestCostCalculation(t *testing.T) {
	t.Parallel()

	pricing := &cost.ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	}

	usage := ryn.Usage{
		InputTokens:  1000000,
		OutputTokens: 1000000,
		TotalTokens:  2000000,
	}

	c := pricing.CalculateCost(usage)
	assertTrue(t, c.TotalCost > 0)
	assertEqual(t, c.InputCost, 5.0)
	assertEqual(t, c.OutputCost, 15.0)
	assertEqual(t, c.Currency, "USD")
}

func TestPricingRegistry(t *testing.T) {
	t.Parallel()

	reg := cost.NewPricingRegistry()
	reg.Set("openai", "gpt-4o", &cost.ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	})
	reg.SetDefault("openai", &cost.ModelPricing{
		InputCostPer1M:  1.00,
		OutputCostPer1M: 3.00,
	})

	// Exact match
	p := reg.Get("openai", "gpt-4o")
	assertTrue(t, p != nil)
	assertEqual(t, p.InputCostPer1M, 5.00)

	// Fallback to default
	p2 := reg.Get("openai", "unknown")
	assertTrue(t, p2 != nil)
	assertEqual(t, p2.InputCostPer1M, 1.00)

	// Not found
	p3 := reg.Get("unknown", "unknown")
	assertTrue(t, p3 == nil)
}

// --- test helpers ---

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

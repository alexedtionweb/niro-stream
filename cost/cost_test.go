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

func TestPricingRegistrySetValidation(t *testing.T) {
	t.Parallel()

	reg := cost.NewPricingRegistry()

	// Empty provider/model/pricing should be no-ops
	reg.Set("", "gpt-4o", &cost.ModelPricing{InputCostPer1M: 5.0})
	reg.Set("openai", "", &cost.ModelPricing{InputCostPer1M: 5.0})
	reg.Set("openai", "gpt-4o", nil)
	reg.SetDefault("", &cost.ModelPricing{InputCostPer1M: 5.0})
	reg.SetDefault("openai", nil)

	// Nothing should have been registered
	assertTrue(t, reg.Get("openai", "gpt-4o") == nil)
}

func TestPricingRegistryCalculateCost(t *testing.T) {
	t.Parallel()

	reg := cost.NewPricingRegistry()
	reg.Set("openai", "gpt-4o", &cost.ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	})

	usage := ryn.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	c := reg.CalculateCost("openai", "gpt-4o", usage)
	assertEqual(t, c.InputCost, 5.0)
	assertEqual(t, c.OutputCost, 15.0)
	assertEqual(t, c.Currency, "USD")

	// No pricing → zero cost
	c2 := reg.CalculateCost("unknown", "unknown-model", usage)
	assertEqual(t, c2.TotalCost, 0.0)
}

func TestNilModelPricingCalculateCost(t *testing.T) {
	t.Parallel()
	var p *cost.ModelPricing
	c := p.CalculateCost(ryn.Usage{InputTokens: 100, OutputTokens: 100})
	assertEqual(t, c.TotalCost, 0.0)
	assertEqual(t, c.Currency, "")
}

func TestInitializePricingRegistry(t *testing.T) {
	t.Parallel()

	reg := cost.NewPricingRegistry()
	cost.InitializePricingRegistry(reg)

	// Check a few known models
	openai := reg.Get("openai", "gpt-4o")
	assertTrue(t, openai != nil)
	assertEqual(t, openai.InputCostPer1M, 5.00)

	anthropic := reg.Get("anthropic", "claude-3-5-sonnet-20241022")
	assertTrue(t, anthropic != nil)
	assertTrue(t, anthropic.InputCostPer1M > 0)

	google := reg.Get("google", "gemini-2.0-flash")
	assertTrue(t, google != nil)

	bedrock := reg.Get("bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0")
	assertTrue(t, bedrock != nil)

	// Default fallback
	openaiDefault := reg.Get("openai", "gpt-99999")
	assertTrue(t, openaiDefault != nil)
}

func TestGetPricingRegistry(t *testing.T) {
	t.Parallel()

	reg := cost.GetPricingRegistry()
	assertTrue(t, reg != nil)

	// Should always return the same instance
	reg2 := cost.GetPricingRegistry()
	assertTrue(t, reg == reg2)
}

func TestPackageLevelCalculateCost(t *testing.T) {
	t.Parallel()

	// Initialize so we have pricing data
	cost.InitializePricingRegistry(cost.GetPricingRegistry())

	usage := ryn.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	c := cost.CalculateCost("openai", "gpt-4o", usage)
	assertEqual(t, c.InputCost, 5.0)
	assertEqual(t, c.Currency, "USD")

	// Unknown → zero
	c2 := cost.CalculateCost("unknown-provider", "unknown-model", usage)
	assertEqual(t, c2.TotalCost, 0.0)
}

func TestModelPricingWithCacheFields(t *testing.T) {
	t.Parallel()

	pricing := &cost.ModelPricing{
		InputCostPer1M:      3.00,
		OutputCostPer1M:     15.00,
		CacheReadCostPer1M:  0.30,
		CacheWriteCostPer1M: 3.75,
	}
	usage := ryn.Usage{InputTokens: 500_000, OutputTokens: 500_000}
	c := pricing.CalculateCost(usage)
	assertEqual(t, c.InputCost, 1.5)
	assertEqual(t, c.OutputCost, 7.5)
	assertTrue(t, c.TotalCost > 0)
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

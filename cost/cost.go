package cost

import (
	"sync"

	"ryn.dev/ryn"
)

// ModelPricing represents the cost per 1M tokens for a model.
type ModelPricing struct {
	// Cost per 1M input tokens
	InputCostPer1M float64
	// Cost per 1M output tokens
	OutputCostPer1M float64
	// Optional: cache read cost per 1M tokens (if supported)
	CacheReadCostPer1M float64
	// Optional: cache write cost per 1M tokens (if supported)
	CacheWriteCostPer1M float64
}

// CalculateCost computes the cost for a given usage.
func (mp *ModelPricing) CalculateCost(usage ryn.Usage) ryn.Cost {
	if mp == nil {
		return ryn.Cost{}
	}
	return ryn.Cost{
		InputCost:  float64(usage.InputTokens) * mp.InputCostPer1M / 1_000_000,
		OutputCost: float64(usage.OutputTokens) * mp.OutputCostPer1M / 1_000_000,
		TotalCost:  (float64(usage.InputTokens)*mp.InputCostPer1M + float64(usage.OutputTokens)*mp.OutputCostPer1M) / 1_000_000,
		Currency:   "USD",
	}
}

// PricingRegistry manages model pricing across providers.
type PricingRegistry struct {
	mu       sync.RWMutex
	pricing  map[string]*ModelPricing // key: "provider:model"
	defaults map[string]*ModelPricing // key: provider (fallback)
}

// NewPricingRegistry creates an empty pricing registry.
func NewPricingRegistry() *PricingRegistry {
	return &PricingRegistry{
		pricing:  make(map[string]*ModelPricing),
		defaults: make(map[string]*ModelPricing),
	}
}

// Set registers pricing for a specific model.
func (pr *PricingRegistry) Set(provider, model string, pricing *ModelPricing) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if provider == "" || model == "" || pricing == nil {
		return
	}
	key := provider + ":" + model
	pr.pricing[key] = pricing
}

// SetDefault registers a default pricing for a provider (fallback for unknown models).
func (pr *PricingRegistry) SetDefault(provider string, pricing *ModelPricing) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if provider == "" || pricing == nil {
		return
	}
	pr.defaults[provider] = pricing
}

// Get retrieves pricing for a model, falling back to provider default if not found.
func (pr *PricingRegistry) Get(provider, model string) *ModelPricing {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	// Try exact match first
	key := provider + ":" + model
	if p, ok := pr.pricing[key]; ok {
		return p
	}

	// Fall back to provider default
	if p, ok := pr.defaults[provider]; ok {
		return p
	}

	return nil
}

// CalculateCost computes cost for a generation.
func (pr *PricingRegistry) CalculateCost(provider, model string, usage ryn.Usage) ryn.Cost {
	pricing := pr.Get(provider, model)
	if pricing == nil {
		return ryn.Cost{} // No pricing info
	}
	return pricing.CalculateCost(usage)
}

// defaultPricingRegistry is a global pricing registry.
var defaultPricingRegistry = NewPricingRegistry()

// InitializePricingRegistry populates reg with known pricing.
// This is set up for common models as of 2025.
func InitializePricingRegistry(reg *PricingRegistry) {
	// OpenAI pricing (as of 2025)
	reg.Set("openai", "gpt-4o", &ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	})
	reg.Set("openai", "gpt-4-turbo", &ModelPricing{
		InputCostPer1M:  10.00,
		OutputCostPer1M: 30.00,
	})
	reg.Set("openai", "gpt-4", &ModelPricing{
		InputCostPer1M:  30.00,
		OutputCostPer1M: 60.00,
	})
	reg.Set("openai", "gpt-3.5-turbo", &ModelPricing{
		InputCostPer1M:  0.50,
		OutputCostPer1M: 1.50,
	})
	reg.SetDefault("openai", &ModelPricing{
		InputCostPer1M:  5.00,
		OutputCostPer1M: 15.00,
	})

	// Anthropic pricing (as of 2025)
	reg.Set("anthropic", "claude-3-5-sonnet-20241022", &ModelPricing{
		InputCostPer1M:  3.00,
		OutputCostPer1M: 15.00,
	})
	reg.Set("anthropic", "claude-3-opus-20250219", &ModelPricing{
		InputCostPer1M:  15.00,
		OutputCostPer1M: 75.00,
	})
	reg.SetDefault("anthropic", &ModelPricing{
		InputCostPer1M:  3.00,
		OutputCostPer1M: 15.00,
	})

	// Google Gemini pricing (as of 2025)
	reg.Set("google", "gemini-2.0-flash", &ModelPricing{
		InputCostPer1M:  0.075,
		OutputCostPer1M: 0.30,
	})
	reg.Set("google", "gemini-1.5-pro", &ModelPricing{
		InputCostPer1M:  1.25,
		OutputCostPer1M: 5.00,
	})
	reg.SetDefault("google", &ModelPricing{
		InputCostPer1M:  0.075,
		OutputCostPer1M: 0.30,
	})

	// AWS Bedrock pricing (on-demand, as of 2025)
	reg.Set("bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0", &ModelPricing{
		InputCostPer1M:  3.00,
		OutputCostPer1M: 15.00,
	})
	reg.SetDefault("bedrock", &ModelPricing{
		InputCostPer1M:  0.50,
		OutputCostPer1M: 1.50,
	})
}

// GetPricingRegistry returns the global pricing registry.
// Call InitializePricingRegistry(GetPricingRegistry()) first if you want pre-populated model pricing.
func GetPricingRegistry() *PricingRegistry {
	return defaultPricingRegistry
}

// CalculateCost computes cost using the global registry.
func CalculateCost(provider, model string, usage ryn.Usage) ryn.Cost {
	return defaultPricingRegistry.CalculateCost(provider, model, usage)
}

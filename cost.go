package niro

// Cost represents the calculated cost of a generation.
type Cost struct {
	InputCost  float64 // Cost of input tokens
	OutputCost float64 // Cost of output tokens
	TotalCost  float64 // InputCost + OutputCost
	Currency   string  // Currency code (e.g. "USD")
}

// Add accumulates costs.
func (c *Cost) Add(other Cost) {
	if c == nil {
		return
	}
	c.InputCost += other.InputCost
	c.OutputCost += other.OutputCost
	c.TotalCost += other.TotalCost
}

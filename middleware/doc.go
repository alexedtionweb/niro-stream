// Package middleware provides provider wrappers for retry, timeout, response cache, and tracing.
//
// Wrap a niro.Provider to add production behavior:
//
//	p := openai.New(apiKey)
//	p = middleware.NewRetryProvider(p, middleware.RetryConfig{...})
//	p = middleware.NewTimeoutProvider(p, 5*time.Minute)
//
// All wrappers implement niro.Provider. Compose in any order; typically
// retry innermost, then timeout, then cache or tracing.
package middleware

package ryn

import (
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
	"time"
)

// Transport creates a production-grade *http.Transport tuned for
// high-concurrency LLM API traffic.
//
// Key characteristics:
//   - Aggressive keep-alive to amortize TLS handshakes
//   - Large idle connection pool (matches expected provider concurrency)
//   - TLS 1.2+ with session ticket resumption
//   - HTTP/2 when available (most LLM APIs support it)
//   - Timeouts tuned for streaming (long response, fast connect)
//
// Usage:
//
//	// Use as the transport for any provider's HTTP client
//	httpClient := &http.Client{Transport: ryn.Transport(nil)}
//	llm := compat.New(baseURL, apiKey, compat.WithClient(httpClient))
//
// Or with custom options:
//
//	httpClient := &http.Client{Transport: ryn.Transport(&ryn.TransportOptions{
//	    MaxIdleConnsPerHost: 50,
//	    IdleConnTimeout:     5 * time.Minute,
//	})}
type TransportOptions struct {
	// MaxIdleConns is the total number of idle connections across all hosts.
	// Default: GOMAXPROCS * 64 (scales with available CPUs).
	MaxIdleConns int

	// MaxIdleConnsPerHost is the max idle connections per host.
	// Default: GOMAXPROCS * 16. Set high for single-provider deployments.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost limits total connections (active + idle) per host.
	// 0 = unlimited (default). Set this for rate-limited APIs.
	MaxConnsPerHost int

	// IdleConnTimeout is how long idle connections live in the pool.
	// Default: 120s. Increase for bursty traffic patterns.
	IdleConnTimeout time.Duration

	// TLSHandshakeTimeout is the max time for a TLS handshake.
	// Default: 10s.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout is the max time to wait for response headers
	// after sending the request. Default: 30s.
	// Note: streaming bodies can take much longer — this only covers headers.
	ResponseHeaderTimeout time.Duration

	// DialTimeout is the max time for TCP connection establishment.
	// Default: 10s.
	DialTimeout time.Duration

	// KeepAlive is the TCP keep-alive interval.
	// Default: 30s. Set lower for aggressive detection of dead connections.
	KeepAlive time.Duration

	// ForceHTTP2 enables HTTP/2 even when the server doesn't advertise it.
	// Default: false (negotiate via ALPN — works for all major LLM APIs).
	ForceHTTP2 bool

	// DisableKeepAlives disables HTTP keep-alive connections.
	// Default: false. Only set for debugging.
	DisableKeepAlives bool
}

// Transport creates an optimized *http.Transport.
// Pass nil opts for production defaults.
func Transport(opts *TransportOptions) *http.Transport {
	if opts == nil {
		opts = &TransportOptions{}
	}

	procs := runtime.GOMAXPROCS(0)

	maxIdle := opts.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = procs * 64
	}

	maxIdlePerHost := opts.MaxIdleConnsPerHost
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = procs * 16
	}

	idleTimeout := opts.IdleConnTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}

	tlsTimeout := opts.TLSHandshakeTimeout
	if tlsTimeout <= 0 {
		tlsTimeout = 10 * time.Second
	}

	respHeaderTimeout := opts.ResponseHeaderTimeout
	if respHeaderTimeout <= 0 {
		respHeaderTimeout = 30 * time.Second
	}

	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}

	keepAlive := opts.KeepAlive
	if keepAlive <= 0 {
		keepAlive = 30 * time.Second
	}

	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,

		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdlePerHost,
		MaxConnsPerHost:     opts.MaxConnsPerHost, // 0 = unlimited
		IdleConnTimeout:     idleTimeout,

		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: respHeaderTimeout,

		// ExpectContinueTimeout for POST bodies (LLM requests are POST)
		ExpectContinueTimeout: 1 * time.Second,

		// Enable compression for non-streaming requests
		DisableCompression: false,

		// Keep-alive toggle
		DisableKeepAlives: opts.DisableKeepAlives,

		// TLS config: allow session resumption
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},

		// Write buffer: 64KB is good for typical LLM request payloads
		WriteBufferSize: 64 << 10,
		// Read buffer: 32KB for streaming SSE
		ReadBufferSize: 32 << 10,

		ForceAttemptHTTP2: !opts.ForceHTTP2, // negotiate via ALPN by default
	}

	return t
}

// DefaultTransport is a process-wide optimized transport.
// Use this as the default for all providers unless you need isolation.
var DefaultTransport = Transport(nil)

// HTTPClient creates an *http.Client using the optimized transport.
// This is a convenience for passing to provider constructors.
//
//	llm := compat.New(url, key, compat.WithClient(ryn.HTTPClient(nil)))
func HTTPClient(opts *TransportOptions) *http.Client {
	return &http.Client{
		Transport: Transport(opts),
		// No global timeout — streaming responses have unbounded body duration.
		// Per-request timeouts are handled via context.Context.
	}
}

// DefaultHTTPClient is a process-wide optimized HTTP client.
var DefaultHTTPClient = HTTPClient(nil)

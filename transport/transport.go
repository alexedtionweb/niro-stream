// Package transport provides production-grade HTTP transport and client
// configurations tuned for high-concurrency LLM API traffic.
package transport

import (
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
	"time"
)

// Options configures an HTTP transport.
type Options struct {
	// MaxIdleConns is the total number of idle connections across all hosts.
	// Default: GOMAXPROCS * 64 (scales with available CPUs).
	MaxIdleConns int

	// MaxIdleConnsPerHost is the max idle connections per host.
	// Default: GOMAXPROCS * 16.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost limits total connections (active + idle) per host.
	// 0 = unlimited (default).
	MaxConnsPerHost int

	// IdleConnTimeout is how long idle connections live in the pool.
	// Default: 120s.
	IdleConnTimeout time.Duration

	// TLSHandshakeTimeout is the max time for a TLS handshake.
	// Default: 10s.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout is the max time to wait for response headers.
	// Default: 30s.
	ResponseHeaderTimeout time.Duration

	// DialTimeout is the max time for TCP connection establishment.
	// Default: 10s.
	DialTimeout time.Duration

	// KeepAlive is the TCP keep-alive interval.
	// Default: 30s.
	KeepAlive time.Duration

	// ForceHTTP2 enables HTTP/2 even when the server doesn't advertise it.
	ForceHTTP2 bool

	// DisableKeepAlives disables HTTP keep-alive connections.
	DisableKeepAlives bool
}

// New creates a production-grade *http.Transport tuned for
// high-concurrency LLM API traffic.
//
// Key characteristics:
//   - Aggressive keep-alive to amortize TLS handshakes
//   - Large idle connection pool
//   - TLS 1.2+ with session ticket resumption
//   - HTTP/2 when available
func New(opts *Options) *http.Transport {
	if opts == nil {
		opts = &Options{}
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
		MaxConnsPerHost:     opts.MaxConnsPerHost,
		IdleConnTimeout:     idleTimeout,

		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: respHeaderTimeout,

		ExpectContinueTimeout: 1 * time.Second,

		DisableCompression: false,
		DisableKeepAlives:  opts.DisableKeepAlives,

		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},

		WriteBufferSize: 64 << 10,
		ReadBufferSize:  32 << 10,

		ForceAttemptHTTP2: true,
	}

	return t
}

// Default is a process-wide optimized transport.
var Default = New(nil)

// NewHTTPClient creates an *http.Client using the optimized transport.
func NewHTTPClient(opts *Options) *http.Client {
	return &http.Client{
		Transport: New(opts),
	}
}

// DefaultHTTPClient is a process-wide optimized HTTP client.
var DefaultHTTPClient = NewHTTPClient(nil)

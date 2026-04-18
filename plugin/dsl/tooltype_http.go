package dsl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
	"github.com/theory/jsonpath"
)

const (
	defaultHTTPToolTimeout = 30 * time.Second // per-request timeout for HTTP tools
	defaultHTTPMaxBodyBytes = 8 << 20          // 8 MiB cap on response body read into memory
	outputModeText          = "text"
)

// HTTPToolPolicy controls outbound network access for DSL "http" tools.
//
// LLM-driven tool calls expose the host to SSRF: a model that has been
// jailbroken or prompt-injected can ask the tool layer to fetch
// http://169.254.169.254/, http://localhost:6379/, or any internal service.
// The default policy is conservative — only public HTTP(S) destinations are
// allowed. Operators can opt into private targets explicitly via
// [SetHTTPToolPolicy].
type HTTPToolPolicy struct {
	// AllowPrivateNetworks permits requests to RFC1918, link-local,
	// loopback, ULA, and other non-public destinations. Default: false.
	AllowPrivateNetworks bool

	// AllowedSchemes is the set of URL schemes that may be requested.
	// Defaults to {"http", "https"}. file://, gopher://, ftp:// and any
	// other scheme are rejected unless explicitly listed here.
	AllowedSchemes []string

	// AllowedHosts, if non-empty, is an exact-match host allowlist applied
	// after scheme and IP checks. Useful in regulated environments where
	// only specific upstream APIs may be reached. Use lowercased hostnames.
	AllowedHosts []string

	// MaxResponseBytes caps how much of a successful response body is read
	// into memory. Defaults to defaultHTTPMaxBodyBytes when zero. Bodies
	// larger than the cap are truncated; the truncation is transparent to
	// callers that consume the parsed result.
	MaxResponseBytes int64
}

var httpToolPolicy atomic.Pointer[HTTPToolPolicy]

// SetHTTPToolPolicy installs the global HTTP tool security policy.
// Pass nil to restore defaults. Safe to call concurrently.
func SetHTTPToolPolicy(p *HTTPToolPolicy) {
	if p == nil {
		httpToolPolicy.Store(nil)
		return
	}
	cp := *p
	httpToolPolicy.Store(&cp)
}

func currentHTTPToolPolicy() HTTPToolPolicy {
	if p := httpToolPolicy.Load(); p != nil {
		return *p
	}
	return HTTPToolPolicy{}
}

func (p HTTPToolPolicy) maxBytes() int64 {
	if p.MaxResponseBytes > 0 {
		return p.MaxResponseBytes
	}
	return defaultHTTPMaxBodyBytes
}

func (p HTTPToolPolicy) schemeAllowed(scheme string) bool {
	scheme = strings.ToLower(scheme)
	if len(p.AllowedSchemes) == 0 {
		return scheme == "http" || scheme == "https"
	}
	for _, s := range p.AllowedSchemes {
		if strings.EqualFold(s, scheme) {
			return true
		}
	}
	return false
}

func (p HTTPToolPolicy) hostAllowed(host string) bool {
	if len(p.AllowedHosts) == 0 {
		return true
	}
	host = strings.ToLower(host)
	for _, h := range p.AllowedHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// validateHTTPToolURL enforces the active [HTTPToolPolicy] for a tool URL
// resolved at request time. It rejects unsupported schemes, disallowed hosts,
// and (unless explicitly enabled) any IP literal that resolves to a private,
// loopback, or link-local destination.
func validateHTTPToolURL(raw string) (*url.URL, error) {
	policy := currentHTTPToolPolicy()
	u, err := url.Parse(raw)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "http tool: invalid url", err)
	}
	if !policy.schemeAllowed(u.Scheme) {
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "http tool: scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "http tool: url is missing host")
	}
	if !policy.hostAllowed(host) {
		return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest, "http tool: host %q not in allowlist", host)
	}
	if !policy.AllowPrivateNetworks {
		if ip := net.ParseIP(host); ip != nil {
			if isPrivateIP(ip) {
				return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest,
					"http tool: refusing to call private/loopback address %q (set HTTPToolPolicy.AllowPrivateNetworks to override)", host)
			}
		}
		// Empty hostnames and obvious local aliases are rejected without DNS.
		if isLocalHostname(host) {
			return nil, niro.NewErrorf(niro.ErrCodeInvalidRequest,
				"http tool: refusing to call local hostname %q (set HTTPToolPolicy.AllowPrivateNetworks to override)", host)
		}
	}
	return u, nil
}

func isLocalHostname(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	switch h {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	return strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local")
}

// isPrivateIP reports whether ip is in a range we never want a model-driven
// tool call to reach by default: loopback, link-local, private (RFC1918),
// unique-local (fc00::/7), unspecified, multicast, and the IMDS literal.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsPrivate() {
		return true
	}
	// Cloud metadata endpoints — IsLinkLocalUnicast already covers them, but
	// we list the IPv6 form explicitly so future Go versions cannot regress.
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	return false
}

func init() {
	RegisterToolType("http", ToolTypeBuilderFunc(BuildHTTPTool))
}

// HTTPToolSpec is the DSL shape for type "http" tools (flat: no schema/http wrappers).
// Input = tool param JSON Schema. Method, url, headers, timeout at top level.
// All string fields are Go text/template with RunContext + call args as data.
// Output: "text" = return raw response body as string; "json" or omit = use cases/parse or decode JSON.
type HTTPToolSpec struct {
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Input       json.RawMessage   `json:"input"`
	Output      string            `json:"output,omitempty"` // "text" | "json" (default): text = raw body; json = cases/parse or decode
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
	Cases       []httpCase        `json:"cases,omitempty"`
	Retry       *struct {
		MaxRetries int    `json:"max_retries"`
		Backoff    string `json:"backoff"`
	} `json:"retry,omitempty"`
}

type httpCase struct {
	When  string         `json:"when"`
	Parse map[string]any `json:"parse"`
}

// BuildHTTPTool builds a tools.ToolDefinition for a type "http" tool.
func BuildHTTPTool(name string, spec json.RawMessage) (tools.ToolDefinition, error) {
	s, err := niro.UnmarshalTo[HTTPToolSpec](spec)
	if err != nil {
		return tools.ToolDefinition{}, niro.WrapError(niro.ErrCodeInvalidRequest, "http tool spec", err)
	}
	if len(s.Input) == 0 {
		return tools.ToolDefinition{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "http tool %q: input is required", name)
	}
	var inputMap map[string]any
	if err := niro.JSONUnmarshal(s.Input, &inputMap); err != nil {
		return tools.ToolDefinition{}, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "http tool %q: input", err, name)
	}
	schema, err := niro.JSONMarshal(inputMap)
	if err != nil {
		return tools.ToolDefinition{}, err
	}
	if s.URL == "" {
		return tools.ToolDefinition{}, niro.NewErrorf(niro.ErrCodeInvalidRequest, "http tool %q: url is required", name)
	}
	method := s.Method
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	desc := s.Description
	if desc == "" {
		desc = "HTTP " + method + " " + s.URL
		if len(desc) > 256 {
			desc = desc[:253] + "..."
		}
	}
	specCopy := s
	handler := func(ctx context.Context, args json.RawMessage) (any, error) {
		return runHTTPTool(ctx, &specCopy, args)
	}
	return tools.NewToolDefinition(name, desc, schema, handler)
}

func runHTTPTool(ctx context.Context, spec *HTTPToolSpec, args json.RawMessage) (any, error) {
	req, method, urlStr, err := buildHTTPRequest(ctx, spec, args)
	if err != nil {
		return nil, err
	}
	timeout := parseDuration(spec.Timeout)
	if timeout <= 0 {
		timeout = defaultHTTPToolTimeout
	}
	client := &http.Client{Timeout: timeout}
	if spec.Retry != nil && spec.Retry.MaxRetries > 0 {
		client.Transport = &retryTransport{
			Base:       http.DefaultTransport,
			MaxRetries: spec.Retry.MaxRetries,
			Backoff:    spec.Retry.Backoff,
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	policy := currentHTTPToolPolicy()
	body, err := io.ReadAll(io.LimitReader(resp.Body, policy.maxBytes()))
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeStreamError, "http tool: read body", err)
	}
	return handleHTTPResponse(spec, method, urlStr, resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

func buildHTTPRequest(ctx context.Context, spec *HTTPToolSpec, args json.RawMessage) (*http.Request, string, string, error) {
	var argsMap map[string]any
	if len(args) > 0 {
		m, err := niro.UnmarshalTo[map[string]any](args)
		if err != nil {
			return nil, "", "", niro.WrapError(niro.ErrCodeInvalidRequest, "http tool: invalid args", err)
		}
		argsMap = m
	}
	if argsMap == nil {
		argsMap = make(map[string]any)
	}
	data := argsMap
	if runCtx := RunContextFrom(ctx); runCtx != nil {
		data = make(map[string]any, len(argsMap)+8)
		for k, v := range runCtx.envMap() {
			data[k] = v
		}
		for k, v := range argsMap {
			data[k] = v
		}
	}
	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	urlStr, err := expandTemplateString(spec.URL, data)
	if err != nil {
		return nil, "", "", err
	}
	if _, err := validateHTTPToolURL(urlStr); err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
	if err != nil {
		return nil, "", "", err
	}
	for k, v := range spec.Headers {
		exp, e := expandTemplateString(v, data)
		if e != nil {
			return nil, "", "", e
		}
		// Reject CR/LF-injected header values (CWE-93).
		if strings.ContainsAny(exp, "\r\n") {
			return nil, "", "", niro.NewErrorf(niro.ErrCodeInvalidRequest,
				"http tool: header %q contains CR/LF (header injection)", k)
		}
		req.Header.Set(k, exp)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
		// Use GetBody so the retry transport (and stdlib redirect handling)
		// can re-issue the request with a fresh body reader. Without this,
		// retries send an empty body because bytes.Reader is single-shot.
		bodyBytes := append([]byte(nil), args...)
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
	}
	return req, method, urlStr, nil
}

func handleHTTPResponse(spec *HTTPToolSpec, method, url string, statusCode int, contentType string, body []byte) (any, error) {
	if strings.TrimSpace(strings.ToLower(spec.Output)) == outputModeText {
		if statusCode >= 400 {
			return nil, niro.NewErrorf(niro.ConvertHTTPStatusToCode(statusCode), "http %s %s: %d", method, url, statusCode)
		}
		return string(body), nil
	}
	response := buildResponse(statusCode, body)
	if len(spec.Cases) > 0 {
		out, err := applyCases(response, spec.Cases)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	if statusCode >= 400 {
		return nil, niro.NewErrorf(niro.ConvertHTTPStatusToCode(statusCode), "http %s %s: %d", method, url, statusCode)
	}
	if contentType == "application/json" {
		var out any
		if err := niro.JSONUnmarshal(body, &out); err != nil {
			return string(body), nil
		}
		return out, nil
	}
	return string(body), nil
}

type retryTransport struct {
	Base       http.RoundTripper
	MaxRetries int
	Backoff    string
}

const maxRetryBackoff = 30 * time.Second

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	parsed := parseDuration(t.Backoff)
	ctx := req.Context()

	var lastErr error
	for i := 0; i <= t.MaxRetries; i++ {
		// Re-create the request body for retries — RoundTripper consumes it
		// once per call and bytes.Reader cannot be rewound by the transport.
		attempt := req
		if i > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attempt = req.Clone(ctx)
			attempt.Body = body
		}

		resp, err := base.RoundTrip(attempt)
		if err != nil {
			lastErr = err
			// Do not retry if the caller cancelled or the deadline elapsed.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if i >= t.MaxRetries {
				return nil, err
			}
			if werr := waitBackoff(ctx, retryDelay(i, parsed, "")); werr != nil {
				return nil, werr
			}
			continue
		}
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		retryAfter := resp.Header.Get("Retry-After")
		resp.Body.Close()
		if i >= t.MaxRetries {
			return resp, nil
		}
		if werr := waitBackoff(ctx, retryDelay(i, parsed, retryAfter)); werr != nil {
			return nil, werr
		}
	}
	return nil, lastErr
}

// retryDelay derives the backoff for attempt i. A Retry-After header on a
// 429/5xx response always wins, capped to maxRetryBackoff.
func retryDelay(attempt int, base time.Duration, retryAfter string) time.Duration {
	if retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > maxRetryBackoff {
				return maxRetryBackoff
			}
			return d
		}
		if t, err := http.ParseTime(retryAfter); err == nil {
			d := time.Until(t)
			if d <= 0 {
				return 0
			}
			if d > maxRetryBackoff {
				return maxRetryBackoff
			}
			return d
		}
	}
	if base <= 0 {
		base = time.Second
	}
	d := time.Duration(attempt+1) * base
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}

// waitBackoff sleeps for d, returning early (and forwarding the cancellation
// error) if ctx is cancelled. Replaces a context-blind time.Sleep.
func waitBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("http tool retry: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func buildResponse(status int, body []byte) map[string]any {
	var bodyMap any
	if len(body) > 0 {
		_ = niro.JSONUnmarshal(body, &bodyMap)
	}
	if bodyMap == nil {
		bodyMap = map[string]any{}
	}
	return map[string]any{
		"status": status,
		"body":   bodyMap,
	}
}

func applyCases(response map[string]any, cases []httpCase) (map[string]any, error) {
	for _, c := range cases {
		ok, err := EvalResponseCondition(c.When, response)
		if err != nil {
			return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "cases when %q", err, c.When)
		}
		if !ok {
			continue
		}
		out := make(map[string]any)
		for k, v := range c.Parse {
			resolved, err := resolveParseValue(response, v)
			if err != nil {
				return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "cases parse %q", err, k)
			}
			out[k] = resolved
		}
		return out, nil
	}
	return nil, niro.NewError(niro.ErrCodeInvalidRequest, "no case matched (when conditions)")
}

var (
	jsonPathCache   = make(map[string]*jsonpath.Path)
	jsonPathCacheMu sync.RWMutex
)

func resolveParseValue(response map[string]any, v any) (any, error) {
	switch s := v.(type) {
	case string:
		if strings.HasPrefix(s, "$.") {
			return jsonPathLookup(response, s)
		}
		if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1], nil
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return n, nil
		}
		if b, err := strconv.ParseBool(s); err == nil {
			return b, nil
		}
		return s, nil
	case float64:
		return s, nil
	case int:
		return s, nil
	case bool:
		return s, nil
	case nil:
		return nil, nil
	default:
		return v, nil
	}
}

// jsonPathLookup compiles and runs a JSONPath expression over response (e.g. $.body.userId, $.status, $.body.items[0].name).
// Uses github.com/theory/jsonpath (RFC 9535).
func jsonPathLookup(response map[string]any, expr string) (any, error) {
	path, err := getCompiledJSONPath(expr)
	if err != nil {
		return nil, niro.WrapErrorf(niro.ErrCodeInvalidRequest, "jsonpath %q", err, expr)
	}
	nodes := path.Select(response)
	if len(nodes) == 0 {
		return nil, nil
	}
	return nodes[0], nil
}

func getCompiledJSONPath(expr string) (*jsonpath.Path, error) {
	return GetOrCompute(&jsonPathCacheMu, &jsonPathCache, expr, func() (*jsonpath.Path, error) {
		return jsonpath.Parse(expr)
	})
}

func expandTemplateString(templateStr string, data map[string]any) (string, error) {
	return ExecuteTemplate(templateStr, data)
}

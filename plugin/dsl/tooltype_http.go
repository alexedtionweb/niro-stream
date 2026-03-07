package dsl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
	"github.com/theory/jsonpath"
)

const (
	defaultHTTPToolTimeout = 30 * time.Second // per-request timeout for HTTP tools
	outputModeText         = "text"
)

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
	req, method, url, err := buildHTTPRequest(ctx, spec, args)
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
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return handleHTTPResponse(spec, method, url, resp.StatusCode, resp.Header.Get("Content-Type"), body)
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
	url, err := expandTemplateString(spec.URL, data)
	if err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, "", "", err
	}
	for k, v := range spec.Headers {
		exp, e := expandTemplateString(v, data)
		if e != nil {
			return nil, "", "", e
		}
		req.Header.Set(k, exp)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(bytes.NewReader(args))
	}
	return req, method, url, nil
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

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Base == nil {
		t.Base = http.DefaultTransport
	}
	var lastErr error
	for i := 0; i <= t.MaxRetries; i++ {
		resp, err := t.Base.RoundTrip(req)
		if err != nil {
			lastErr = err
			if i < t.MaxRetries {
				time.Sleep(time.Duration(i+1) * time.Second)
				continue
			}
			return nil, err
		}
		if resp.StatusCode < 500 {
			return resp, nil
		}
		resp.Body.Close()
		if i < t.MaxRetries {
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
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

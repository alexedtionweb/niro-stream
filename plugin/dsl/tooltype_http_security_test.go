package dsl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPToolSSRFBlocksLoopback verifies that the default policy refuses to
// dial loopback / link-local / metadata addresses. Without this defence a
// jailbroken or prompt-injected model can pivot through the tool layer to
// reach internal services (Redis, IMDS, k8s API, etc.).
func TestHTTPToolSSRFBlocksLoopback(t *testing.T) {
	SetHTTPToolPolicy(nil) // ensure defaults

	cases := []struct {
		name string
		url  string
	}{
		{"loopback-v4", "http://127.0.0.1:6379/"},
		{"loopback-v6", "http://[::1]:6379/"},
		{"localhost", "http://localhost:8080/admin"},
		{"private-rfc1918", "http://10.0.0.5/"},
		{"link-local", "http://169.254.169.254/latest/meta-data/"},
		{"file-scheme", "file:///etc/passwd"},
		{"gopher-scheme", "gopher://evil/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &HTTPToolSpec{Method: "GET", URL: tc.url, Input: json.RawMessage(`{"type":"object"}`)}
			_, _, _, err := buildHTTPRequest(context.Background(), spec, nil)
			if err == nil {
				t.Fatalf("expected SSRF policy to reject %s, got nil error", tc.url)
			}
		})
	}
}

func TestHTTPToolPolicyAllowPrivateOptIn(t *testing.T) {
	t.Cleanup(func() { SetHTTPToolPolicy(nil) })
	SetHTTPToolPolicy(&HTTPToolPolicy{AllowPrivateNetworks: true})
	spec := &HTTPToolSpec{Method: "GET", URL: "http://127.0.0.1:9999/", Input: json.RawMessage(`{"type":"object"}`)}
	if _, _, _, err := buildHTTPRequest(context.Background(), spec, nil); err != nil {
		t.Fatalf("opt-in private should pass policy, got %v", err)
	}
}

func TestHTTPToolHostAllowlist(t *testing.T) {
	t.Cleanup(func() { SetHTTPToolPolicy(nil) })
	SetHTTPToolPolicy(&HTTPToolPolicy{AllowedHosts: []string{"api.example.com"}})

	spec := &HTTPToolSpec{Method: "GET", URL: "https://evil.example.com/", Input: json.RawMessage(`{"type":"object"}`)}
	if _, _, _, err := buildHTTPRequest(context.Background(), spec, nil); err == nil {
		t.Fatalf("expected allowlist to reject evil.example.com")
	}
	spec.URL = "https://api.example.com/v1/x"
	if _, _, _, err := buildHTTPRequest(context.Background(), spec, nil); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
}

// TestHTTPToolHeaderInjection blocks CR/LF injected via templated header
// values (e.g. `{{.event.text}}` reflected into a header).
func TestHTTPToolHeaderInjection(t *testing.T) {
	t.Cleanup(func() { SetHTTPToolPolicy(nil) })
	SetHTTPToolPolicy(&HTTPToolPolicy{AllowPrivateNetworks: true})

	spec := &HTTPToolSpec{
		Method:  "GET",
		URL:     "http://127.0.0.1/",
		Headers: map[string]string{"X-User": "alice\r\nX-Admin: true"},
		Input:   json.RawMessage(`{"type":"object"}`),
	}
	_, _, _, err := buildHTTPRequest(context.Background(), spec, nil)
	if err == nil || !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("expected header injection error, got %v", err)
	}
}

// TestHTTPToolBodyLimit verifies the response body cap so a hostile endpoint
// streaming gigabytes cannot OOM the host.
func TestHTTPToolBodyLimit(t *testing.T) {
	t.Cleanup(func() { SetHTTPToolPolicy(nil) })
	const cap = 1024
	SetHTTPToolPolicy(&HTTPToolPolicy{AllowPrivateNetworks: true, MaxResponseBytes: cap})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// Write 10x the cap.
		_, _ = w.Write([]byte(strings.Repeat("A", cap*10)))
	}))
	defer srv.Close()

	spec := &HTTPToolSpec{Method: "GET", URL: srv.URL, Output: "text", Input: json.RawMessage(`{"type":"object"}`)}
	out, err := runHTTPTool(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("runHTTPTool: %v", err)
	}
	s, _ := out.(string)
	if len(s) != cap {
		t.Fatalf("body not truncated: got %d bytes, want %d", len(s), cap)
	}
}

// TestRetryTransportContextCancel verifies the retry backoff observes
// ctx cancellation instead of blocking on time.Sleep.
func TestRetryTransportContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	tr := &retryTransport{Base: http.DefaultTransport, MaxRetries: 5, Backoff: "10s"}

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)

	done := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip(req)
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry transport ignored ctx cancel (still sleeping)")
	}
}

// TestRetryTransportBodyResent verifies that POST retries actually re-send
// the request body — the previous implementation reused a single
// bytes.Reader which got drained on the first attempt.
func TestRetryTransportBodyResent(t *testing.T) {
	t.Cleanup(func() { SetHTTPToolPolicy(nil) })
	SetHTTPToolPolicy(&HTTPToolPolicy{AllowPrivateNetworks: true})

	var calls atomic.Int32
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		if calls.Add(1) <= 1 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	spec := &HTTPToolSpec{
		Method: "POST",
		URL:    srv.URL,
		Input:  json.RawMessage(`{"type":"object"}`),
		Retry:  &struct {
			MaxRetries int    `json:"max_retries"`
			Backoff    string `json:"backoff"`
		}{MaxRetries: 1, Backoff: "1ms"},
	}
	args := json.RawMessage(`{"hello":"world"}`)
	if _, err := runHTTPTool(context.Background(), spec, args); err != nil {
		t.Fatalf("runHTTPTool: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(seen))
	}
	for i, body := range seen {
		if body != string(args) {
			t.Errorf("attempt %d body = %q, want %q (retry must resend body)", i, body, string(args))
		}
	}
}

// TestRetryAfterHeader verifies the transport honours the upstream
// Retry-After hint when it is shorter than the backoff schedule.
func TestRetryAfterHeader(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := &retryTransport{Base: http.DefaultTransport, MaxRetries: 1, Backoff: "10s"}
	req, _ := http.NewRequest("GET", srv.URL, nil)

	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Retry-After: 0 should not wait long, took %v", took)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 after retry, got %d", resp.StatusCode)
	}
}

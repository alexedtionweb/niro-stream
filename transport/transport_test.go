package transport_test

import (
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream/transport"
)

func TestTransportDefaults(t *testing.T) {
	t.Parallel()

	tr := transport.New(nil)
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	if tr.MaxIdleConns <= 0 {
		t.Error("expected MaxIdleConns > 0")
	}
	if tr.MaxIdleConnsPerHost <= 0 {
		t.Error("expected MaxIdleConnsPerHost > 0")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("expected IdleConnTimeout > 0")
	}
	if tr.DisableKeepAlives {
		t.Error("expected DisableKeepAlives to be false")
	}
}

func TestTransportCustomOptions(t *testing.T) {
	t.Parallel()

	tr := transport.New(&transport.Options{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 25,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   true,
	})
	if tr.MaxIdleConns != 100 {
		t.Errorf("got MaxIdleConns %d, want 100", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 25 {
		t.Errorf("got MaxIdleConnsPerHost %d, want 25", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 50 {
		t.Errorf("got MaxConnsPerHost %d, want 50", tr.MaxConnsPerHost)
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("got IdleConnTimeout %v, want 30s", tr.IdleConnTimeout)
	}
	if !tr.DisableKeepAlives {
		t.Error("expected DisableKeepAlives to be true")
	}
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()
	client := transport.NewHTTPClient(nil)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Transport == nil {
		t.Error("expected non-nil Transport")
	}
}

func TestDefaultTransportAndClient(t *testing.T) {
	t.Parallel()
	if transport.Default == nil {
		t.Error("expected non-nil Default transport")
	}
	if transport.DefaultHTTPClient == nil {
		t.Error("expected non-nil DefaultHTTPClient")
	}
}

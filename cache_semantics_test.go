package niro_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

type testCacheEngine struct{}

func (testCacheEngine) ResolvePrefixHash(ctx context.Context, req *niro.Request, scope niro.CacheScope) (string, bool, error) {
	return "", false, nil
}

func (testCacheEngine) StorePrefix(ctx context.Context, key string, scope niro.CacheScope, ttl time.Duration, meta map[string]string) error {
	return nil
}

func (testCacheEngine) LookupPrefix(ctx context.Context, key string, scope niro.CacheScope) (map[string]string, bool, error) {
	return nil, false, nil
}

type cacheCapsProvider struct {
	niro.ProviderFunc
}

func (cacheCapsProvider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,
		SupportsExplicitKeys: false,
		SupportsTTL:          true,
	}
}

func TestNormalizeCacheOptions_ExplicitKeyNamespaced(t *testing.T) {
	t.Parallel()

	req := &niro.Request{
		Client:   "tenant-a",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{
				Mode: niro.CachePrefer,
				Key:  "prefix-v1",
				TTL:  5 * time.Minute,
			},
		},
	}

	h, err := niro.NormalizeCacheOptions(req, nil)
	assertNil(t, err)
	assertEqual(t, h.Mode, niro.CachePrefer)
	assertEqual(t, h.Scope, niro.CacheScopePrefix)
	assertEqual(t, h.Key, "tenant-a:prefix-v1")
	assertEqual(t, h.TTL, 5*time.Minute)
}

func TestNormalizeCacheOptions_ExplicitKeyRequiresTenantNamespace(t *testing.T) {
	t.Parallel()

	req := &niro.Request{
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{
				Mode: niro.CachePrefer,
				Key:  "prefix-v1",
			},
		},
	}

	_, err := niro.NormalizeCacheOptions(req, nil)
	assertNotNil(t, err)
	assertErrorContains(t, err, "tenant namespace")
}

func TestNormalizeCacheOptions_DerivesDeterministicKey(t *testing.T) {
	t.Parallel()

	req := &niro.Request{
		Client:   "tenant-a",
		Messages: []niro.Message{niro.UserText("hello")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
		},
	}

	const normalized = "deterministic-prefix"
	norm := niro.PrefixNormalizerFunc(func(req *niro.Request) ([]byte, error) { return []byte(normalized), nil })

	h, err := niro.NormalizeCacheOptions(req, norm)
	assertNil(t, err)
	assertTrue(t, h.Key != "")
	assertTrue(t, h.PrefixHash != "")
	assertTrue(t, h.Key[:9] == "tenant-a:")

	seed := "tenant-a:(default):" + normalized
	sum := sha256.Sum256([]byte(seed))
	wantHash := hex.EncodeToString(sum[:])
	assertEqual(t, h.PrefixHash, wantHash)
	assertEqual(t, h.Key, "tenant-a:"+wantHash)
}

func TestOptionsValidate_CacheScopeUnsupported(t *testing.T) {
	t.Parallel()

	o := &niro.Options{
		Cache: &niro.CacheOptions{
			Scope: niro.CacheScope(99),
		},
	}

	err := o.Validate()
	assertErrorContains(t, err, "cache")
}

func TestNormalizeCacheOptions_RequireTenantNamespace(t *testing.T) {
	t.Parallel()

	req := &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
		},
	}
	_, err := niro.NormalizeCacheOptions(req, niro.DefaultPrefixNormalizer{})
	assertNotNil(t, err)
	assertErrorContains(t, err, "tenant namespace")
}

func TestNormalizeCacheOptions_BypassSkipsTenantRequirement(t *testing.T) {
	t.Parallel()

	req := &niro.Request{
		Messages: []niro.Message{niro.UserText("hello")},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CacheBypass},
		},
	}

	h, err := niro.NormalizeCacheOptions(req, niro.DefaultPrefixNormalizer{})
	assertNil(t, err)
	assertEqual(t, h.Mode, niro.CacheBypass)
	assertEqual(t, h.Key, "")
	assertEqual(t, h.PrefixHash, "")
}

func TestNormalizeCacheOptions_DefaultNormalizerCanonicalJSONOrder(t *testing.T) {
	t.Parallel()

	reqA := &niro.Request{
		Client: "tenant-a",
		Messages: []niro.Message{
			niro.UserText("  hello\r\nworld  "),
			{
				Role: niro.RoleAssistant,
				Parts: []niro.Part{{
					Kind: niro.KindToolCall,
					Tool: &niro.ToolCall{
						ID:   "c1",
						Name: "lookup",
						Args: []byte(`{"b":2,"a":1}`),
					},
				}},
			},
		},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
		},
	}
	reqB := &niro.Request{
		Client: "tenant-a",
		Messages: []niro.Message{
			niro.UserText("hello\nworld"),
			{
				Role: niro.RoleAssistant,
				Parts: []niro.Part{{
					Kind: niro.KindToolCall,
					Tool: &niro.ToolCall{
						ID:   "c1",
						Name: "lookup",
						Args: []byte(` { "a" : 1, "b":2 } `),
					},
				}},
			},
		},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
		},
	}

	hA, err := niro.NormalizeCacheOptions(reqA, nil)
	assertNil(t, err)
	hB, err := niro.NormalizeCacheOptions(reqB, nil)
	assertNil(t, err)

	assertEqual(t, hA.Key, hB.Key)
	assertEqual(t, hA.PrefixHash, hB.PrefixHash)
}

func TestNormalizeCacheOptions_DefaultNormalizerBinaryAffectsKey(t *testing.T) {
	t.Parallel()

	base := niro.Request{
		Client: "tenant-a",
		Messages: []niro.Message{{
			Role: niro.RoleUser,
			Parts: []niro.Part{{
				Kind: niro.KindImage,
				Data: []byte{1, 2, 3, 4},
				Mime: "image/png",
			}},
		}},
		Options: niro.Options{
			Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
		},
	}
	other := base
	other.Messages = []niro.Message{{
		Role: niro.RoleUser,
		Parts: []niro.Part{{
			Kind: niro.KindImage,
			Data: []byte{1, 2, 3, 5},
			Mime: "image/png",
		}},
	}}

	hA, err := niro.NormalizeCacheOptions(&base, nil)
	assertNil(t, err)
	hB, err := niro.NormalizeCacheOptions(&other, nil)
	assertNil(t, err)
	assertTrue(t, hA.Key != hB.Key)
	assertTrue(t, hA.PrefixHash != hB.PrefixHash)
}

func TestSetCacheUsageDetail_AndUsageAdd_MergeBooleanFields(t *testing.T) {
	t.Parallel()

	u1 := &niro.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	niro.SetCacheUsageDetail(u1, true, false, false, 0, 0)

	u2 := &niro.Usage{InputTokens: 12, OutputTokens: 8, TotalTokens: 20}
	niro.SetCacheUsageDetail(u2, true, true, true, 6, 3)

	u1.Add(u2)

	assertEqual(t, u1.Detail[niro.UsageCacheAttempted], 1)
	assertEqual(t, u1.Detail[niro.UsageCacheHit], 1)
	assertEqual(t, u1.Detail[niro.UsageCacheWrite], 1)
	assertEqual(t, u1.Detail[niro.UsageCachedInputTokens], 6)
	assertEqual(t, u1.Detail[niro.UsageCacheLatencySavedMS], 3)
}

func TestDefaultPrefixNormalizer_NormalizePrefixCanonical(t *testing.T) {
	t.Parallel()

	norm := niro.DefaultPrefixNormalizer{}
	reqA := &niro.Request{
		Model:        "gpt-4o",
		SystemPrompt: " system\r\nprompt ",
		Messages: []niro.Message{
			{
				Role: niro.RoleUser,
				Parts: []niro.Part{
					{Kind: niro.KindText, Text: "  hello\r\nworld  "},
					{Kind: niro.KindImage, Data: []byte{1, 2, 3}, Mime: "image/png"},
					{
						Kind: niro.KindToolCall,
						Tool: &niro.ToolCall{
							ID:   "id-1",
							Name: "lookup",
							Args: []byte(`{"z":1,"a":[2,1]}`),
						},
					},
				},
			},
		},
	}
	reqB := &niro.Request{
		Model:        "gpt-4o",
		SystemPrompt: "system\nprompt",
		Messages: []niro.Message{
			{
				Role: niro.RoleUser,
				Parts: []niro.Part{
					{Kind: niro.KindText, Text: "hello\nworld"},
					{Kind: niro.KindImage, Data: []byte{1, 2, 3}, Mime: "image/png"},
					{
						Kind: niro.KindToolCall,
						Tool: &niro.ToolCall{
							ID:   "id-1",
							Name: "lookup",
							Args: []byte(`{"a":[2,1], "z":1}`),
						},
					},
				},
			},
		},
	}

	pA, err := norm.NormalizePrefix(reqA)
	assertNil(t, err)
	pB, err := norm.NormalizePrefix(reqB)
	assertNil(t, err)

	assertEqual(t, string(pA), string(pB))
	assertTrue(t, strings.Contains(string(pA), `"data_sha256":"`))
	assertTrue(t, strings.Contains(string(pA), `"args":{"a":[2,1],"z":1}`))
}

func TestCacheContextHelpers_RoundTrip(t *testing.T) {
	t.Parallel()

	var eng niro.CacheEngine = testCacheEngine{}
	ctx := context.Background()
	hint := niro.CacheHint{Mode: niro.CachePrefer, Key: "tenant:key"}

	ctx = niro.AttachCacheContext(ctx, hint, eng)
	gotHint, ok := niro.GetCacheHint(ctx)
	assertTrue(t, ok)
	assertEqual(t, gotHint.Key, "tenant:key")

	gotEngine, ok := niro.GetCacheEngine(ctx)
	assertTrue(t, ok)
	assertTrue(t, gotEngine != nil)

	ctx2 := niro.WithCacheHint(ctx, niro.CacheHint{Mode: niro.CacheBypass})
	h2, ok := niro.GetCacheHint(ctx2)
	assertTrue(t, ok)
	assertEqual(t, h2.Mode, niro.CacheBypass)

	ctx3 := niro.WithCacheEngine(ctx2, eng)
	_, ok = niro.GetCacheEngine(ctx3)
	assertTrue(t, ok)
}

func TestProviderCacheCaps_AndSupportsHint(t *testing.T) {
	t.Parallel()

	p := cacheCapsProvider{ProviderFunc: func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	}}
	caps := niro.ProviderCacheCaps(p)
	assertTrue(t, caps.SupportsHint(niro.CacheHint{Mode: niro.CacheRequire, TTL: 5 * time.Minute}))
	assertTrue(t, !caps.SupportsHint(niro.CacheHint{Mode: niro.CacheRequire, Key: "k"}))
	assertTrue(t, !niro.ProviderCacheCaps(niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice(nil), nil
	})).SupportsPrefix)
}

func TestOptionsValidate_CacheInvalidValues(t *testing.T) {
	t.Parallel()

	invalidTTL := niro.Options{Cache: &niro.CacheOptions{TTL: -time.Second}}
	err := invalidTTL.Validate()
	assertErrorContains(t, err, "ttl")

	invalidKey := niro.Options{Cache: &niro.CacheOptions{Key: "tenant:key"}}
	err = invalidKey.Validate()
	assertErrorContains(t, err, "key")
}

package niro_test

import (
	"context"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/runtime"
)

func TestRuntimeCacheIntegration_HitAndMissMetrics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name         string
		cachedTokens int
		wantHit      int
		wantCachedIn int
	}{
		{name: "hit", cachedTokens: 8, wantHit: 1, wantCachedIn: 8},
		{name: "miss", cachedTokens: 0, wantHit: 0, wantCachedIn: 0},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := cacheAwareMockProvider{
				cachedTokens: tc.cachedTokens,
			}
			rt := runtime.New(mock)

			stream, err := rt.Generate(ctx, &niro.Request{
				Client:   "tenant-int",
				Model:    "test-model",
				Messages: []niro.Message{niro.UserText("hello")},
				Options: niro.Options{
					Cache: &niro.CacheOptions{Mode: niro.CachePrefer},
				},
			})
			assertNoError(t, err)

			_, err = niro.CollectText(ctx, stream)
			assertNoError(t, err)

			u := stream.Usage()
			assertEqual(t, u.Detail[niro.UsageCacheAttempted], 1)
			assertEqual(t, u.Detail[niro.UsageCacheHit], tc.wantHit)
			assertEqual(t, u.Detail[niro.UsageCachedInputTokens], tc.wantCachedIn)
		})
	}
}

type cacheAwareMockProvider struct {
	cachedTokens int
}

func (m cacheAwareMockProvider) Generate(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
	s, e := niro.NewStream(2)
	go func() {
		defer e.Close()
		_ = e.Emit(ctx, niro.TextFrame("ok"))
		u := niro.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
		_, attempted := niro.GetCacheHint(ctx)
		niro.SetCacheUsageDetail(&u, attempted, m.cachedTokens > 0, false, m.cachedTokens, 0)
		_ = e.Emit(ctx, niro.UsageFrame(&u))
	}()
	return s, nil
}

func (cacheAwareMockProvider) CacheCaps() niro.CacheCapabilities {
	return niro.CacheCapabilities{
		SupportsPrefix:       true,
		SupportsExplicitKeys: true,
		SupportsTTL:          true,
		SupportsBypass:       true,
	}
}

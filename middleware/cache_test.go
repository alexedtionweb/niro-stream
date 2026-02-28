package middleware_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/middleware"
)

func TestCacheHitMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100, TTL: time.Minute})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Model: "test", Messages: []ryn.Message{ryn.UserText("hi")}}

	// First call: miss
	s, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "hello")
	assertEqual(t, callCount, 1)

	// Second call: hit
	s2, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text2, _ := ryn.CollectText(ctx, s2)
	assertEqual(t, text2, "hello")
	assertEqual(t, callCount, 1) // not called again

	hits, misses := cache.Stats()
	assertEqual(t, hits, int64(1))
	assertEqual(t, misses, int64(1))
}

func TestCacheDifferentRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(req.Messages[0].Parts[0].Text)}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	s1, _ := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("a")}})
	ryn.CollectText(ctx, s1)

	s2, _ := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("b")}})
	ryn.CollectText(ctx, s2)

	assertEqual(t, callCount, 2) // different requests, both miss
}

func TestCacheTTLExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100, TTL: 50 * time.Millisecond})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}}

	s1, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s1)
	assertEqual(t, callCount, 1)

	time.Sleep(100 * time.Millisecond)

	s2, _ := provider.Generate(ctx, req)
	ryn.CollectText(ctx, s2)
	assertEqual(t, callCount, 2) // expired, called again
}

func TestCacheLRUEviction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 64, TTL: time.Hour})
	provider := cache.Wrap(mock)

	// Fill cache beyond capacity
	for i := 0; i < 200; i++ {
		s, _ := provider.Generate(ctx, &ryn.Request{
			Model:    fmt.Sprintf("model-%d", i),
			Messages: []ryn.Message{ryn.UserText("hi")},
		})
		ryn.CollectText(ctx, s)
	}

	assertTrue(t, cache.Len() <= 64)
}

func TestCacheClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	for i := 0; i < 10; i++ {
		s, _ := provider.Generate(ctx, &ryn.Request{
			Model:    fmt.Sprintf("m%d", i),
			Messages: []ryn.Message{ryn.UserText("x")},
		})
		ryn.CollectText(ctx, s)
	}

	assertTrue(t, cache.Len() > 0)
	cache.Clear()
	assertEqual(t, cache.Len(), 0)
}

func TestCacheConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 1000})
	provider := cache.Wrap(mock)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				req := &ryn.Request{
					Model:    fmt.Sprintf("m%d", j%5),
					Messages: []ryn.Message{ryn.UserText("hi")},
				}
				s, err := provider.Generate(ctx, req)
				if err != nil {
					t.Errorf("generate error: %v", err)
					return
				}
				ryn.CollectText(ctx, s)
			}
		}()
	}
	wg.Wait()
}

func TestCacheProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf("provider down")
	})

	// CacheOptions{} uses default MaxEntries (0 → 1024) covering that branch.
	cache := middleware.NewCache(middleware.CacheOptions{})
	provider := cache.Wrap(mock)

	_, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertErrorContains(t, err, "provider down")
}

func TestCacheStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(4)
		go func() {
			defer em.Close()
			em.Error(fmt.Errorf("stream error"))
		}()
		return out, nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	s, err := provider.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	_, err = ryn.Collect(ctx, s)
	assertErrorContains(t, err, "stream error")
}

func TestCacheCustomKeyFn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	// Fixed key → all requests map to same cache entry.
	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 100,
		TTL:        time.Minute,
		KeyFn: func(*ryn.Request) [32]byte {
			return [32]byte{1, 2, 3}
		},
	})
	provider := cache.Wrap(mock)

	for i := 0; i < 3; i++ {
		s, _ := provider.Generate(ctx, &ryn.Request{
			Model:    fmt.Sprintf("model-%d", i),
			Messages: []ryn.Message{ryn.UserText("hi")},
		})
		ryn.CollectText(ctx, s)
	}
	// All requests resolve to the same key → only the first is a miss.
	assertEqual(t, callCount, 1)
}

func TestCacheMoveToFront(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Use a custom key function to place multiple entries in the same shard (shard 0).
	// Three requests: A, B, C (all → shard 0, different keys).
	// Then re-access B → moveToFront(B) where B is in the middle → triggers actual move.
	keySeq := []int{1, 2, 3, 1, 2} // key discriminators in order
	keyIdx := 0

	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 2048, // large enough to avoid eviction
		TTL:        time.Minute,
		KeyFn: func(req *ryn.Request) [32]byte {
			var k [32]byte
			k[0] = 0                      // shard 0
			k[1] = byte(keySeq[keyIdx%5]) // cyclic index
			keyIdx++
			return k
		},
	})

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(req.Model)}), nil
	})
	provider := cache.Wrap(mock)

	req := func(model string) *ryn.Request {
		return &ryn.Request{Model: model, Messages: []ryn.Message{ryn.UserText("hi")}}
	}

	// Populate shard 0 with 3 distinct entries: key1(A), key2(B), key3(C).
	s, _ := provider.Generate(ctx, req("A"))
	ryn.CollectText(ctx, s) // key1 → miss → put(A); head=A

	s, _ = provider.Generate(ctx, req("B"))
	ryn.CollectText(ctx, s) // key2 → miss → put(B); head=B, tail=A

	s, _ = provider.Generate(ctx, req("C"))
	ryn.CollectText(ctx, s) // key3 → miss → put(C); head=C, B in middle, tail=A

	// Re-access key1 → hit → moveToFront(A) where A is tail (not head).
	s, _ = provider.Generate(ctx, req("A-again"))
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "A") // served from cache

	// Re-access key2 → hit → moveToFront(B) where B may be middle.
	s, _ = provider.Generate(ctx, req("B-again"))
	text, _ = ryn.CollectText(ctx, s)
	assertEqual(t, text, "B") // served from cache

	hits, _ := cache.Stats()
	assertTrue(t, hits >= 2)
}

func TestCacheHitWithUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(8)
		go func() {
			defer em.Close()
			em.Emit(ctx, ryn.TextFrame("hello"))
			usage := ryn.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}
			em.Emit(ctx, ryn.UsageFrame(&usage))
		}()
		return out, nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100, TTL: time.Minute})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Model: "m", Messages: []ryn.Message{ryn.UserText("hi")}}

	// First call: miss, provider emits text + usage frame.
	s1, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	ryn.Collect(ctx, s1)

	// Second call: cache hit — should replay frames and re-emit usage.
	s2, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, s2)
	assertNoError(t, err)
	assertEqual(t, text, "hello")

	// KindUsage frames are consumed automatically by Next() and reflected in Usage().
	u := s2.Usage()
	assertTrue(t, u.InputTokens > 0 || u.OutputTokens > 0)
}

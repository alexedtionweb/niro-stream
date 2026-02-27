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

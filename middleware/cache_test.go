package middleware_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/middleware"
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

func TestCacheMissForwardsUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Provider emits a usage frame alongside text.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(8)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, ryn.TextFrame("hello"))
			u := ryn.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
			_ = em.Emit(ctx, ryn.UsageFrame(&u))
		}()
		return out, nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Model: "m", Messages: []ryn.Message{ryn.UserText("usage-test")}}

	// Cache miss: usage frame emitted by provider should be forwarded to caller.
	s, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text, err := ryn.CollectText(ctx, s)
	assertNoError(t, err)
	assertEqual(t, text, "hello")

	u := s.Usage()
	assertEqual(t, u.InputTokens, 10)
	assertEqual(t, u.OutputTokens, 5)
	assertEqual(t, u.TotalTokens, 15)
}

func TestCachePutUpdateExisting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		callCount++
		text := fmt.Sprintf("response-%d", callCount)
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(text)}), nil
	})

	// Use a custom key function that always returns the same key,
	// so the second distinct request ends up in the same cache slot.
	var fixedKey [32]byte
	fixedKey[0] = 42
	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 100,
		KeyFn:      func(*ryn.Request) [32]byte { return fixedKey },
	})
	provider := cache.Wrap(mock)

	req := &ryn.Request{Model: "m", Messages: []ryn.Message{ryn.UserText("first")}}

	// First call: miss, stores "response-1".
	s1, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	ryn.CollectText(ctx, s1)
	assertEqual(t, callCount, 1)

	// Second call with same forced key: hit, returns "response-1".
	s2, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text2, _ := ryn.CollectText(ctx, s2)
	assertEqual(t, text2, "response-1")
	assertEqual(t, callCount, 1) // still only 1 upstream call

	// Invalidate to force a fresh miss that updates the existing slot.
	cache.Clear()
	s3, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	text3, _ := ryn.CollectText(ctx, s3)
	assertEqual(t, text3, "response-2")
	assertEqual(t, callCount, 2)
}

func TestCacheMissForwardsResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Provider sets a ResponseMeta on its stream.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(4)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, ryn.TextFrame("text"))
			em.SetResponse(&ryn.ResponseMeta{Model: "model-x", FinishReason: "stop"})
		}()
		return out, nil
	})

	cache := middleware.NewCache(middleware.CacheOptions{MaxEntries: 100})
	provider := cache.Wrap(mock)
	req := &ryn.Request{Model: "model-x", Messages: []ryn.Message{ryn.UserText("x")}}

	// Cache miss — response meta should be forwarded.
	s, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	ryn.CollectText(ctx, s)
	resp := s.Response()
	assertNotNil(t, resp)
	assertEqual(t, resp.FinishReason, "stop")

	// Cache hit — response meta should also be forwarded from cache.
	s2, err := provider.Generate(ctx, req)
	assertNoError(t, err)
	ryn.CollectText(ctx, s2)
	resp2 := s2.Response()
	assertNotNil(t, resp2)
	assertEqual(t, resp2.FinishReason, "stop")
}

func TestCacheRemoveMiddleEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Build 3 entries in the same shard then access the MIDDLE entry to
	// trigger remove() on a node that has both prev AND next (covers the
	// e.next.prev = e.prev branch in remove()).
	keySeq := 0
	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 2048,
		KeyFn: func(req *ryn.Request) [32]byte {
			var k [32]byte
			k[0] = 0 // always shard 0
			k[1] = byte(keySeq)
			keySeq++
			return k
		},
	})
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(req.Model)}), nil
	})
	wrapped := cache.Wrap(mock)
	req := func(m string) *ryn.Request {
		return &ryn.Request{Model: m, Messages: []ryn.Message{ryn.UserText("x")}}
	}

	// Insert A (key=0), B (key=1), C (key=2).
	// After inserts: head=C, middle=B, tail=A.
	for _, m := range []string{"A", "B", "C"} {
		s, _ := wrapped.Generate(ctx, req(m))
		ryn.CollectText(ctx, s)
	}
	assertEqual(t, cache.Len(), 3)

	// Re-access B (key=1): this is a hit → moveToFront(B).
	// B is the MIDDLE entry (prev=C, next=A), so remove(B) hits e.next.prev = e.prev.
	// Reset keySeq so B's key (=1) is generated on the next call.
	keySeq = 1
	s, _ := wrapped.Generate(ctx, req("B-again"))
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "B") // served from cache
	hits, _ := cache.Stats()
	assertTrue(t, hits >= 1)
}

func TestCachePutUpdateExistingConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Use a slow provider so two concurrent misses race to store the same key,
	// triggering the update-existing branch in put().
	ready := make(chan struct{})
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		<-ready // block until both goroutines start
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ok")}), nil
	})

	var fixedKey [32]byte
	fixedKey[0] = 77
	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 100,
		TTL:        time.Minute, // enable TTL so the update-existing TTL branch is exercised too
		KeyFn:      func(*ryn.Request) [32]byte { return fixedKey },
	})
	provider := cache.Wrap(mock)
	req := &ryn.Request{Model: "m", Messages: []ryn.Message{ryn.UserText("x")}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s, err := provider.Generate(ctx, req)
		assertNoError(t, err)
		ryn.CollectText(ctx, s)
	}()
	go func() {
		defer wg.Done()
		s, err := provider.Generate(ctx, req)
		assertNoError(t, err)
		ryn.CollectText(ctx, s)
	}()

	// Release both goroutines simultaneously so they both miss, both put.
	close(ready)
	wg.Wait()

	// At least 1 entry should be in cache regardless of which goroutine "won".
	assertTrue(t, cache.Len() >= 1)
}

func TestCacheWrapEmitEarlyReturn(t *testing.T) {
	t.Parallel()

	// The miss goroutine calls em.Emit(ctx, f) for each frame; if ctx is
	// cancelled while the output buffer (size 32) is full, Emit returns an
	// error and the goroutine takes the early-return path.
	// Strategy: produce 64 frames (> buffer 32) so Emit blocks, then let the
	// caller context expire via a very short deadline.
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		frames := make([]ryn.Frame, 64)
		for i := range frames {
			frames[i] = ryn.TextFrame(fmt.Sprintf("f%d", i))
		}
		return ryn.StreamFromSlice(frames), nil
	})

	var fixedKey [32]byte
	fixedKey[0] = 88
	cache := middleware.NewCache(middleware.CacheOptions{
		MaxEntries: 100,
		KeyFn:      func(*ryn.Request) [32]byte { return fixedKey },
	})
	provider := cache.Wrap(mock)
	req := &ryn.Request{Model: "m", Messages: []ryn.Message{ryn.UserText("x")}}

	// Use a very short deadline — fires while the miss goroutine is blocked
	// on a full output channel, causing em.Emit to return ctx.Err().
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	stream, err := provider.Generate(ctx, req)
	assertNoError(t, err)

	// Sleep briefly so the deadline fires before we start draining.
	time.Sleep(5 * time.Millisecond)
	for stream.Next(context.Background()) {
	}
	_ = stream.Err()
}

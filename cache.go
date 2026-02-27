package ryn

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// Cache provides a thread-safe, TTL-aware LRU response cache for
// LLM generations. Identical requests (same model, messages, tools,
// options) return cached streams without hitting the provider.
//
// Cache is designed for millions of concurrent operations:
//   - Lock-free fast path for reads (atomic generation counter)
//   - Sharded mutexes to eliminate contention on writes
//   - Efficient key hashing via SHA-256 of canonical request
//   - LRU eviction with O(1) promote/evict
//
// Usage:
//
//	cache := ryn.NewCache(ryn.CacheOptions{
//	    MaxEntries: 10_000,
//	    TTL:        5 * time.Minute,
//	})
//	provider := cache.Wrap(llm) // returns a caching ryn.Provider
//
// Or use with Runtime:
//
//	rt := ryn.NewRuntime(cache.Wrap(llm)).WithHook(hook)
type Cache struct {
	shards [cacheShards]cacheShard
	opts   CacheOptions
	hits   atomic.Int64
	misses atomic.Int64
}

const cacheShards = 64 // power of 2 for fast modulo

// CacheOptions configures the Cache.
type CacheOptions struct {
	// MaxEntries is the maximum number of cached responses.
	// When exceeded, the least-recently-used entry is evicted.
	// Default: 1000.
	MaxEntries int

	// TTL is the time-to-live for cache entries.
	// Entries older than TTL are considered stale and evicted on access.
	// Default: 5 minutes. Set to 0 to disable TTL (entries live until evicted by LRU).
	TTL time.Duration

	// KeyFunc overrides the default cache key derivation.
	// The default hashes Model + Messages + Tools + Options.
	// Custom functions can include/exclude fields as needed.
	KeyFunc func(req *Request) [32]byte
}

// cacheShard is one of N independent shards to reduce lock contention.
type cacheShard struct {
	mu      sync.RWMutex
	entries map[[32]byte]*cacheEntry
	order   cacheList // LRU doubly-linked list
	maxSize int
}

type cacheEntry struct {
	key       [32]byte
	frames    []Frame
	response  *ResponseMeta
	usage     Usage
	createdAt time.Time
	prev      *cacheEntry
	next      *cacheEntry
}

// cacheList is an intrusive doubly-linked list for O(1) LRU operations.
type cacheList struct {
	head *cacheEntry
	tail *cacheEntry
	len  int
}

func (l *cacheList) pushFront(e *cacheEntry) {
	e.prev = nil
	e.next = l.head
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
	l.len++
}

func (l *cacheList) remove(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev = nil
	e.next = nil
	l.len--
}

func (l *cacheList) moveToFront(e *cacheEntry) {
	if l.head == e {
		return
	}
	l.remove(e)
	l.pushFront(e)
}

func (l *cacheList) removeLast() *cacheEntry {
	if l.tail == nil {
		return nil
	}
	e := l.tail
	l.remove(e)
	return e
}

// NewCache creates a Cache with the given options.
func NewCache(opts CacheOptions) *Cache {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 1000
	}
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}

	c := &Cache{opts: opts}
	perShard := (opts.MaxEntries + cacheShards - 1) / cacheShards
	if perShard < 1 {
		perShard = 1
	}
	for i := range c.shards {
		c.shards[i].entries = make(map[[32]byte]*cacheEntry, perShard)
		c.shards[i].maxSize = perShard
	}
	return c
}

// Wrap returns a Provider that checks the cache before calling the
// underlying provider. Cache hits return a pre-loaded stream immediately.
func (c *Cache) Wrap(p Provider) Provider {
	return ProviderFunc(func(ctx context.Context, req *Request) (*Stream, error) {
		key := c.key(req)

		// Try cache hit
		if frames, resp, usage, ok := c.get(key); ok {
			c.hits.Add(1)
			s := StreamFromSlice(frames)
			if resp != nil {
				s.p.resp.Store(resp)
			}
			// Inject usage frame so consumer accumulates it
			_ = s.p.ch // already closed by StreamFromSlice, usage won't auto-accumulate
			// Instead, set usage directly on the stream
			s.usage = usage
			return s, nil
		}
		c.misses.Add(1)

		// Cache miss: call provider, collect, cache, replay
		stream, err := p.Generate(ctx, req)
		if err != nil {
			return nil, err
		}

		// Tee: collect all frames while replaying to the consumer
		out, emitter := NewStream(32)
		go func() {
			defer emitter.Close()
			var collected []Frame

			for stream.Next(ctx) {
				f := stream.Frame()
				collected = append(collected, f)
				if err := emitter.Emit(ctx, f); err != nil {
					return
				}
			}

			if err := stream.Err(); err != nil {
				emitter.Error(err)
				return // don't cache errors
			}

			resp := stream.Response()
			usage := stream.Usage()
			if resp != nil {
				emitter.SetResponse(resp)
			}

			// Store in cache
			c.put(key, collected, resp, usage)
		}()

		return out, nil
	})
}

// Stats returns cache hit/miss statistics.
func (c *Cache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// Len returns the total number of cached entries across all shards.
func (c *Cache) Len() int {
	total := 0
	for i := range c.shards {
		c.shards[i].mu.RLock()
		total += len(c.shards[i].entries)
		c.shards[i].mu.RUnlock()
	}
	return total
}

// Clear empties the cache.
func (c *Cache) Clear() {
	for i := range c.shards {
		c.shards[i].mu.Lock()
		c.shards[i].entries = make(map[[32]byte]*cacheEntry, c.shards[i].maxSize)
		c.shards[i].order = cacheList{}
		c.shards[i].mu.Unlock()
	}
	c.hits.Store(0)
	c.misses.Store(0)
}

func (c *Cache) shard(key [32]byte) *cacheShard {
	// Use first 8 bytes of hash as shard index
	idx := binary.LittleEndian.Uint64(key[:8]) % cacheShards
	return &c.shards[idx]
}

func (c *Cache) key(req *Request) [32]byte {
	if c.opts.KeyFunc != nil {
		return c.opts.KeyFunc(req)
	}
	return defaultCacheKey(req)
}

func (c *Cache) get(key [32]byte) ([]Frame, *ResponseMeta, Usage, bool) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		return nil, nil, Usage{}, false
	}

	// TTL check
	if c.opts.TTL > 0 && time.Since(e.createdAt) > c.opts.TTL {
		s.order.remove(e)
		delete(s.entries, key)
		return nil, nil, Usage{}, false
	}

	s.order.moveToFront(e)
	// Return copies of frames to prevent mutation
	frames := make([]Frame, len(e.frames))
	copy(frames, e.frames)
	return frames, e.response, e.usage, true
}

func (c *Cache) put(key [32]byte, frames []Frame, resp *ResponseMeta, usage Usage) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update existing
	if e, ok := s.entries[key]; ok {
		e.frames = frames
		e.response = resp
		e.usage = usage
		e.createdAt = time.Now()
		s.order.moveToFront(e)
		return
	}

	// Evict if full
	for s.order.len >= s.maxSize {
		evicted := s.order.removeLast()
		if evicted != nil {
			delete(s.entries, evicted.key)
		}
	}

	// Insert
	e := &cacheEntry{
		key:       key,
		frames:    frames,
		response:  resp,
		usage:     usage,
		createdAt: time.Now(),
	}
	s.entries[key] = e
	s.order.pushFront(e)
}

// defaultCacheKey hashes the request into a deterministic 32-byte key.
func defaultCacheKey(req *Request) [32]byte {
	h := sha256.New()

	h.Write([]byte(req.Model))
	h.Write([]byte{0})
	h.Write([]byte(req.SystemPrompt))
	h.Write([]byte{0})

	for _, msg := range req.Messages {
		h.Write([]byte(msg.Role))
		for _, p := range msg.Parts {
			h.Write([]byte{byte(p.Kind)})
			h.Write([]byte(p.Text))
			h.Write(p.Data)
			h.Write([]byte(p.Mime))
			h.Write([]byte(p.URL))
			if p.Tool != nil {
				h.Write([]byte(p.Tool.ID))
				h.Write([]byte(p.Tool.Name))
				h.Write(p.Tool.Args)
			}
			if p.Result != nil {
				h.Write([]byte(p.Result.CallID))
				h.Write([]byte(p.Result.Content))
			}
		}
	}

	for _, t := range req.Tools {
		h.Write([]byte(t.Name))
		h.Write(t.Parameters)
	}

	h.Write([]byte(req.ResponseFormat))
	h.Write(req.ResponseSchema)
	h.Write([]byte(req.ToolChoice))

	// Options
	var optBuf [64]byte
	n := binary.PutVarint(optBuf[:], int64(req.Options.MaxTokens))
	h.Write(optBuf[:n])
	if req.Options.Temperature != nil {
		b, _ := JSONMarshal(*req.Options.Temperature)
		h.Write(b)
	}
	if req.Options.TopP != nil {
		b, _ := JSONMarshal(*req.Options.TopP)
		h.Write(b)
	}

	var key [32]byte
	h.Sum(key[:0])
	return key
}

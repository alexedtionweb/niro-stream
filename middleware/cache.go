// Package middleware provides provider wrappers: Cache, Retry, Timeout, and Tracing.
package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"ryn.dev/ryn"
)

// Cache is a sharded LRU response cache with TTL for Provider responses.
// Safe for concurrent use by multiple goroutines.
type Cache struct {
	shards [cacheShards]*cacheShard
	opts   CacheOptions
	hits   atomic.Int64
	misses atomic.Int64
}

const cacheShards = 64 // power of 2 for fast modulo

// CacheOptions controls Cache behavior.
type CacheOptions struct {
	// MaxEntries is the maximum total number of entries across all shards.
	// Defaults to 1024 if zero.
	MaxEntries int

	// TTL is how long a cached entry is considered valid.
	// Zero means no expiry.
	TTL time.Duration

	// KeyFn allows custom cache key generation.
	// Defaults to sha256 of (model + messages + tools + response_format).
	KeyFn func(*ryn.Request) [32]byte
}

type cacheShard struct {
	mu      sync.Mutex
	entries map[[32]byte]*cacheEntry
	list    cacheList
	max     int
}

type cacheEntry struct {
	key     [32]byte
	frames  []ryn.Frame
	resp    *ryn.ResponseMeta
	usage   ryn.Usage
	expires time.Time
	prev    *cacheEntry
	next    *cacheEntry
}

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

// NewCache creates a sharded LRU cache.
func NewCache(opts CacheOptions) *Cache {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 1024
	}
	if opts.KeyFn == nil {
		opts.KeyFn = defaultCacheKey
	}
	maxPerShard := max(1, opts.MaxEntries/cacheShards)
	c := &Cache{opts: opts}
	for i := range c.shards {
		c.shards[i] = &cacheShard{
			entries: make(map[[32]byte]*cacheEntry),
			max:     maxPerShard,
		}
	}
	return c
}

// Wrap returns a caching Provider that serves cached responses when available.
func (c *Cache) Wrap(p ryn.Provider) ryn.Provider {
	return ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		key := c.opts.KeyFn(req)

		if frames, resp, usage, ok := c.get(key); ok {
			c.hits.Add(1)
			out, em := ryn.NewStream(len(frames) + 2)
			go func() {
				defer em.Close()
				for _, f := range frames {
					if err := em.Emit(ctx, f); err != nil {
						return
					}
				}
				if resp != nil {
					em.SetResponse(resp)
				}
				if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
					u := usage
					_ = em.Emit(ctx, ryn.UsageFrame(&u))
				}
			}()
			return out, nil
		}
		c.misses.Add(1)

		stream, err := p.Generate(ctx, req)
		if err != nil {
			return nil, err
		}

		// Intercept: collect frames, then serve from memory and store.
		out, em := ryn.NewStream(32)
		go func() {
			defer em.Close()
			var collected []ryn.Frame
			for stream.Next(ctx) {
				f := stream.Frame()
				collected = append(collected, f)
				if err := em.Emit(ctx, f); err != nil {
					return
				}
			}
			if err := stream.Err(); err != nil {
				em.Error(err)
				return
			}
			resp := stream.Response()
			usage := stream.Usage()
			if resp != nil {
				em.SetResponse(resp)
			}
			c.put(key, collected, resp, usage)
		}()
		return out, nil
	})
}

// Stats returns hit and miss counts since the cache was created or last cleared.
func (c *Cache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// Len returns the total number of cached entries.
func (c *Cache) Len() int {
	total := 0
	for _, s := range c.shards {
		s.mu.Lock()
		total += s.list.len
		s.mu.Unlock()
	}
	return total
}

// Clear removes all entries from the cache and resets stats.
func (c *Cache) Clear() {
	for _, s := range c.shards {
		s.mu.Lock()
		s.entries = make(map[[32]byte]*cacheEntry)
		s.list = cacheList{}
		s.mu.Unlock()
	}
	c.hits.Store(0)
	c.misses.Store(0)
}

func (c *Cache) shard(key [32]byte) *cacheShard {
	return c.shards[key[0]%cacheShards]
}

func (c *Cache) get(key [32]byte) ([]ryn.Frame, *ryn.ResponseMeta, ryn.Usage, bool) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil, nil, ryn.Usage{}, false
	}
	if c.opts.TTL > 0 && !e.expires.IsZero() && time.Now().After(e.expires) {
		s.list.remove(e)
		delete(s.entries, key)
		return nil, nil, ryn.Usage{}, false
	}
	s.list.moveToFront(e)
	return e.frames, e.resp, e.usage, true
}

func (c *Cache) put(key [32]byte, frames []ryn.Frame, resp *ryn.ResponseMeta, usage ryn.Usage) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.entries[key]; ok {
		s.list.moveToFront(e)
		e.frames = frames
		e.resp = resp
		e.usage = usage
		if c.opts.TTL > 0 {
			e.expires = time.Now().Add(c.opts.TTL)
		}
		return
	}

	e := &cacheEntry{
		key:    key,
		frames: frames,
		resp:   resp,
		usage:  usage,
	}
	if c.opts.TTL > 0 {
		e.expires = time.Now().Add(c.opts.TTL)
	}
	s.list.pushFront(e)
	s.entries[key] = e

	for s.list.len > s.max {
		evicted := s.list.removeLast()
		if evicted != nil {
			delete(s.entries, evicted.key)
		}
	}
}

func defaultCacheKey(req *ryn.Request) [32]byte {
	h := sha256.New()
	b, _ := json.Marshal(struct {
		Model          string        `json:"m"`
		Messages       []ryn.Message `json:"msgs"`
		Tools          []ryn.Tool    `json:"tools,omitempty"`
		ResponseFormat string        `json:"rf,omitempty"`
	}{
		Model:          req.Model,
		Messages:       req.Messages,
		Tools:          req.Tools,
		ResponseFormat: req.ResponseFormat,
	})
	h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

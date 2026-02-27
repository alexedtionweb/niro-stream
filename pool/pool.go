// Package pool provides a size-class byte buffer pool that eliminates
// allocations on media-heavy hot paths (audio chunks, image tiles, video frames).
//
// Under millions of concurrent LLM calls the GC cost of per-frame
// []byte allocations dominates. BytePool provides O(1) amortised
// Get/Put with no per-operation heap allocation.
package pool

import "sync"

// BytePool is a size-class byte buffer pool.
//
// Usage:
//
//	buf := pool.DefaultBytePool.Get(frameSize)
//	copy(buf, rawPayload)
//	// ... after consumer is done:
//	pool.DefaultBytePool.Put(buf)
type BytePool struct {
	// Size classes: small (≤4KB), medium (≤64KB), large (≤1MB), huge (>1MB)
	small  sync.Pool // ≤4KB  — typical audio chunks (20ms PCM)
	medium sync.Pool // ≤64KB — larger audio, small images
	large  sync.Pool // ≤1MB  — images, video frames
}

const (
	sizeSmall  = 4 << 10  // 4 KB
	sizeMedium = 64 << 10 // 64 KB
	sizeLarge  = 1 << 20  // 1 MB
)

// NewBytePool creates a new BytePool with pre-allocated size classes.
func NewBytePool() *BytePool {
	return &BytePool{
		small: sync.Pool{New: func() any {
			b := make([]byte, 0, sizeSmall)
			return &b
		}},
		medium: sync.Pool{New: func() any {
			b := make([]byte, 0, sizeMedium)
			return &b
		}},
		large: sync.Pool{New: func() any {
			b := make([]byte, 0, sizeLarge)
			return &b
		}},
	}
}

// DefaultBytePool is the process-wide byte pool.
// Providers and processors should use this unless they need isolation.
var DefaultBytePool = NewBytePool()

// Get returns a byte slice with len=size from the appropriate size class.
// The returned slice may have capacity > size.
func (p *BytePool) Get(size int) []byte {
	switch {
	case size <= sizeSmall:
		bp := p.small.Get().(*[]byte)
		b := (*bp)[:size]
		return b
	case size <= sizeMedium:
		bp := p.medium.Get().(*[]byte)
		b := (*bp)[:size]
		return b
	case size <= sizeLarge:
		bp := p.large.Get().(*[]byte)
		b := (*bp)[:size]
		return b
	default:
		// Too large for pooling — allocate directly.
		return make([]byte, size)
	}
}

// Put returns a byte slice to the pool.
// After Put, the caller must not reference the slice.
// Slices larger than the large class are dropped (GC collects them).
func (p *BytePool) Put(b []byte) {
	c := cap(b)
	b = b[:0]
	switch {
	case c >= sizeLarge:
		// Only put back if it fits the large class capacity range.
		if c <= sizeLarge*2 {
			p.large.Put(&b)
		}
		// Oversized: let GC collect.
	case c >= sizeMedium:
		p.medium.Put(&b)
	case c >= sizeSmall:
		p.small.Put(&b)
	}
	// Undersized slices are dropped.
}

package ryn

import (
	"sync"
)

// ─── Byte Buffer Pool ───────────────────────────────────────

// BytePool is a size-class byte buffer pool that eliminates allocations
// on media-heavy hot paths (audio chunks, image tiles, video frames).
//
// Under millions of concurrent LLM calls the GC cost of per-frame
// []byte allocations dominates. BytePool provides O(1) amortized
// Get/Put with no per-operation heap allocation.
//
// Usage in providers:
//
//	buf := ryn.DefaultBytePool.Get(frameSize)
//	copy(buf, rawPayload)
//	frame := ryn.Frame{Kind: ryn.KindAudio, Data: buf, Mime: "audio/pcm"}
//	// ... after consumer is done:
//	ryn.DefaultBytePool.Put(buf)
//
// Or use the convenience constructors:
//
//	frame := ryn.AudioFramePooled(pool, data, "audio/pcm")
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

// NewBytePool creates a new BytePool.
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

// Get returns a byte slice with len=size from the appropriate pool.
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
		// Too large for pooling — allocate directly
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
		// Only put back if it fits the large class capacity range
		if c <= sizeLarge*2 {
			p.large.Put(&b)
		}
		// Oversized: let GC collect
	case c >= sizeMedium:
		p.medium.Put(&b)
	case c >= sizeSmall:
		p.small.Put(&b)
	}
	// Undersized slices are dropped
}

// ─── Pooled Frame Constructors ──────────────────────────────

// AudioFramePooled creates an audio Frame using a pooled buffer.
// The data is copied into a buffer from the pool. The caller
// should call [BytePool.Put] on Frame.Data after consumption.
func AudioFramePooled(pool *BytePool, data []byte, mime string) Frame {
	buf := pool.Get(len(data))
	copy(buf, data)
	return Frame{Kind: KindAudio, Data: buf, Mime: mime}
}

// ImageFramePooled creates an image Frame using a pooled buffer.
func ImageFramePooled(pool *BytePool, data []byte, mime string) Frame {
	buf := pool.Get(len(data))
	copy(buf, data)
	return Frame{Kind: KindImage, Data: buf, Mime: mime}
}

// VideoFramePooled creates a video Frame using a pooled buffer.
func VideoFramePooled(pool *BytePool, data []byte, mime string) Frame {
	buf := pool.Get(len(data))
	copy(buf, data)
	return Frame{Kind: KindVideo, Data: buf, Mime: mime}
}

// ─── Usage Pool ─────────────────────────────────────────────

var usagePool = sync.Pool{
	New: func() any { return &Usage{} },
}

// GetUsage returns a Usage from the pool. Reset to zero values.
// Call PutUsage when done.
func GetUsage() *Usage {
	u := usagePool.Get().(*Usage)
	*u = Usage{}
	return u
}

// PutUsage returns a Usage to the pool.
func PutUsage(u *Usage) {
	if u == nil {
		return
	}
	u.Detail = nil
	usagePool.Put(u)
}

// ─── ResponseMeta Pool ──────────────────────────────────────

var responseMetaPool = sync.Pool{
	New: func() any { return &ResponseMeta{} },
}

// GetResponseMeta returns a ResponseMeta from the pool. Reset to zero.
func GetResponseMeta() *ResponseMeta {
	m := responseMetaPool.Get().(*ResponseMeta)
	*m = ResponseMeta{}
	return m
}

// PutResponseMeta returns a ResponseMeta to the pool.
func PutResponseMeta(m *ResponseMeta) {
	if m == nil {
		return
	}
	m.ProviderMeta = nil
	responseMetaPool.Put(m)
}

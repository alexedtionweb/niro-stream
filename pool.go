package ryn

import (
	"sync"

	"ryn.dev/ryn/pool"
)

// BytePool is an alias for pool.BytePool.
// See ryn.dev/ryn/pool for direct usage without the ryn import.
type BytePool = pool.BytePool

// NewBytePool creates a new BytePool.
func NewBytePool() *BytePool { return pool.NewBytePool() }

// DefaultBytePool is the process-wide byte pool.
// Providers and processors should use this unless they need isolation.
var DefaultBytePool = pool.DefaultBytePool

// ─── Pooled Frame Constructors ──────────────────────────────

// AudioFramePooled creates an audio Frame using a pooled buffer.
// The data is copied into a buffer from the pool. The caller
// should call [BytePool.Put] on Frame.Data after consumption.
func AudioFramePooled(bp *BytePool, data []byte, mime string) Frame {
	buf := bp.Get(len(data))
	copy(buf, data)
	return Frame{Kind: KindAudio, Data: buf, Mime: mime}
}

// ImageFramePooled creates an image Frame using a pooled buffer.
func ImageFramePooled(bp *BytePool, data []byte, mime string) Frame {
	buf := bp.Get(len(data))
	copy(buf, data)
	return Frame{Kind: KindImage, Data: buf, Mime: mime}
}

// VideoFramePooled creates a video Frame using a pooled buffer.
func VideoFramePooled(bp *BytePool, data []byte, mime string) Frame {
	buf := bp.Get(len(data))
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

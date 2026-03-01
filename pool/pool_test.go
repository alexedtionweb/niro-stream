package pool_test

import (
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-stream/pool"
)

func TestBytePoolGetPut(t *testing.T) {
	t.Parallel()
	p := pool.NewBytePool()

	// Small buffer (≤4KB)
	buf := p.Get(100)
	if len(buf) != 100 {
		t.Errorf("small: got len %d, want 100", len(buf))
	}
	if cap(buf) < 100 {
		t.Errorf("small: cap %d < 100", cap(buf))
	}
	p.Put(buf)

	// Medium buffer (≤64KB)
	buf = p.Get(32 * 1024)
	if len(buf) != 32*1024 {
		t.Errorf("medium: got len %d, want 32768", len(buf))
	}
	p.Put(buf)

	// Large buffer (≤1MB)
	buf = p.Get(512 * 1024)
	if len(buf) != 512*1024 {
		t.Errorf("large: got len %d, want 524288", len(buf))
	}
	p.Put(buf)

	// Huge buffer — not pooled, allocated directly
	buf = p.Get(2 * 1024 * 1024)
	if len(buf) != 2*1024*1024 {
		t.Errorf("huge: got len %d, want 2097152", len(buf))
	}
	p.Put(buf)
}

func TestBytePoolConcurrent(t *testing.T) {
	t.Parallel()
	p := pool.NewBytePool()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buf := p.Get(960) // typical 20ms audio chunk
				buf[0] = 42
				p.Put(buf)
			}
		}()
	}
	wg.Wait()
}

func TestDefaultBytePool(t *testing.T) {
	t.Parallel()
	if pool.DefaultBytePool == nil {
		t.Fatal("DefaultBytePool is nil")
	}
	buf := pool.DefaultBytePool.Get(512)
	if len(buf) != 512 {
		t.Errorf("got len %d, want 512", len(buf))
	}
	pool.DefaultBytePool.Put(buf)
}

func TestBytePoolPutNilSafe(t *testing.T) {
	t.Parallel()
	p := pool.NewBytePool()
	// Undersized slice — should be dropped silently
	p.Put([]byte{})
	p.Put(make([]byte, 0, 1))
}

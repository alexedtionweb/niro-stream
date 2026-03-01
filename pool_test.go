package niro_test

import (
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestBytePoolGetPut(t *testing.T) {
	t.Parallel()
	pool := niro.NewBytePool()

	// Small buffer
	buf := pool.Get(100)
	assertEqual(t, len(buf), 100)
	assertTrue(t, cap(buf) >= 100)
	pool.Put(buf)

	// Medium buffer
	buf = pool.Get(32 * 1024)
	assertEqual(t, len(buf), 32*1024)
	pool.Put(buf)

	// Large buffer
	buf = pool.Get(512 * 1024)
	assertEqual(t, len(buf), 512*1024)
	pool.Put(buf)

	// Huge buffer (not pooled)
	buf = pool.Get(2 * 1024 * 1024)
	assertEqual(t, len(buf), 2*1024*1024)
	pool.Put(buf)
}

func TestBytePoolConcurrent(t *testing.T) {
	t.Parallel()
	pool := niro.NewBytePool()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buf := pool.Get(960) // typical audio chunk
				buf[0] = 42
				pool.Put(buf)
			}
		}()
	}
	wg.Wait()
}

func TestPooledFrameConstructors(t *testing.T) {
	t.Parallel()
	pool := niro.NewBytePool()

	data := []byte{1, 2, 3, 4, 5}

	af := niro.AudioFramePooled(pool, data, "audio/pcm")
	assertEqual(t, af.Kind, niro.KindAudio)
	assertEqual(t, len(af.Data), 5)
	assertEqual(t, af.Data[0], byte(1))
	assertEqual(t, af.Mime, "audio/pcm")
	pool.Put(af.Data)

	imgF := niro.ImageFramePooled(pool, data, "image/png")
	assertEqual(t, imgF.Kind, niro.KindImage)
	pool.Put(imgF.Data)

	vf := niro.VideoFramePooled(pool, data, "video/mp4")
	assertEqual(t, vf.Kind, niro.KindVideo)
	pool.Put(vf.Data)
}

func TestUsagePool(t *testing.T) {
	t.Parallel()
	u := niro.GetUsage()
	assertEqual(t, u.InputTokens, 0)
	assertEqual(t, u.OutputTokens, 0)

	u.InputTokens = 100
	u.OutputTokens = 50
	u.Detail = map[string]int{"cached": 10}
	niro.PutUsage(u)

	// After put, getting a new one should be zeroed
	u2 := niro.GetUsage()
	assertEqual(t, u2.InputTokens, 0)
	assertTrue(t, u2.Detail == nil)
	niro.PutUsage(u2)
}

func TestResponseMetaPool(t *testing.T) {
	t.Parallel()
	m := niro.GetResponseMeta()
	assertEqual(t, m.Model, "")
	assertEqual(t, m.ID, "")

	m.Model = "gpt-4o"
	m.ProviderMeta = map[string]any{"x": 1}
	niro.PutResponseMeta(m)

	m2 := niro.GetResponseMeta()
	assertEqual(t, m2.Model, "")
	assertTrue(t, m2.ProviderMeta == nil)
	niro.PutResponseMeta(m2)
}

func TestUsageReset(t *testing.T) {
	t.Parallel()
	u := niro.Usage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		Detail:       map[string]int{"cached": 10},
	}
	u.Reset()
	assertEqual(t, u.InputTokens, 0)
	assertEqual(t, u.OutputTokens, 0)
	assertEqual(t, u.TotalTokens, 0)
	assertEqual(t, len(u.Detail), 0) // map cleared, not nil
}

func TestPutUsageNil(t *testing.T) {
	t.Parallel()
	// PutUsage(nil) should be a no-op and not panic.
	niro.PutUsage(nil)
}

func TestPutResponseMetaNil(t *testing.T) {
	t.Parallel()
	// PutResponseMeta(nil) should be a no-op and not panic.
	niro.PutResponseMeta(nil)
}

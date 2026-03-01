package registry_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/registry"
)

func TestRegistryBasic(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello")}), nil
	})

	reg.Register("test", mock)
	assertTrue(t, reg.Has("test"))
	assertEqual(t, reg.Len(), 1)

	p, err := reg.Get("test")
	assertNoError(t, err)
	assertNotNil(t, p)
}

func TestRegistryNotFound(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	_, err := reg.Get("missing")
	assertErrorContains(t, err, "not registered")
}

func TestRegistryMustGetPanics(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	defer func() {
		r := recover()
		assertNotNil(t, r)
	}()
	reg.MustGet("missing")
}

func TestRegistryRemove(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	})

	reg.Register("x", mock)
	assertTrue(t, reg.Has("x"))
	reg.Remove("x")
	assertEqual(t, reg.Has("x"), false)
}

func TestRegistryAllAndNames(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	for _, name := range []string{"a", "b", "c"} {
		n := name
		reg.Register(n, ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
			return nil, nil
		}))
	}

	all := reg.All()
	assertEqual(t, len(all), 3)
	names := reg.Names()
	assertEqual(t, len(names), 3)
}

func TestRegistryGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("mock", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("from registry")}), nil
	}))

	s, err := reg.Generate(ctx, "mock", &ryn.Request{Messages: []ryn.Message{ryn.UserText("hi")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "from registry")
}

func TestRegistryGenerateNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	_, err := reg.Generate(ctx, "nope", &ryn.Request{})
	assertErrorContains(t, err, "not registered")
}

func TestRegistryMustGetSuccess(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	reg.Register("x", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, nil
	}))
	p := reg.MustGet("x")
	assertNotNil(t, p)
}

func TestRegistryConcurrent(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("p%d", i%10)
			reg.Register(name, ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
				return nil, nil
			}))
			reg.Has(name)
			reg.Get(name)
			reg.Names()
		}()
	}
	wg.Wait()
}

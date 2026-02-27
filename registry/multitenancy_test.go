package registry_test

import (
	"context"
	"fmt"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/registry"
)

func TestMultiTenantProviderSelectByRequestClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("tenant-a", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("A")}), nil
	}))
	reg.Register("tenant-b", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("B")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg)

	s, err := router.Generate(ctx, &ryn.Request{
		Client:   "tenant-b",
		Messages: []ryn.Message{ryn.UserText("hi")},
	})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "B")
}

func TestMultiTenantProviderContextAndDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("default", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("default")}), nil
	}))
	reg.Register("ctx-client", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("ctx")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("default"))

	ctxReq := registry.WithClient(ctx, "ctx-client")
	s1, err := router.Generate(ctxReq, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
	assertNoError(t, err)
	text1, _ := ryn.CollectText(ctx, s1)
	assertEqual(t, text1, "ctx")

	s2, err := router.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
	assertNoError(t, err)
	text2, _ := ryn.CollectText(ctx, s2)
	assertEqual(t, text2, "default")
}

func TestMultiTenantProviderMutatorClonesRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("tenant", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s := ""
		if v, ok := req.Extra.(string); ok {
			s = v
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(s)}), nil
	}))

	router := registry.NewMultiTenantProvider(reg,
		registry.WithDefaultClient("tenant"),
		registry.WithClientMutator("tenant", func(ctx context.Context, req *ryn.Request) error {
			req.Extra = "tenant-auth"
			return nil
		}),
	)

	original := &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}}
	s, err := router.Generate(ctx, original)
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "tenant-auth")

	assertEqual(t, original.Extra, nil)
}

func TestMultiTenantProviderSelectorAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("x", ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("x")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithClientSelector(func(ctx context.Context, req *ryn.Request) (string, error) {
		return "x", nil
	}))

	s, err := router.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
	assertNoError(t, err)
	text, _ := ryn.CollectText(ctx, s)
	assertEqual(t, text, "x")

	fail := registry.NewMultiTenantProvider(reg, registry.WithClientSelector(func(ctx context.Context, req *ryn.Request) (string, error) {
		return "", fmt.Errorf("selector failed")
	}))
	_, err = fail.Generate(ctx, &ryn.Request{Messages: []ryn.Message{ryn.UserText("x")}})
	assertErrorContains(t, err, "selector failed")
}

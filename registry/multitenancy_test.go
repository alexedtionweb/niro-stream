package registry_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/registry"
)

func TestMultiTenantProviderSelectByRequestClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("tenant-a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("A")}), nil
	}))
	reg.Register("tenant-b", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("B")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg)

	s, err := router.Generate(ctx, &niro.Request{
		Client:   "tenant-b",
		Messages: []niro.Message{niro.UserText("hi")},
	})
	assertNoError(t, err)
	text, _ := niro.CollectText(ctx, s)
	assertEqual(t, text, "B")
}

func TestMultiTenantProviderContextAndDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("default", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("default")}), nil
	}))
	reg.Register("ctx-client", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ctx")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("default"))

	ctxReq := registry.WithClient(ctx, "ctx-client")
	s1, err := router.Generate(ctxReq, &niro.Request{Messages: []niro.Message{niro.UserText("x")}})
	assertNoError(t, err)
	text1, _ := niro.CollectText(ctx, s1)
	assertEqual(t, text1, "ctx")

	s2, err := router.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("x")}})
	assertNoError(t, err)
	text2, _ := niro.CollectText(ctx, s2)
	assertEqual(t, text2, "default")
}

func TestMultiTenantProviderMutatorClonesRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("tenant", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s := ""
		if v, ok := req.Extra.(string); ok {
			s = v
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(s)}), nil
	}))

	router := registry.NewMultiTenantProvider(reg,
		registry.WithDefaultClient("tenant"),
		registry.WithClientMutator("tenant", func(ctx context.Context, req *niro.Request) error {
			req.Extra = "tenant-auth"
			return nil
		}),
	)

	original := &niro.Request{Messages: []niro.Message{niro.UserText("x")}}
	s, err := router.Generate(ctx, original)
	assertNoError(t, err)
	text, _ := niro.CollectText(ctx, s)
	assertEqual(t, text, "tenant-auth")

	assertEqual(t, original.Extra, nil)
}

func TestMultiTenantProviderSelectorAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("x", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("x")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithClientSelector(func(ctx context.Context, req *niro.Request) (string, error) {
		return "x", nil
	}))

	s, err := router.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("x")}})
	assertNoError(t, err)
	text, _ := niro.CollectText(ctx, s)
	assertEqual(t, text, "x")

	fail := registry.NewMultiTenantProvider(reg, registry.WithClientSelector(func(ctx context.Context, req *niro.Request) (string, error) {
		return "", fmt.Errorf("selector failed")
	}))
	_, err = fail.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("x")}})
	assertErrorContains(t, err, "selector failed")
}

func TestMultiTenantNilRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("a"))
	_, err := router.Generate(ctx, nil)
	assertErrorContains(t, err, "cannot be nil")
}

func TestSingleProviderAutoSelect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("only", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("auto")}), nil
	}))

	// No default, no selector, no req.Client — but only 1 provider, so auto-select.
	router := registry.NewMultiTenantProvider(reg)
	s, err := router.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertNoError(t, err)
	text, _ := niro.CollectText(ctx, s)
	assertEqual(t, text, "auto")
}

func TestNoClientSelectedError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, nil
	}))
	reg.Register("b", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, nil
	}))

	// 2 providers, no client specified anywhere.
	router := registry.NewMultiTenantProvider(reg)
	_, err := router.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertErrorContains(t, err, "no client selected")
}

func TestMutatorError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg,
		registry.WithDefaultClient("a"),
		registry.WithClientMutator("a", func(ctx context.Context, req *niro.Request) error {
			return fmt.Errorf("mutator boom")
		}),
	)
	_, err := router.Generate(ctx, &niro.Request{Messages: []niro.Message{niro.UserText("hi")}})
	assertErrorContains(t, err, "mutator boom")
}

func TestCloneRequestWithExtras(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var got *niro.Request
	reg := registry.New()
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		got = req
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("a"))

	tools := []niro.Tool{{Name: "calc", Description: "calculator"}}
	schema := []byte(`{"type":"object"}`)
	stop := []string{"stop1", "stop2"}

	orig := &niro.Request{
		Messages:       []niro.Message{niro.UserText("hi")},
		Tools:          tools,
		ResponseSchema: schema,
		Options:        niro.Options{Stop: stop},
	}
	s, err := router.Generate(ctx, orig)
	assertNoError(t, err)
	niro.CollectText(ctx, s)

	// Cloned request should have copies of slices, not the same backing arrays.
	assertNotNil(t, got)
	assertEqual(t, len(got.Tools), 1)
	assertEqual(t, string(got.ResponseSchema), `{"type":"object"}`)
	assertEqual(t, len(got.Options.Stop), 2)
}

func TestCloneMessageWithData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var got *niro.Request
	reg := registry.New()
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		got = req
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("a"))

	// Build a message with binary data (image), tool call with args, and tool result.
	imgPart := niro.ImagePart([]byte{0x89, 0x50}, "image/png")
	toolCall := &niro.ToolCall{ID: "call1", Name: "fn", Args: []byte(`{"x":1}`)}
	toolCallPart := niro.ToolCallPart(toolCall)
	toolResult := &niro.ToolResult{CallID: "call1", Content: "42"}
	toolResultPart := niro.ToolResultPart(toolResult)

	msg := niro.Multi(niro.RoleUser, imgPart, toolCallPart, toolResultPart)

	orig := &niro.Request{Messages: []niro.Message{msg}}
	s, err := router.Generate(ctx, orig)
	assertNoError(t, err)
	niro.CollectText(ctx, s)

	assertNotNil(t, got)
	assertEqual(t, len(got.Messages[0].Parts), 3)
	// Image data should be copied.
	assertEqual(t, len(got.Messages[0].Parts[0].Data), 2)
	// Tool call should be cloned.
	assertNotNil(t, got.Messages[0].Parts[1].Tool)
	assertEqual(t, got.Messages[0].Parts[1].Tool.Name, "fn")
	// Tool result should be cloned.
	assertNotNil(t, got.Messages[0].Parts[2].Result)
	assertEqual(t, got.Messages[0].Parts[2].Result.CallID, "call1")
}

func TestCloneRequestDeepCopiesOptionPointers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	temp := 0.7
	topP := 0.9
	topK := 40
	freq := 0.1
	pres := 0.2

	req := &niro.Request{
		Model:    "m",
		Messages: []niro.Message{niro.UserText("hi")},
		Options: niro.Options{
			Temperature:      &temp,
			TopP:             &topP,
			TopK:             &topK,
			FrequencyPenalty: &freq,
			PresencePenalty:  &pres,
		},
	}

	reg := registry.New()
	var cloned *niro.Request
	reg.Register("a", niro.ProviderFunc(func(ctx context.Context, r *niro.Request) (*niro.Stream, error) {
		cloned = r
		// Mutate the cloned request's pointer fields in-place.
		*r.Options.Temperature = 0.1
		*r.Options.TopP = 0.1
		*r.Options.TopK = 1
		*r.Options.FrequencyPenalty = 0.1
		*r.Options.PresencePenalty = 0.1
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	}))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("a"))
	s, err := router.Generate(ctx, req)
	assertNoError(t, err)
	niro.CollectText(ctx, s)
	assertNotNil(t, cloned)

	// Original request's pointer fields must be unchanged.
	assertEqual(t, *req.Options.Temperature, 0.7)
	assertEqual(t, *req.Options.TopP, 0.9)
	assertEqual(t, *req.Options.TopK, 40)
	assertEqual(t, *req.Options.FrequencyPenalty, 0.1)
	assertEqual(t, *req.Options.PresencePenalty, 0.2)
}

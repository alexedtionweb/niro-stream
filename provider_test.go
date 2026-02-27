package ryn_test

import (
	"context"
	"testing"

	"ryn.dev/ryn"
)

func TestProviderFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		s, e := ryn.NewStream(2)
		go func() {
			defer e.Close()
			e.Emit(ctx, ryn.TextFrame("mock: "+req.Messages[0].Parts[0].Text))
		}()
		return s, nil
	})

	stream, err := mock.Generate(ctx, &ryn.Request{
		Messages: []ryn.Message{ryn.UserText("ping")},
	})
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "mock: ping")
}

func TestRequestEffectiveMessages(t *testing.T) {
	t.Parallel()

	t.Run("NoSystemPrompt", func(t *testing.T) {
		req := &ryn.Request{
			Messages: []ryn.Message{ryn.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 1)
	})

	t.Run("WithSystemPrompt", func(t *testing.T) {
		req := &ryn.Request{
			SystemPrompt: "be helpful",
			Messages:     []ryn.Message{ryn.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 2)
		assertEqual(t, msgs[0].Role, ryn.RoleSystem)
		assertEqual(t, msgs[0].Parts[0].Text, "be helpful")
	})
}

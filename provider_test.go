package niro_test

import (
	"context"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestProviderFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		s, e := niro.NewStream(2)
		go func() {
			defer e.Close()
			e.Emit(ctx, niro.TextFrame("mock: "+req.Messages[0].Parts[0].Text))
		}()
		return s, nil
	})

	stream, err := mock.Generate(ctx, &niro.Request{
		Messages: []niro.Message{niro.UserText("ping")},
	})
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, stream)
	assertNoError(t, err)
	assertEqual(t, text, "mock: ping")
}

func TestRequestEffectiveMessages(t *testing.T) {
	t.Parallel()

	t.Run("NoSystemPrompt", func(t *testing.T) {
		req := &niro.Request{
			Messages: []niro.Message{niro.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 1)
	})

	t.Run("WithSystemPrompt", func(t *testing.T) {
		req := &niro.Request{
			SystemPrompt: "be helpful",
			Messages:     []niro.Message{niro.UserText("hi")},
		}
		msgs := req.EffectiveMessages()
		assertEqual(t, len(msgs), 2)
		assertEqual(t, msgs[0].Role, niro.RoleSystem)
		assertEqual(t, msgs[1].Role, niro.RoleUser)
	})
}

func TestOptionHelpers(t *testing.T) {
	t.Parallel()

	temp := niro.Temp(0.7)
	assertNotNil(t, temp)
	assertEqual(t, *temp, 0.7)

	topP := niro.TopPVal(0.9)
	assertNotNil(t, topP)
	assertEqual(t, *topP, 0.9)

	topK := niro.TopKVal(40)
	assertNotNil(t, topK)
	assertEqual(t, *topK, 40)
}

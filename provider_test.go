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
		assertEqual(t, msgs[1].Role, ryn.RoleUser)
	})
}

func TestOptionHelpers(t *testing.T) {
	t.Parallel()

	temp := ryn.Temp(0.7)
	assertNotNil(t, temp)
	assertEqual(t, *temp, 0.7)

	topP := ryn.TopPVal(0.9)
	assertNotNil(t, topP)
	assertEqual(t, *topP, 0.9)

	topK := ryn.TopKVal(40)
	assertNotNil(t, topK)
	assertEqual(t, *topK, 40)
}

func TestCostAdd(t *testing.T) {
	t.Parallel()

	c := ryn.Cost{InputCost: 1.0, OutputCost: 2.0, TotalCost: 3.0, Currency: "USD"}
	c.Add(ryn.Cost{InputCost: 0.5, OutputCost: 1.0, TotalCost: 1.5})
	assertEqual(t, c.InputCost, 1.5)
	assertEqual(t, c.OutputCost, 3.0)
	assertEqual(t, c.TotalCost, 4.5)

	// nil receiver is safe
	var nilCost *ryn.Cost
	nilCost.Add(ryn.Cost{InputCost: 1.0})
}

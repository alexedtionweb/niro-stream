package tools_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/tools"
)

func TestGenerateInputStreamFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	provider := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		if len(req.Messages) == 0 {
			return nil, fmt.Errorf("no messages")
		}
		last := req.Messages[len(req.Messages)-1]
		var text string
		for _, p := range last.Parts {
			if p.Kind == ryn.KindText {
				text += p.Text
			}
		}
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("echo:" + text)}), nil
	})

	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("hello "), ryn.TextFrame("world")})
	out, err := tools.GenerateInputStream(ctx, provider, &ryn.Request{}, in, tools.DefaultInputStreamOptions())
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "echo:hello world")
}

func TestGenerateInputStreamNative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := &nativeInputProviderMock{}
	in := ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame("a")})
	out, err := tools.GenerateInputStream(ctx, p, &ryn.Request{}, in, tools.DefaultInputStreamOptions())
	assertNoError(t, err)

	text, err := ryn.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "native")
	assertTrue(t, p.called)
}

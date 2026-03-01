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

	provider := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		if len(req.Messages) == 0 {
			return nil, fmt.Errorf("no messages")
		}
		last := req.Messages[len(req.Messages)-1]
		var text string
		for _, p := range last.Parts {
			if p.Kind == niro.KindText {
				text += p.Text
			}
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("echo:" + text)}), nil
	})

	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("hello "), niro.TextFrame("world")})
	out, err := tools.GenerateInputStream(ctx, provider, &niro.Request{}, in, tools.DefaultInputStreamOptions())
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "echo:hello world")
}

func TestGenerateInputStreamNative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := &nativeInputProviderMock{}
	in := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("a")})
	out, err := tools.GenerateInputStream(ctx, p, &niro.Request{}, in, tools.DefaultInputStreamOptions())
	assertNoError(t, err)

	text, err := niro.CollectText(ctx, out)
	assertNoError(t, err)
	assertEqual(t, text, "native")
	assertTrue(t, p.called)
}

package niro_test

import (
	"encoding/json"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestFrameConstructors(t *testing.T) {
	t.Parallel()

	t.Run("TextFrame", func(t *testing.T) {
		f := niro.TextFrame("hello")
		assertEqual(t, f.Kind, niro.KindText)
		assertEqual(t, f.Text, "hello")
	})

	t.Run("AudioFrame", func(t *testing.T) {
		f := niro.AudioFrame([]byte{1, 2}, "audio/pcm")
		assertEqual(t, f.Kind, niro.KindAudio)
		assertEqual(t, len(f.Data), 2)
		assertEqual(t, f.Mime, "audio/pcm")
	})

	t.Run("ImageFrame", func(t *testing.T) {
		f := niro.ImageFrame([]byte{0xFF}, "image/png")
		assertEqual(t, f.Kind, niro.KindImage)
	})

	t.Run("VideoFrame", func(t *testing.T) {
		f := niro.VideoFrame([]byte{0x00, 0x01}, "video/mp4")
		assertEqual(t, f.Kind, niro.KindVideo)
		assertEqual(t, f.Mime, "video/mp4")
		assertEqual(t, len(f.Data), 2)
	})

	t.Run("ToolCallFrame", func(t *testing.T) {
		tc := &niro.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		f := niro.ToolCallFrame(tc)
		assertEqual(t, f.Kind, niro.KindToolCall)
		assertEqual(t, f.Tool.Name, "fn")
	})

	t.Run("ToolResultFrame", func(t *testing.T) {
		tr := &niro.ToolResult{CallID: "c1", Content: "ok"}
		f := niro.ToolResultFrame(tr)
		assertEqual(t, f.Kind, niro.KindToolResult)
		assertEqual(t, f.Result.Content, "ok")
	})

	t.Run("UsageFrame", func(t *testing.T) {
		u := &niro.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
		f := niro.UsageFrame(u)
		assertEqual(t, f.Kind, niro.KindUsage)
		assertEqual(t, f.Usage.TotalTokens, 30)
	})

	t.Run("ControlFrame", func(t *testing.T) {
		f := niro.ControlFrame(niro.SignalFlush)
		assertEqual(t, f.Kind, niro.KindControl)
		assertEqual(t, f.Signal, niro.SignalFlush)
	})

	t.Run("CustomFrame", func(t *testing.T) {
		c := &niro.ExperimentalFrame{Type: "reasoning_summary", Data: "condensed trace"}
		f := niro.CustomFrame(c)
		assertEqual(t, f.Kind, niro.KindCustom)
		assertEqual(t, f.Custom.Type, "reasoning_summary")
	})
}

func TestKindString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind niro.Kind
		want string
	}{
		{niro.KindText, "text"},
		{niro.KindAudio, "audio"},
		{niro.KindImage, "image"},
		{niro.KindVideo, "video"},
		{niro.KindToolCall, "tool_call"},
		{niro.KindToolResult, "tool_result"},
		{niro.KindUsage, "usage"},
		{niro.KindCustom, "custom"},
		{niro.KindControl, "control"},
		{niro.Kind(0), "unknown"},
	}
	for _, tc := range cases {
		assertEqual(t, tc.kind.String(), tc.want)
	}
}

func TestSignalString(t *testing.T) {
	t.Parallel()
	assertEqual(t, niro.SignalFlush.String(), "flush")
	assertEqual(t, niro.SignalEOT.String(), "eot")
	assertEqual(t, niro.SignalAbort.String(), "abort")
	assertEqual(t, niro.SignalNone.String(), "none")
	assertEqual(t, niro.Signal(99).String(), "none") // default case
}

func TestUsageAdd(t *testing.T) {
	t.Parallel()
	u := niro.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	u.Add(&niro.Usage{
		InputTokens:  20,
		OutputTokens: 10,
		TotalTokens:  30,
		Detail:       map[string]int{"cached": 5},
	})
	assertEqual(t, u.InputTokens, 30)
	assertEqual(t, u.OutputTokens, 15)
	assertEqual(t, u.TotalTokens, 45)
	assertEqual(t, u.Detail["cached"], 5)

	// Add with detail again
	u.Add(&niro.Usage{Detail: map[string]int{"cached": 3, niro.UsageReasoningTokens: 10}})
	assertEqual(t, u.Detail["cached"], 8)
	assertEqual(t, u.Detail[niro.UsageReasoningTokens], 10)

	// Add nil is safe
	u.Add(nil)
	assertEqual(t, u.InputTokens, 30)
}

func TestToolChoice(t *testing.T) {
	t.Parallel()
	assertEqual(t, string(niro.ToolChoiceAuto), "auto")
	assertEqual(t, string(niro.ToolChoiceNone), "none")
	assertEqual(t, string(niro.ToolChoiceRequired), "required")
	assertEqual(t, string(niro.ToolChoiceFunc("weather")), "func:weather")
}

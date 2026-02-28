package ryn_test

import (
	"encoding/json"
	"testing"

	"ryn.dev/ryn"
)

func TestFrameConstructors(t *testing.T) {
	t.Parallel()

	t.Run("TextFrame", func(t *testing.T) {
		f := ryn.TextFrame("hello")
		assertEqual(t, f.Kind, ryn.KindText)
		assertEqual(t, f.Text, "hello")
	})

	t.Run("AudioFrame", func(t *testing.T) {
		f := ryn.AudioFrame([]byte{1, 2}, "audio/pcm")
		assertEqual(t, f.Kind, ryn.KindAudio)
		assertEqual(t, len(f.Data), 2)
		assertEqual(t, f.Mime, "audio/pcm")
	})

	t.Run("ImageFrame", func(t *testing.T) {
		f := ryn.ImageFrame([]byte{0xFF}, "image/png")
		assertEqual(t, f.Kind, ryn.KindImage)
	})

	t.Run("VideoFrame", func(t *testing.T) {
		f := ryn.VideoFrame([]byte{0x00, 0x01}, "video/mp4")
		assertEqual(t, f.Kind, ryn.KindVideo)
		assertEqual(t, f.Mime, "video/mp4")
		assertEqual(t, len(f.Data), 2)
	})

	t.Run("ToolCallFrame", func(t *testing.T) {
		tc := &ryn.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		f := ryn.ToolCallFrame(tc)
		assertEqual(t, f.Kind, ryn.KindToolCall)
		assertEqual(t, f.Tool.Name, "fn")
	})

	t.Run("ToolResultFrame", func(t *testing.T) {
		tr := &ryn.ToolResult{CallID: "c1", Content: "ok"}
		f := ryn.ToolResultFrame(tr)
		assertEqual(t, f.Kind, ryn.KindToolResult)
		assertEqual(t, f.Result.Content, "ok")
	})

	t.Run("UsageFrame", func(t *testing.T) {
		u := &ryn.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
		f := ryn.UsageFrame(u)
		assertEqual(t, f.Kind, ryn.KindUsage)
		assertEqual(t, f.Usage.TotalTokens, 30)
	})

	t.Run("ControlFrame", func(t *testing.T) {
		f := ryn.ControlFrame(ryn.SignalFlush)
		assertEqual(t, f.Kind, ryn.KindControl)
		assertEqual(t, f.Signal, ryn.SignalFlush)
	})
}

func TestKindString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind ryn.Kind
		want string
	}{
		{ryn.KindText, "text"},
		{ryn.KindAudio, "audio"},
		{ryn.KindImage, "image"},
		{ryn.KindVideo, "video"},
		{ryn.KindToolCall, "tool_call"},
		{ryn.KindToolResult, "tool_result"},
		{ryn.KindUsage, "usage"},
		{ryn.KindControl, "control"},
		{ryn.Kind(0), "unknown"},
	}
	for _, tc := range cases {
		assertEqual(t, tc.kind.String(), tc.want)
	}
}

func TestSignalString(t *testing.T) {
	t.Parallel()
	assertEqual(t, ryn.SignalFlush.String(), "flush")
	assertEqual(t, ryn.SignalEOT.String(), "eot")
	assertEqual(t, ryn.SignalAbort.String(), "abort")
	assertEqual(t, ryn.SignalNone.String(), "none")
	assertEqual(t, ryn.Signal(99).String(), "none") // default case
}

func TestUsageAdd(t *testing.T) {
	t.Parallel()
	u := ryn.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	u.Add(&ryn.Usage{
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
	u.Add(&ryn.Usage{Detail: map[string]int{"cached": 3, "reasoning": 10}})
	assertEqual(t, u.Detail["cached"], 8)
	assertEqual(t, u.Detail["reasoning"], 10)

	// Add nil is safe
	u.Add(nil)
	assertEqual(t, u.InputTokens, 30)
}

func TestToolChoice(t *testing.T) {
	t.Parallel()
	assertEqual(t, string(ryn.ToolChoiceAuto), "auto")
	assertEqual(t, string(ryn.ToolChoiceNone), "none")
	assertEqual(t, string(ryn.ToolChoiceRequired), "required")
	assertEqual(t, string(ryn.ToolChoiceFunc("weather")), "func:weather")
}

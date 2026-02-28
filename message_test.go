package ryn_test

import (
	"encoding/json"
	"testing"

	"ryn.dev/ryn"
)

func TestMessageConstructors(t *testing.T) {
	t.Parallel()

	t.Run("UserText", func(t *testing.T) {
		m := ryn.UserText("hi")
		assertEqual(t, m.Role, ryn.RoleUser)
		assertEqual(t, len(m.Parts), 1)
		assertEqual(t, m.Parts[0].Text, "hi")
	})

	t.Run("SystemText", func(t *testing.T) {
		m := ryn.SystemText("be brief")
		assertEqual(t, m.Role, ryn.RoleSystem)
		assertEqual(t, m.Parts[0].Text, "be brief")
	})

	t.Run("AssistantText", func(t *testing.T) {
		m := ryn.AssistantText("ok")
		assertEqual(t, m.Role, ryn.RoleAssistant)
		assertEqual(t, m.Parts[0].Text, "ok")
	})

	t.Run("ToolMessage", func(t *testing.T) {
		m := ryn.ToolMessage("call1", `{"temp":72}`)
		assertEqual(t, m.Role, ryn.RoleTool)
		assertEqual(t, m.Parts[0].Result.CallID, "call1")
		assertEqual(t, m.Parts[0].Result.IsError, false)
	})

	t.Run("ToolErrorMessage", func(t *testing.T) {
		m := ryn.ToolErrorMessage("call2", "failed")
		assertEqual(t, m.Parts[0].Result.IsError, true)
	})

	t.Run("Multi", func(t *testing.T) {
		m := ryn.Multi(ryn.RoleUser,
			ryn.TextPart("check this image"),
			ryn.ImageURLPart("https://example.com/img.png", "image/png"),
		)
		assertEqual(t, len(m.Parts), 2)
		assertEqual(t, m.Parts[1].URL, "https://example.com/img.png")
	})
}

func TestPartConstructors(t *testing.T) {
	t.Parallel()

	t.Run("ImagePart", func(t *testing.T) {
		p := ryn.ImagePart([]byte{0xFF, 0xD8}, "image/jpeg")
		assertEqual(t, p.Kind, ryn.KindImage)
		assertEqual(t, p.Mime, "image/jpeg")
		assertEqual(t, len(p.Data), 2)
	})

	t.Run("AudioPart", func(t *testing.T) {
		p := ryn.AudioPart([]byte{0x01}, "audio/pcm")
		assertEqual(t, p.Kind, ryn.KindAudio)
		assertEqual(t, p.Mime, "audio/pcm")
	})

	t.Run("VideoPart", func(t *testing.T) {
		p := ryn.VideoPart([]byte{0x00}, "video/mp4")
		assertEqual(t, p.Kind, ryn.KindVideo)
		assertEqual(t, p.Mime, "video/mp4")
	})

	t.Run("ToolCallPart", func(t *testing.T) {
		call := &ryn.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		p := ryn.ToolCallPart(call)
		assertEqual(t, p.Kind, ryn.KindToolCall)
		assertEqual(t, p.Tool.Name, "fn")
	})

	t.Run("ToolResultPart", func(t *testing.T) {
		result := &ryn.ToolResult{CallID: "c1", Content: "success"}
		p := ryn.ToolResultPart(result)
		assertEqual(t, p.Kind, ryn.KindToolResult)
		assertEqual(t, p.Result.Content, "success")
	})
}

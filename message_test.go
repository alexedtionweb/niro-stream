package niro_test

import (
	"encoding/json"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestMessageConstructors(t *testing.T) {
	t.Parallel()

	t.Run("UserText", func(t *testing.T) {
		m := niro.UserText("hi")
		assertEqual(t, m.Role, niro.RoleUser)
		assertEqual(t, len(m.Parts), 1)
		assertEqual(t, m.Parts[0].Text, "hi")
	})

	t.Run("SystemText", func(t *testing.T) {
		m := niro.SystemText("be brief")
		assertEqual(t, m.Role, niro.RoleSystem)
		assertEqual(t, m.Parts[0].Text, "be brief")
	})

	t.Run("AssistantText", func(t *testing.T) {
		m := niro.AssistantText("ok")
		assertEqual(t, m.Role, niro.RoleAssistant)
		assertEqual(t, m.Parts[0].Text, "ok")
	})

	t.Run("ToolMessage", func(t *testing.T) {
		m := niro.ToolMessage("call1", `{"temp":72}`)
		assertEqual(t, m.Role, niro.RoleTool)
		assertEqual(t, m.Parts[0].Result.CallID, "call1")
		assertEqual(t, m.Parts[0].Result.IsError, false)
	})

	t.Run("ToolErrorMessage", func(t *testing.T) {
		m := niro.ToolErrorMessage("call2", "failed")
		assertEqual(t, m.Parts[0].Result.IsError, true)
	})

	t.Run("Multi", func(t *testing.T) {
		m := niro.Multi(niro.RoleUser,
			niro.TextPart("check this image"),
			niro.ImageURLPart("https://example.com/img.png", "image/png"),
		)
		assertEqual(t, len(m.Parts), 2)
		assertEqual(t, m.Parts[1].URL, "https://example.com/img.png")
	})
}

func TestPartConstructors(t *testing.T) {
	t.Parallel()

	t.Run("ImagePart", func(t *testing.T) {
		p := niro.ImagePart([]byte{0xFF, 0xD8}, "image/jpeg")
		assertEqual(t, p.Kind, niro.KindImage)
		assertEqual(t, p.Mime, "image/jpeg")
		assertEqual(t, len(p.Data), 2)
	})

	t.Run("AudioPart", func(t *testing.T) {
		p := niro.AudioPart([]byte{0x01}, "audio/pcm")
		assertEqual(t, p.Kind, niro.KindAudio)
		assertEqual(t, p.Mime, "audio/pcm")
	})

	t.Run("VideoPart", func(t *testing.T) {
		p := niro.VideoPart([]byte{0x00}, "video/mp4")
		assertEqual(t, p.Kind, niro.KindVideo)
		assertEqual(t, p.Mime, "video/mp4")
	})

	t.Run("ToolCallPart", func(t *testing.T) {
		call := &niro.ToolCall{ID: "c1", Name: "fn", Args: json.RawMessage(`{}`)}
		p := niro.ToolCallPart(call)
		assertEqual(t, p.Kind, niro.KindToolCall)
		assertEqual(t, p.Tool.Name, "fn")
	})

	t.Run("ToolResultPart", func(t *testing.T) {
		result := &niro.ToolResult{CallID: "c1", Content: "success"}
		p := niro.ToolResultPart(result)
		assertEqual(t, p.Kind, niro.KindToolResult)
		assertEqual(t, p.Result.Content, "success")
	})

	t.Run("CustomPart", func(t *testing.T) {
		p := niro.CustomPart(&niro.ExperimentalFrame{Type: "reasoning_summary", Data: "ok"})
		assertEqual(t, p.Kind, niro.KindCustom)
		assertEqual(t, p.Custom.Type, "reasoning_summary")
	})
}

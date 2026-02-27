package ryn_test

import (
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

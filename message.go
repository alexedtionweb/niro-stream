package ryn

// Role identifies the sender of a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a single message in a conversation.
// A message contains one or more Parts, enabling multimodal content:
// a user message can carry text alongside images, audio, or video.
type Message struct {
	Role  Role
	Parts []Part
}

// Part is a content segment within a Message.
// Each Part carries exactly one kind of content.
type Part struct {
	Kind Kind

	// Text content (KindText)
	Text string

	// Binary content (KindAudio, KindImage, KindVideo)
	Data []byte
	Mime string // MIME type for Data

	// URL reference for remote content (alternative to Data).
	// Providers that support URL references will use this directly;
	// others will fetch and inline the data.
	URL string

	// Tool call (KindToolCall) — for assistant messages
	Tool *ToolCall

	// Tool result (KindToolResult) — for tool messages
	Result *ToolResult
}

// --- Message constructors ---

// UserText creates a single-part text message from the user.
func UserText(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{{Kind: KindText, Text: text}}}
}

// SystemText creates a system message.
func SystemText(text string) Message {
	return Message{Role: RoleSystem, Parts: []Part{{Kind: KindText, Text: text}}}
}

// AssistantText creates an assistant text message.
// Useful for injecting assistant-turn prefills.
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{{Kind: KindText, Text: text}}}
}

// ToolMessage creates a tool result message.
func ToolMessage(callID, content string) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Kind:   KindToolResult,
			Result: &ToolResult{CallID: callID, Content: content},
		}},
	}
}

// ToolErrorMessage creates a tool error result message.
func ToolErrorMessage(callID, errMsg string) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Kind:   KindToolResult,
			Result: &ToolResult{CallID: callID, Content: errMsg, IsError: true},
		}},
	}
}

// Multi creates a multimodal message with multiple parts.
func Multi(role Role, parts ...Part) Message {
	return Message{Role: role, Parts: parts}
}

// --- Part constructors ---

// TextPart creates a text content Part.
func TextPart(s string) Part {
	return Part{Kind: KindText, Text: s}
}

// ImagePart creates an image content Part from binary data.
func ImagePart(data []byte, mime string) Part {
	return Part{Kind: KindImage, Data: data, Mime: mime}
}

// ImageURLPart creates an image content Part from a URL.
func ImageURLPart(url, mime string) Part {
	return Part{Kind: KindImage, URL: url, Mime: mime}
}

// AudioPart creates an audio content Part from binary data.
func AudioPart(data []byte, mime string) Part {
	return Part{Kind: KindAudio, Data: data, Mime: mime}
}

// VideoPart creates a video content Part from binary data.
func VideoPart(data []byte, mime string) Part {
	return Part{Kind: KindVideo, Data: data, Mime: mime}
}

// ToolCallPart creates a tool call Part (for assistant messages).
func ToolCallPart(call *ToolCall) Part {
	return Part{Kind: KindToolCall, Tool: call}
}

// ToolResultPart creates a tool result Part (for tool messages).
func ToolResultPart(result *ToolResult) Part {
	return Part{Kind: KindToolResult, Result: result}
}

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
	Kind   Kind
	Text   string      // Text content (KindText)
	Data   []byte      // Binary content (KindAudio, KindImage, KindVideo)
	Mime   string      // MIME type for Data
	Tool   *ToolCall   // Tool call (KindToolCall) — for assistant messages
	Result *ToolResult // Tool result (KindToolResult) — for tool messages
}

// --- Message constructors ---

// Text creates a single-part text message.
func Text(role Role, text string) Message {
	return Message{
		Role:  role,
		Parts: []Part{{Kind: KindText, Text: text}},
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

// ImagePart creates an image content Part.
func ImagePart(data []byte, mime string) Part {
	return Part{Kind: KindImage, Data: data, Mime: mime}
}

// AudioPart creates an audio content Part.
func AudioPart(data []byte, mime string) Part {
	return Part{Kind: KindAudio, Data: data, Mime: mime}
}

// VideoPart creates a video content Part.
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

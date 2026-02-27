package ryn

import "encoding/json"

// Kind identifies the type of data a Frame carries.
type Kind uint8

const (
	KindText       Kind = iota + 1 // Text token
	KindAudio                      // Audio chunk (PCM, opus, etc.)
	KindImage                      // Image data (PNG, JPEG, etc.)
	KindVideo                      // Video frame
	KindToolCall                   // Tool invocation request from LLM
	KindToolResult                 // Tool invocation result
	KindControl                    // Pipeline control signal
)

// String returns the human-readable name of the Kind.
func (k Kind) String() string {
	switch k {
	case KindText:
		return "text"
	case KindAudio:
		return "audio"
	case KindImage:
		return "image"
	case KindVideo:
		return "video"
	case KindToolCall:
		return "tool_call"
	case KindToolResult:
		return "tool_result"
	case KindControl:
		return "control"
	default:
		return "unknown"
	}
}

// Frame is the fundamental unit of data flowing through a Ryn pipeline.
//
// Frame is a tagged union optimized for the common case: text tokens.
// For text, only Kind and Text are populated — no allocations beyond
// the string header. Binary payloads (audio, image, video) use the
// Data and Mime fields. Tool interactions use Tool and Result.
//
// Frames are passed by value through channels. They are small (~80 bytes)
// and most fields are zero for any given Kind.
type Frame struct {
	Kind   Kind        // Discriminator — always check this first
	Text   string      // Token text (KindText)
	Data   []byte      // Binary payload (KindAudio, KindImage, KindVideo)
	Mime   string      // MIME type for Data (e.g. "audio/pcm", "image/png")
	Tool   *ToolCall   // Tool call request (KindToolCall)
	Result *ToolResult // Tool call result (KindToolResult)
	Signal Signal      // Control signal (KindControl)
}

// Signal represents a pipeline control signal.
type Signal uint8

const (
	SignalNone  Signal = iota
	SignalFlush        // Flush buffered data downstream
	SignalEOT          // End of turn
	SignalAbort        // Abort pipeline
)

// String returns the human-readable name of the Signal.
func (s Signal) String() string {
	switch s {
	case SignalFlush:
		return "flush"
	case SignalEOT:
		return "eot"
	case SignalAbort:
		return "abort"
	default:
		return "none"
	}
}

// --- Frame constructors ---
// These are the idiomatic way to create Frames. Each constructor
// sets only the fields relevant to the Kind.

// TextFrame creates a Frame carrying a text token.
func TextFrame(s string) Frame {
	return Frame{Kind: KindText, Text: s}
}

// AudioFrame creates a Frame carrying audio data.
func AudioFrame(data []byte, mime string) Frame {
	return Frame{Kind: KindAudio, Data: data, Mime: mime}
}

// ImageFrame creates a Frame carrying image data.
func ImageFrame(data []byte, mime string) Frame {
	return Frame{Kind: KindImage, Data: data, Mime: mime}
}

// VideoFrame creates a Frame carrying video data.
func VideoFrame(data []byte, mime string) Frame {
	return Frame{Kind: KindVideo, Data: data, Mime: mime}
}

// ToolCallFrame creates a Frame carrying a tool call request.
func ToolCallFrame(call *ToolCall) Frame {
	return Frame{Kind: KindToolCall, Tool: call}
}

// ToolResultFrame creates a Frame carrying a tool call result.
func ToolResultFrame(result *ToolResult) Frame {
	return Frame{Kind: KindToolResult, Result: result}
}

// ControlFrame creates a Frame carrying a control signal.
func ControlFrame(sig Signal) Frame {
	return Frame{Kind: KindControl, Signal: sig}
}

// --- Tool types ---

// ToolCall represents an LLM's request to invoke a tool.
type ToolCall struct {
	ID   string          // Provider-assigned call ID
	Name string          // Tool function name
	Args json.RawMessage // Arguments as JSON
}

// ToolResult represents the outcome of a tool invocation.
type ToolResult struct {
	CallID  string // Matches ToolCall.ID
	Content string // Result content
	Err     string // Error message, if any
}

// Tool defines a tool that can be provided to an LLM.
type Tool struct {
	Name        string          // Function name
	Description string          // Human-readable description
	Parameters  json.RawMessage // JSON Schema for parameters
}

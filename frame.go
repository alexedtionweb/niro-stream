package niro

import "encoding/json"

// Kind identifies the type of data a Frame carries.
type Kind uint8

const (
	KindText       Kind = iota + 1 // Text token (the hot path)
	KindAudio                      // Audio chunk (PCM, opus, etc.)
	KindImage                      // Image data (PNG, JPEG, etc.)
	KindVideo                      // Video frame
	KindToolCall                   // Tool invocation request from LLM
	KindToolResult                 // Tool invocation result
	KindUsage                      // Token usage report
	KindCustom                     // Experimental/provider-specific payload
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
	case KindUsage:
		return "usage"
	case KindCustom:
		return "custom"
	case KindControl:
		return "control"
	default:
		return "unknown"
	}
}

// Frame is the fundamental unit of data flowing through a Niro pipeline.
//
// Frame is a tagged union optimized for the common case: text tokens.
// For text, only Kind and Text are populated — zero allocations beyond
// the string header. Binary payloads (audio, image, video) use the
// Data and Mime fields. Tool interactions use Tool and Result.
//
// Frames are passed by value through channels. They are small and
// most fields are zero for any given Kind.
type Frame struct {
	Kind   Kind               // Discriminator — always check this first
	Text   string             // Token text (KindText)
	Data   []byte             // Binary payload (KindAudio, KindImage, KindVideo)
	Mime   string             // MIME type for Data (e.g. "audio/pcm", "image/png")
	Tool   *ToolCall          // Tool call request (KindToolCall)
	Result *ToolResult        // Tool call result (KindToolResult)
	Usage  *Usage             // Token usage (KindUsage) — emitted by providers at end of stream
	Custom *ExperimentalFrame // Provider-specific/experimental payload (KindCustom)
	Signal Signal             // Control signal (KindControl)
}

// ExperimentalFrame carries provider-specific data without expanding core kinds.
// Type is an application/provider-defined discriminator (e.g. "reasoning_summary").
type ExperimentalFrame struct {
	Type string
	Data any
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

// UsageFrame creates a Frame carrying token usage data.
func UsageFrame(u *Usage) Frame {
	return Frame{Kind: KindUsage, Usage: u}
}

// CustomFrame creates a Frame carrying an experimental/provider-specific payload.
func CustomFrame(c *ExperimentalFrame) Frame {
	return Frame{Kind: KindCustom, Custom: c}
}

// ControlFrame creates a Frame carrying a control signal.
func ControlFrame(sig Signal) Frame {
	return Frame{Kind: KindControl, Signal: sig}
}

// --- Audio format MIME types ---
//
// These constants encode sample rate, bit depth, and channel count in a
// single MIME string carried by Frame.Mime / Part.Mime. All formats are
// raw uncompressed PCM (little-endian signed 16-bit, mono) unless noted.
//
// Use them with AudioFrame and AudioPart so consumers can decode without
// additional out-of-band configuration.
const (
	// AudioPCM8k is 8 kHz 16-bit mono PCM — telephony grade (PSTN / G.711-compatible).
	AudioPCM8k = "audio/pcm;rate=8000;bits=16;channels=1"

	// AudioPCM16k is 16 kHz 16-bit mono PCM — standard ASR / Nova Sonic input.
	AudioPCM16k = "audio/pcm;rate=16000;bits=16;channels=1"

	// AudioPCM24k is 24 kHz 16-bit mono PCM — Nova Sonic output, OpenAI Realtime.
	AudioPCM24k = "audio/pcm;rate=24000;bits=16;channels=1"

	// AudioPCM44k is 44.1 kHz 16-bit mono PCM — CD quality.
	AudioPCM44k = "audio/pcm;rate=44100;bits=16;channels=1"

	// AudioPCM48k is 48 kHz 16-bit mono PCM — WebRTC / studio quality.
	AudioPCM48k = "audio/pcm;rate=48000;bits=16;channels=1"
)

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
	Content string // Result content (may be JSON or plain text)
	IsError bool   // Whether this result represents an error
}

// Tool defines a tool that can be provided to an LLM.
type Tool struct {
	Name        string          // Function name
	Description string          // Human-readable description
	Parameters  json.RawMessage // JSON Schema for parameters
}

// ToolChoice controls how the model selects tools.
type ToolChoice string

const (
	ToolChoiceAuto     ToolChoice = "auto"     // Model decides (default)
	ToolChoiceNone     ToolChoice = "none"     // Never call tools
	ToolChoiceRequired ToolChoice = "required" // Must call at least one tool
)

// ToolChoiceFunc forces the model to call a specific tool.
func ToolChoiceFunc(name string) ToolChoice {
	return ToolChoice("func:" + name)
}

// --- Usage types ---

// Usage tracks token consumption for a generation.
type Usage struct {
	InputTokens  int // Prompt tokens
	OutputTokens int // Completion tokens
	TotalTokens  int // InputTokens + OutputTokens (some providers report directly)

	// Provider-specific detail (optional).
	// E.g. cached tokens, audio tokens, reasoning tokens.
	Detail map[string]int
}

const (
	// UsageReasoningTokens is the standard key for reasoning token count.
	UsageReasoningTokens = "reasoning_tokens"
	// UsageReasoningCost is the standard key for provider-reported reasoning cost units.
	UsageReasoningCost = "reasoning_cost"
)

// Add accumulates usage from another Usage into this one.
func (u *Usage) Add(other *Usage) {
	if other == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	if len(other.Detail) > 0 {
		if u.Detail == nil {
			// Size hint: most providers report 2-4 detail keys
			u.Detail = make(map[string]int, 4)
		}
		for k, v := range other.Detail {
			u.Detail[k] += v
		}
	}
}

// Reset zeroes all fields. Useful when reusing a Usage from a pool.
func (u *Usage) Reset() {
	u.InputTokens = 0
	u.OutputTokens = 0
	u.TotalTokens = 0
	// Clear map without reallocating
	for k := range u.Detail {
		delete(u.Detail, k)
	}
}

// ResponseMeta carries metadata about a completed generation.
// Available after the stream is fully consumed via Stream.Response().
type ResponseMeta struct {
	// Model actually used (may differ from requested if provider aliases).
	Model string

	// FinishReason indicates why generation stopped.
	// Common values: "stop", "length", "tool_calls", "content_filter".
	FinishReason string

	// ID is the provider-assigned response ID.
	ID string

	// Usage is the token usage for this generation.
	Usage Usage

	// Provider-specific metadata (opaque).
	ProviderMeta map[string]any
}

package realtime

import (
	"encoding/base64"
	"strings"
	"sync"

	"github.com/alexedtionweb/niro-stream"
)

// CaptureBuffer keeps the newest camera frames. Push from the camera as
// frames arrive. When the model calls a video tool, Snapshot is what it sees.
type CaptureBuffer struct {
	mu     sync.Mutex
	max    int
	frames []niro.Part
}

// NewCaptureBuffer keeps at most maxFrames. A non-positive max keeps 8.
func NewCaptureBuffer(maxFrames int) *CaptureBuffer {
	if maxFrames <= 0 {
		maxFrames = 8
	}
	return &CaptureBuffer{max: maxFrames}
}

// Push copies an image or video frame into the buffer, dropping the oldest.
func (b *CaptureBuffer) Push(f niro.Frame) {
	if b == nil || (f.Kind != niro.KindImage && f.Kind != niro.KindVideo) || len(f.Data) == 0 {
		return
	}
	cp := make([]byte, len(f.Data))
	copy(cp, f.Data)
	part := niro.Part{Kind: f.Kind, Data: cp, Mime: f.Mime}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.frames) >= b.max {
		copy(b.frames, b.frames[1:])
		b.frames = b.frames[:b.max-1]
	}
	b.frames = append(b.frames, part)
}

// Snapshot copies the frames currently in the buffer.
func (b *CaptureBuffer) Snapshot() []niro.Part {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]niro.Part, len(b.frames))
	for i, p := range b.frames {
		cp := make([]byte, len(p.Data))
		copy(cp, p.Data)
		out[i] = niro.Part{Kind: p.Kind, Data: cp, Mime: p.Mime}
	}
	return out
}

// CaptureResult is the tool result for a video tool: the frames from Snapshot.
func CaptureResult(call *niro.ToolCall, parts []niro.Part) niro.Frame {
	id := ""
	if call != nil {
		id = call.ID
	}
	return niro.ToolResultFrame(&niro.ToolResult{CallID: id, Parts: parts})
}

func toolChoiceValue(c niro.ToolChoice) any {
	switch c {
	case "":
		return nil
	case niro.ToolChoiceAuto, niro.ToolChoiceNone, niro.ToolChoiceRequired:
		return string(c)
	default:
		name := strings.TrimPrefix(string(c), niro.ToolChoiceFuncPrefix)
		if name != "" && name != string(c) {
			return map[string]any{"type": "function", "name": name}
		}
		return string(c)
	}
}

func imageFrame(p niro.Part) bool {
	if p.Kind == niro.KindImage {
		return true
	}
	if p.Kind != niro.KindVideo {
		return false
	}
	m := strings.ToLower(p.Mime)
	return strings.HasPrefix(m, "image/") || strings.Contains(m, "jpeg") || strings.Contains(m, "png") || strings.Contains(m, "webp")
}

func mediaRef(p niro.Part) string {
	if p.URL != "" {
		return p.URL
	}
	if len(p.Data) == 0 {
		return ""
	}
	mime := p.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

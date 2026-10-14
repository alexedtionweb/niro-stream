package realtime

import (
	"encoding/base64"
	"strconv"
	"strings"
	"sync"

	"github.com/alexedtionweb/niro-stream"
)

// bufPool holds JSON encode buffers for the audio append hot path.
// 20 ms of 24 kHz PCM16 is 960 bytes → ~1.3 KB of base64 JSON.
var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

func getBuf() *[]byte { return bufPool.Get().(*[]byte) }

func putBuf(bp *[]byte) {
	if bp == nil {
		return
	}
	if cap(*bp) > 1<<20 {
		return
	}
	*bp = (*bp)[:0]
	bufPool.Put(bp)
}

type audioWire struct {
	extended bool
	format   string
	rate     int
	ms       int
}

func encodeAudioAppend(dst, pcm []byte, spec audioWire, frames [][]byte) []byte {
	dst = append(dst, `{"type":"input_audio_buffer.append","audio":"`...)
	dst = appendB64(dst, pcm)
	dst = append(dst, '"')
	if spec.extended {
		if spec.format != "" {
			dst = append(dst, `,"input_audio_format":"`...)
			dst = append(dst, spec.format...)
			dst = append(dst, '"')
		}
		if spec.rate > 0 {
			dst = append(dst, `,"sample_rate_hz":`...)
			dst = strconv.AppendInt(dst, int64(spec.rate), 10)
		}
		if spec.ms > 0 {
			dst = append(dst, `,"duration_ms":`...)
			dst = strconv.AppendInt(dst, int64(spec.ms), 10)
		}
		if len(frames) > 0 {
			dst = append(dst, `,"video_frames":[`...)
			for i, f := range frames {
				if i > 0 {
					dst = append(dst, ',')
				}
				dst = append(dst, '"')
				dst = appendB64(dst, f)
				dst = append(dst, '"')
			}
			dst = append(dst, ']')
		}
	}
	dst = append(dst, '}')
	return dst
}

func appendB64(dst, src []byte) []byte {
	n := base64.StdEncoding.EncodedLen(len(src))
	need := len(dst) + n
	if cap(dst) < need {
		grown := make([]byte, len(dst), need+need/4+256)
		copy(grown, dst)
		dst = grown
	}
	out := dst[:need]
	base64.StdEncoding.Encode(out[len(dst):], src)
	return out
}

func decodeB64(dst, src []byte) ([]byte, error) {
	n := base64.StdEncoding.DecodedLen(len(src))
	if cap(dst) < n {
		dst = make([]byte, n)
	} else {
		dst = dst[:n]
	}
	n, err := base64.StdEncoding.Decode(dst, src)
	if err != nil {
		return nil, err
	}
	return dst[:n], nil
}

func vllmAudioFormat(mime string) string {
	switch {
	case strings.Contains(mime, "pcmu"):
		return "g711_ulaw"
	case strings.Contains(mime, "pcma"):
		return "g711_alaw"
	case strings.Contains(mime, "f32"), strings.Contains(mime, "float"):
		return "pcm_f32le"
	default:
		return "pcm16"
	}
}

func oaiAudioType(mime string) string {
	switch {
	case strings.Contains(mime, "pcmu"):
		return "audio/pcmu"
	case strings.Contains(mime, "pcma"):
		return "audio/pcma"
	default:
		return "audio/pcm"
	}
}

func rateFromMIME(mime string, fallback int) int {
	const key = "rate="
	i := strings.Index(mime, key)
	if i < 0 {
		return fallback
	}
	n := 0
	for _, c := range mime[i+len(key):] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return fallback
	}
	return n
}

func durationMs(pcm []byte, format string, rate int) int {
	if rate <= 0 || len(pcm) == 0 {
		return 0
	}
	bps := 2
	switch format {
	case "pcm_f32le":
		bps = 4
	case "g711_ulaw", "g711_alaw":
		bps = 1
	}
	if bps == 0 || len(pcm) < bps {
		return 0
	}
	return (len(pcm) / bps) * 1000 / rate
}

func pcmMIME(rate int, format string) string {
	if format == "g711_ulaw" || strings.Contains(format, "pcmu") {
		return niro.AudioPCMU8k
	}
	if format == "g711_alaw" || strings.Contains(format, "pcma") {
		return niro.AudioPCMA8k
	}
	switch rate {
	case 8000:
		return niro.AudioPCM8k
	case 16000:
		return niro.AudioPCM16k
	case 24000:
		return niro.AudioPCM24k
	case 44100:
		return niro.AudioPCM44k
	case 48000:
		return niro.AudioPCM48k
	default:
		if rate <= 0 {
			return "audio/pcm;bits=16;channels=1"
		}
		return "audio/pcm;rate=" + strconv.Itoa(rate) + ";bits=16;channels=1"
	}
}

// topFields is a zero-alloc view of the top-level keys the hot path needs.
// String slices alias the message buffer and are invalid after the buffer
// is reused.
type topFields struct {
	typ, delta, audio, transcript, format []byte
	rate                                  int
	seq                                   int64
}

func (t topFields) audioDelta() bool {
	return eq(t.typ, "response.output_audio.delta") || eq(t.typ, "response.audio.delta")
}

func (t topFields) textDelta() bool {
	return eq(t.typ, "response.output_audio_transcript.delta") ||
		eq(t.typ, "response.audio_transcript.delta") ||
		eq(t.typ, "response.output_text.delta") ||
		eq(t.typ, "response.text.delta") ||
		eq(t.typ, "transcription.delta")
}

func (t topFields) payload() []byte {
	if len(t.delta) > 0 {
		return t.delta
	}
	if len(t.audio) > 0 {
		return t.audio
	}
	return t.transcript
}

func scanTop(msg []byte) topFields {
	var t topFields
	i := skipWS(msg, 0)
	if i >= len(msg) || msg[i] != '{' {
		return t
	}
	i++
	for i < len(msg) {
		i = skipWS(msg, i)
		if i >= len(msg) || msg[i] == '}' {
			break
		}
		if msg[i] == ',' {
			i++
			continue
		}
		if msg[i] != '"' {
			break
		}
		key, next, ok := parseString(msg, i)
		if !ok {
			break
		}
		i = skipWS(msg, next)
		if i >= len(msg) || msg[i] != ':' {
			break
		}
		i = skipWS(msg, i+1)
		if i >= len(msg) {
			break
		}
		switch {
		case eq(key, "type"), eq(key, "delta"), eq(key, "audio"), eq(key, "transcript"), eq(key, "format"):
			if msg[i] == '"' {
				v, n, ok := parseString(msg, i)
				if !ok {
					return t
				}
				switch {
				case eq(key, "type"):
					t.typ = v
				case eq(key, "delta"):
					t.delta = v
				case eq(key, "audio"):
					t.audio = v
				case eq(key, "transcript"):
					t.transcript = v
				default:
					t.format = v
				}
				i = n
				continue
			}
		case eq(key, "sample_rate_hz"), eq(key, "server_event_seq"):
			if msg[i] == '-' || (msg[i] >= '0' && msg[i] <= '9') {
				j := i
				if msg[j] == '-' {
					j++
				}
				for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
					j++
				}
				n := parseU(msg[i:j])
				if eq(key, "sample_rate_hz") {
					t.rate = int(n)
				} else {
					t.seq = n
				}
				i = j
				continue
			}
		}
		i = skipValue(msg, i)
	}
	return t
}

func parseU(b []byte) int64 {
	var n int64
	neg := false
	for i, c := range b {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

func eq(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func skipWS(msg []byte, i int) int {
	for i < len(msg) {
		switch msg[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func parseString(msg []byte, i int) (content []byte, next int, ok bool) {
	if i >= len(msg) || msg[i] != '"' {
		return nil, i, false
	}
	i++
	start := i
	for i < len(msg) {
		switch msg[i] {
		case '\\':
			return unescape(msg, start, i)
		case '"':
			return msg[start:i], i + 1, true
		default:
			i++
		}
	}
	return nil, len(msg), false
}

func unescape(msg []byte, start, slash int) ([]byte, int, bool) {
	buf := make([]byte, 0, len(msg)-start)
	buf = append(buf, msg[start:slash]...)
	i := slash
	for i < len(msg) {
		c := msg[i]
		if c == '"' {
			return buf, i + 1, true
		}
		if c != '\\' {
			buf = append(buf, c)
			i++
			continue
		}
		i++
		if i >= len(msg) {
			return nil, len(msg), false
		}
		switch msg[i] {
		case '"', '\\', '/':
			buf = append(buf, msg[i])
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			if i+4 >= len(msg) {
				return nil, len(msg), false
			}
			r, ok := parseHex4(msg[i+1 : i+5])
			if !ok {
				return nil, len(msg), false
			}
			buf = appendRune(buf, r)
			i += 4
		default:
			buf = append(buf, msg[i])
		}
		i++
	}
	return nil, len(msg), false
}

func parseHex4(b []byte) (rune, bool) {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r += rune(c - '0')
		case c >= 'a' && c <= 'f':
			r += rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r += rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

func appendRune(buf []byte, r rune) []byte {
	var tmp [4]byte
	n := copy(tmp[:], string(r))
	return append(buf, tmp[:n]...)
}

func skipValue(msg []byte, i int) int {
	if i >= len(msg) {
		return i
	}
	switch msg[i] {
	case '"':
		_, n, ok := parseString(msg, i)
		if !ok {
			return len(msg)
		}
		return n
	case '{', '[':
		return skipContainer(msg, i)
	default:
		for i < len(msg) {
			c := msg[i]
			if c == ',' || c == '}' || c == ']' {
				break
			}
			i++
		}
		return i
	}
}

func skipContainer(msg []byte, i int) int {
	if i >= len(msg) {
		return i
	}
	open := msg[i]
	closer := byte('}')
	if open == '[' {
		closer = ']'
	}
	depth := 0
	for i < len(msg) {
		c := msg[i]
		if c == '"' {
			_, n, ok := parseString(msg, i)
			if !ok {
				return len(msg)
			}
			i = n
			continue
		}
		if c == open {
			depth++
		} else if c == closer {
			depth--
			i++
			if depth == 0 {
				return i
			}
			continue
		}
		i++
	}
	return i
}

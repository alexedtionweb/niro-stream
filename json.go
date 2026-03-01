package ryn

import (
	stdjson "encoding/json"
	"io"
	"sync/atomic"
)

// JSONEncoder is the minimal interface required by JSON encoders.
// Compatible with encoding/json and the same JSON libraries supported by Fiber.
type JSONEncoder interface {
	Encode(v any) error
}

// JSONDecoder is the minimal interface required by JSON decoders.
// Compatible with encoding/json and the same JSON libraries supported by Fiber.
type JSONDecoder interface {
	Decode(v any) error
}

// JSONLibrary defines the JSON functions used by Niro.
//
// Compatible with the same libraries supported by Fiber:
//   - encoding/json (stdlib)
//   - github.com/goccy/go-json
//   - github.com/bytedance/sonic
//   - github.com/segmentio/encoding/json
//   - github.com/json-iterator/go
//
// Users can call SetJSON to swap the implementation globally.
type JSONLibrary struct {
	Marshal       func(v any) ([]byte, error)
	Unmarshal     func(data []byte, v any) error
	Valid         func(data []byte) bool
	NewEncoder    func(w io.Writer) JSONEncoder
	NewDecoder    func(r io.Reader) JSONDecoder
	MarshalIndent func(v any, prefix, indent string) ([]byte, error)
}

var jsonLib atomic.Value // *JSONLibrary

func init() {
	SetJSON(nil)
}

// SetJSON replaces the JSON implementation used by Niro.
// If lib is nil, the stdlib encoding/json implementation is used.
// Any nil fields are filled with stdlib defaults.
func SetJSON(lib *JSONLibrary) {
	def := defaultJSONLibrary()
	if lib == nil {
		lib = def
	} else {
		// Fill missing fields with defaults
		if lib.Marshal == nil {
			lib.Marshal = def.Marshal
		}
		if lib.Unmarshal == nil {
			lib.Unmarshal = def.Unmarshal
		}
		if lib.Valid == nil {
			lib.Valid = def.Valid
		}
		if lib.NewEncoder == nil {
			lib.NewEncoder = def.NewEncoder
		}
		if lib.NewDecoder == nil {
			lib.NewDecoder = def.NewDecoder
		}
		if lib.MarshalIndent == nil {
			lib.MarshalIndent = def.MarshalIndent
		}
	}
	jsonLib.Store(lib)
}

// JSON returns the currently active JSON library.
func JSON() *JSONLibrary {
	lib, _ := jsonLib.Load().(*JSONLibrary)
	if lib == nil {
		lib = defaultJSONLibrary()
		jsonLib.Store(lib)
	}
	return lib
}

// JSONMarshal marshals v using the configured JSON library.
func JSONMarshal(v any) ([]byte, error) { return JSON().Marshal(v) }

// JSONUnmarshal unmarshals data into v using the configured JSON library.
func JSONUnmarshal(data []byte, v any) error { return JSON().Unmarshal(data, v) }

// JSONValid reports whether data is valid JSON using the configured library.
func JSONValid(data []byte) bool { return JSON().Valid(data) }

// JSONNewEncoder returns a new encoder using the configured JSON library.
func JSONNewEncoder(w io.Writer) JSONEncoder { return JSON().NewEncoder(w) }

// JSONNewDecoder returns a new decoder using the configured JSON library.
func JSONNewDecoder(r io.Reader) JSONDecoder { return JSON().NewDecoder(r) }

// JSONMarshalIndent marshals v with indentation using the configured library.
func JSONMarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return JSON().MarshalIndent(v, prefix, indent)
}

func defaultJSONLibrary() *JSONLibrary {
	return &JSONLibrary{
		Marshal:   stdjson.Marshal,
		Unmarshal: stdjson.Unmarshal,
		Valid:     stdjson.Valid,
		NewEncoder: func(w io.Writer) JSONEncoder {
			return stdjson.NewEncoder(w)
		},
		NewDecoder: func(r io.Reader) JSONDecoder {
			return stdjson.NewDecoder(r)
		},
		MarshalIndent: stdjson.MarshalIndent,
	}
}

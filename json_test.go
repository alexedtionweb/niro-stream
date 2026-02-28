package ryn_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"ryn.dev/ryn"
)

func TestSetJSON(t *testing.T) {
	// Not parallel: mutates global JSON backend.

	called := atomic.Int32{}
	custom := &ryn.JSONLibrary{
		Marshal: func(v any) ([]byte, error) {
			called.Add(1)
			return []byte(`{"a":1}`), nil
		},
		Unmarshal: func(data []byte, v any) error {
			called.Add(1)
			return json.Unmarshal(data, v)
		},
		Valid: func(data []byte) bool {
			called.Add(1)
			return true
		},
	}

	ryn.SetJSON(custom)
	t.Cleanup(func() { ryn.SetJSON(nil) })

	var out struct {
		A int `json:"a"`
	}

	b, err := ryn.JSONMarshal(out)
	assertNoError(t, err)
	assertTrue(t, len(b) > 0)
	assertNoError(t, ryn.JSONUnmarshal(b, &out))
	assertTrue(t, ryn.JSONValid(b))
	assertTrue(t, called.Load() >= 3)
}

func TestJSONNewEncoderDecoder(t *testing.T) {
	t.Parallel()

	// Encoder
	var buf bytes.Buffer
	enc := ryn.JSONNewEncoder(&buf)
	assertNotNil(t, enc)
	type testData struct{ X int }
	err := enc.Encode(testData{X: 42})
	assertNoError(t, err)
	assertTrue(t, strings.Contains(buf.String(), "42"))

	// Decoder
	dec := ryn.JSONNewDecoder(strings.NewReader(`{"X":99}`))
	assertNotNil(t, dec)
	var out testData
	err = dec.Decode(&out)
	assertNoError(t, err)
	assertEqual(t, out.X, 99)
}

func TestJSONMarshalIndent(t *testing.T) {
	t.Parallel()

	type data struct {
		Name string `json:"name"`
	}
	b, err := ryn.JSONMarshalIndent(data{Name: "test"}, "", "  ")
	assertNoError(t, err)
	assertTrue(t, strings.Contains(string(b), "\n"))
	assertTrue(t, strings.Contains(string(b), `"name"`))
}

func TestJSONLibraryDefaultFallback(t *testing.T) {
	t.Parallel()

	// Test that JSON() returns a valid library
	lib := ryn.JSON()
	assertNotNil(t, lib)
	assertNotNil(t, lib.Marshal)
	assertNotNil(t, lib.Unmarshal)

	b, err := lib.Marshal(map[string]int{"x": 1})
	assertNoError(t, err)
	assertTrue(t, ryn.JSONValid(b))
}

func TestSetJSONNilResetsToDefault(t *testing.T) {
	// Not parallel: mutates global JSON backend.
	ryn.SetJSON(nil) // reset
	t.Cleanup(func() { ryn.SetJSON(nil) })

	b, err := ryn.JSONMarshal(map[string]string{"k": "v"})
	assertNoError(t, err)
	assertTrue(t, ryn.JSONValid(b))
}

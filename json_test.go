package ryn_test

import (
	"encoding/json"
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

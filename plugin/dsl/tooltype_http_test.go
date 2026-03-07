package dsl

import (
	"testing"
)

func TestResolveParseValueJSONPath(t *testing.T) {
	response := map[string]any{
		"status": 200,
		"body": map[string]any{
			"userId":    1,
			"id":        101,
			"title":     "delectus aut autem",
			"completed": false,
		},
	}
	tests := []struct {
		expr string
		want any
	}{
		{"$.status", 200},
		{"$.body.userId", 1},
		{"$.body.title", "delectus aut autem"},
		{"$.body.completed", false},
		{"$.body.missing", nil},
		{"'literal'", "literal"},
	}
	for _, tt := range tests {
		got, err := resolveParseValue(response, tt.expr)
		if err != nil {
			t.Errorf("resolveParseValue(%q): %v", tt.expr, err)
			continue
		}
		if !valueEqual(got, tt.want) {
			t.Errorf("resolveParseValue(%q) = %v (%T), want %v (%T)", tt.expr, got, got, tt.want, tt.want)
		}
	}
}

func valueEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Normalize number types for comparison (JSONPath may return float64 or int)
	if na, ok := toNumber(a); ok {
		if nb, ok := toNumber(b); ok {
			return na == nb
		}
	}
	return a == b
}

func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}

func TestApplyCases(t *testing.T) {
	response := map[string]any{
		"status": 200,
		"body": map[string]any{
			"userId": 1, "id": 101, "title": "test", "completed": false,
		},
	}
	cases := []httpCase{
		{When: "$.status >= 200 && $.status < 300", Parse: map[string]any{
			"userId": "$.body.userId", "id": "$.body.id", "title": "$.body.title", "completed": "$.body.completed",
		}},
	}
	out, err := applyCases(response, cases)
	if err != nil {
		t.Fatalf("applyCases: %v", err)
	}
	if out["userId"] != 1 || out["id"] != 101 || out["title"] != "test" || out["completed"] != false {
		t.Errorf("applyCases: got %v", out)
	}
}

func TestApplyCasesNoMatch(t *testing.T) {
	response := map[string]any{"status": 500, "body": map[string]any{}}
	cases := []httpCase{
		{When: "$.status == 200", Parse: map[string]any{"x": "$.body.x"}},
		{When: "true", Parse: map[string]any{"error": "'fallback'"}},
	}
	out, err := applyCases(response, cases)
	if err != nil {
		t.Fatalf("applyCases: %v", err)
	}
	if out["error"] != "fallback" {
		t.Errorf("applyCases fallback: got %v", out)
	}
}

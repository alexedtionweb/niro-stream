package dsl

import (
	"context"
	"testing"
)

func TestWithRunContext(t *testing.T) {
	ctx := context.Background()
	if RunContextFrom(ctx) != nil {
		t.Error("RunContextFrom(background) should be nil")
	}
	rc := NewRunContext()
	rc.Set("x", "y")
	ctx2 := WithRunContext(ctx, rc)
	if RunContextFrom(ctx2) != rc {
		t.Error("RunContextFrom(WithRunContext) should return same RunContext")
	}
	if RunContextFrom(context.Background()) != nil {
		t.Error("RunContextFrom(background) should be nil")
	}
}

func TestRunContextNil(t *testing.T) {
	ctx := context.Background()
	ctx2 := WithRunContext(ctx, nil)
	if RunContextFrom(ctx2) != nil {
		t.Error("WithRunContext(ctx, nil) should not attach value")
	}
}

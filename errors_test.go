package ryn_test

import (
	"fmt"
	"strings"
	"testing"

	"ryn.dev/ryn"
)

func TestErrorCreation(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeInvalidRequest, "test error")
	assertNotNil(t, err)
	assertEqual(t, err.Code, ryn.ErrCodeInvalidRequest)
	assertEqual(t, err.Message, "test error")
	assertTrue(t, err.Error() != "")
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("inner error")
	err := ryn.WrapError(ryn.ErrCodeProviderError, "provider failed", inner)
	assertTrue(t, err != nil)

	// Check error message contains both
	msg := err.Error()
	assertTrue(t, strings.Contains(msg, "provider failed"))
	assertTrue(t, strings.Contains(msg, "inner error"))
}

func TestErrorCheckers(t *testing.T) {
	t.Parallel()

	rateLimitErr := ryn.NewError(ryn.ErrCodeRateLimited, "too many requests")
	assertTrue(t, ryn.IsRetryable(rateLimitErr))
	assertTrue(t, ryn.IsRateLimited(rateLimitErr))
	assertEqual(t, ryn.IsTimeout(rateLimitErr), false)

	authErr := ryn.NewError(ryn.ErrCodeAuthenticationFailed, "invalid key")
	assertTrue(t, ryn.IsAuthError(authErr))
	assertEqual(t, ryn.IsRetryable(authErr), false)

	timeoutErr := ryn.NewError(ryn.ErrCodeTimeout, "took too long")
	assertTrue(t, ryn.IsTimeout(timeoutErr))
	assertTrue(t, ryn.IsRetryable(timeoutErr))
}

func TestErrorWithContext(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeProviderError, "failed")
	err.WithProvider("openai").WithRequestID("req_123").WithStatusCode(500)
	msg := err.Error()
	assertTrue(t, strings.Contains(msg, "openai"))
	assertTrue(t, strings.Contains(msg, "req_123"))
	assertEqual(t, err.StatusCode, 500)
}

package ryn_test

import (
	"errors"
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

func TestWrapErrorf(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("inner")
	err := ryn.WrapErrorf(ryn.ErrCodeProviderError, "failed with code %d", inner, 42)
	assertNotNil(t, err)
	assertTrue(t, strings.Contains(err.Error(), "failed with code 42"))
	assertTrue(t, strings.Contains(err.Error(), "inner"))
	assertTrue(t, ryn.IsRetryable(err) == false)
}

func TestErrorUnwrap(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("root cause")
	err := ryn.WrapError(ryn.ErrCodeProviderError, "outer", inner)
	unwrapped := errors.Unwrap(err)
	assertEqual(t, unwrapped, inner)
}

func TestErrorIs(t *testing.T) {
	t.Parallel()

	target := ryn.NewError(ryn.ErrCodeRateLimited, "rate limited")
	err := ryn.NewError(ryn.ErrCodeRateLimited, "too many")

	// errors.Is should match on code
	assertTrue(t, errors.Is(err, target))

	// Different code should not match
	other := ryn.NewError(ryn.ErrCodeProviderError, "server error")
	assertEqual(t, errors.Is(other, target), false)

	// Non-Error target
	wrapped := ryn.WrapError(ryn.ErrCodeProviderError, "wrap", fmt.Errorf("base"))
	assertTrue(t, errors.Is(wrapped, fmt.Errorf("base")) == false) // Err != target
}

func TestErrorNilReceiver(t *testing.T) {
	t.Parallel()
	var e *ryn.Error
	assertEqual(t, e.Error(), "")
}

func TestErrorWithProvider(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeProviderError, "failed")
	result := err.WithProvider("anthropic")
	assertTrue(t, result == err) // same pointer
	assertTrue(t, strings.Contains(err.Error(), "anthropic"))

	// nil receiver
	var nilErr *ryn.Error
	assertEqual(t, nilErr.WithProvider("x"), nil)
}

func TestErrorWithRequestID(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeProviderError, "failed")
	err.WithRequestID("req_123")
	assertTrue(t, strings.Contains(err.Error(), "req_123"))

	// nil receiver
	var nilErr *ryn.Error
	assertEqual(t, nilErr.WithRequestID("x"), nil)
}

func TestErrorWithStatusCode(t *testing.T) {
	t.Parallel()

	err := ryn.NewError(ryn.ErrCodeProviderError, "failed")
	err.WithStatusCode(503)
	assertEqual(t, err.StatusCode, 503)

	// nil receiver
	var nilErr *ryn.Error
	assertEqual(t, nilErr.WithStatusCode(500), nil)
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

	// Non-ryn errors return false
	plainErr := fmt.Errorf("plain error")
	assertEqual(t, ryn.IsRetryable(plainErr), false)
	assertEqual(t, ryn.IsRateLimited(plainErr), false)
	assertEqual(t, ryn.IsTimeout(plainErr), false)
	assertEqual(t, ryn.IsAuthError(plainErr), false)
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

func TestConvertHTTPStatusToCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		code   ryn.ErrorCode
	}{
		{401, ryn.ErrCodeAuthenticationFailed},
		{404, ryn.ErrCodeModelNotFound},
		{429, ryn.ErrCodeRateLimited},
		{400, ryn.ErrCodeInvalidRequest},
		{422, ryn.ErrCodeInvalidRequest},
		{503, ryn.ErrCodeServiceUnavailable},
		{504, ryn.ErrCodeTimeout},
		{500, ryn.ErrCodeProviderError},
		{502, ryn.ErrCodeProviderError},
		{200, ryn.ErrCodeInternalError},
	}

	for _, tc := range cases {
		got := ryn.ConvertHTTPStatusToCode(tc.status)
		if got != tc.code {
			t.Errorf("ConvertHTTPStatusToCode(%d) = %v, want %v", tc.status, got, tc.code)
		}
	}
}

func TestRetryableCodes(t *testing.T) {
	t.Parallel()

	retryable := []ryn.ErrorCode{
		ryn.ErrCodeRateLimited,
		ryn.ErrCodeServiceUnavailable,
		ryn.ErrCodeTimeout,
		ryn.ErrCodeStreamError,
	}
	for _, code := range retryable {
		err := ryn.NewError(code, "test")
		assertTrue(t, ryn.IsRetryable(err))
	}

	nonRetryable := []ryn.ErrorCode{
		ryn.ErrCodeInvalidRequest,
		ryn.ErrCodeAuthenticationFailed,
		ryn.ErrCodeModelNotFound,
		ryn.ErrCodeProviderError,
	}
	for _, code := range nonRetryable {
		err := ryn.NewError(code, "test")
		assertEqual(t, ryn.IsRetryable(err), false)
	}
}

package niro_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestErrorCreation(t *testing.T) {
	t.Parallel()

	err := niro.NewError(niro.ErrCodeInvalidRequest, "test error")
	assertNotNil(t, err)
	assertEqual(t, err.Code, niro.ErrCodeInvalidRequest)
	assertEqual(t, err.Message, "test error")
	assertTrue(t, err.Error() != "")
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("inner error")
	err := niro.WrapError(niro.ErrCodeProviderError, "provider failed", inner)
	assertTrue(t, err != nil)

	// Check error message contains both
	msg := err.Error()
	assertTrue(t, strings.Contains(msg, "provider failed"))
	assertTrue(t, strings.Contains(msg, "inner error"))
}

func TestWrapErrorf(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("inner")
	err := niro.WrapErrorf(niro.ErrCodeProviderError, "failed with code %d", inner, 42)
	assertNotNil(t, err)
	assertTrue(t, strings.Contains(err.Error(), "failed with code 42"))
	assertTrue(t, strings.Contains(err.Error(), "inner"))
	assertTrue(t, niro.IsRetryable(err) == false)
}

func TestErrorUnwrap(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("root cause")
	err := niro.WrapError(niro.ErrCodeProviderError, "outer", inner)
	unwrapped := errors.Unwrap(err)
	assertEqual(t, unwrapped, inner)
}

func TestErrorIs(t *testing.T) {
	t.Parallel()

	target := niro.NewError(niro.ErrCodeRateLimited, "rate limited")
	err := niro.NewError(niro.ErrCodeRateLimited, "too many")

	// errors.Is should match on code
	assertTrue(t, errors.Is(err, target))

	// Different code should not match
	other := niro.NewError(niro.ErrCodeProviderError, "server error")
	assertEqual(t, errors.Is(other, target), false)

	// Non-Error target
	wrapped := niro.WrapError(niro.ErrCodeProviderError, "wrap", fmt.Errorf("base"))
	assertTrue(t, errors.Is(wrapped, fmt.Errorf("base")) == false) // Err != target
}

func TestErrorNilReceiver(t *testing.T) {
	t.Parallel()
	var e *niro.Error
	assertEqual(t, e.Error(), "")
}

func TestErrorWithProvider(t *testing.T) {
	t.Parallel()

	err := niro.NewError(niro.ErrCodeProviderError, "failed")
	result := err.WithProvider("anthropic")
	assertTrue(t, result == err) // same pointer
	assertTrue(t, strings.Contains(err.Error(), "anthropic"))

	// nil receiver
	var nilErr *niro.Error
	assertEqual(t, nilErr.WithProvider("x"), nil)
}

func TestErrorWithRequestID(t *testing.T) {
	t.Parallel()

	err := niro.NewError(niro.ErrCodeProviderError, "failed")
	err.WithRequestID("req_123")
	assertTrue(t, strings.Contains(err.Error(), "req_123"))

	// nil receiver
	var nilErr *niro.Error
	assertEqual(t, nilErr.WithRequestID("x"), nil)
}

func TestErrorWithStatusCode(t *testing.T) {
	t.Parallel()

	err := niro.NewError(niro.ErrCodeProviderError, "failed")
	err.WithStatusCode(503)
	assertEqual(t, err.StatusCode, 503)

	// nil receiver
	var nilErr *niro.Error
	assertEqual(t, nilErr.WithStatusCode(500), nil)
}

func TestErrorCheckers(t *testing.T) {
	t.Parallel()

	rateLimitErr := niro.NewError(niro.ErrCodeRateLimited, "too many requests")
	assertTrue(t, niro.IsRetryable(rateLimitErr))
	assertTrue(t, niro.IsRateLimited(rateLimitErr))
	assertEqual(t, niro.IsTimeout(rateLimitErr), false)

	authErr := niro.NewError(niro.ErrCodeAuthenticationFailed, "invalid key")
	assertTrue(t, niro.IsAuthError(authErr))
	assertEqual(t, niro.IsRetryable(authErr), false)

	timeoutErr := niro.NewError(niro.ErrCodeTimeout, "took too long")
	assertTrue(t, niro.IsTimeout(timeoutErr))
	assertTrue(t, niro.IsRetryable(timeoutErr))

	// Non-niro errors return false
	plainErr := fmt.Errorf("plain error")
	assertEqual(t, niro.IsRetryable(plainErr), false)
	assertEqual(t, niro.IsRateLimited(plainErr), false)
	assertEqual(t, niro.IsTimeout(plainErr), false)
	assertEqual(t, niro.IsAuthError(plainErr), false)
}

func TestErrorWithContext(t *testing.T) {
	t.Parallel()

	err := niro.NewError(niro.ErrCodeProviderError, "failed")
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
		code   niro.ErrorCode
	}{
		{401, niro.ErrCodeAuthenticationFailed},
		{404, niro.ErrCodeModelNotFound},
		{429, niro.ErrCodeRateLimited},
		{400, niro.ErrCodeInvalidRequest},
		{422, niro.ErrCodeInvalidRequest},
		{503, niro.ErrCodeServiceUnavailable},
		{504, niro.ErrCodeTimeout},
		{500, niro.ErrCodeProviderError},
		{502, niro.ErrCodeProviderError},
		{200, niro.ErrCodeInternalError},
	}

	for _, tc := range cases {
		got := niro.ConvertHTTPStatusToCode(tc.status)
		if got != tc.code {
			t.Errorf("ConvertHTTPStatusToCode(%d) = %v, want %v", tc.status, got, tc.code)
		}
	}
}

func TestRetryableCodes(t *testing.T) {
	t.Parallel()

	retryable := []niro.ErrorCode{
		niro.ErrCodeRateLimited,
		niro.ErrCodeServiceUnavailable,
		niro.ErrCodeTimeout,
		niro.ErrCodeStreamError,
	}
	for _, code := range retryable {
		err := niro.NewError(code, "test")
		assertTrue(t, niro.IsRetryable(err))
	}

	nonRetryable := []niro.ErrorCode{
		niro.ErrCodeInvalidRequest,
		niro.ErrCodeAuthenticationFailed,
		niro.ErrCodeModelNotFound,
		niro.ErrCodeProviderError,
	}
	for _, code := range nonRetryable {
		err := niro.NewError(code, "test")
		assertEqual(t, niro.IsRetryable(err), false)
	}
}

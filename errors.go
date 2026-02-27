package ryn

import (
	"errors"
	"fmt"
)

// ErrorCode categorizes runtime errors for proper handling.
type ErrorCode int

const (
	// Client errors (4xx)
	ErrCodeInvalidRequest       ErrorCode = 400
	ErrCodeAuthenticationFailed ErrorCode = 401
	ErrCodeModelNotFound        ErrorCode = 404
	ErrCodeInvalidModel         ErrorCode = 422
	ErrCodeInsufficientQuota    ErrorCode = 429

	// Server errors (5xx)
	ErrCodeProviderError      ErrorCode = 500
	ErrCodeServiceUnavailable ErrorCode = 503
	ErrCodeRateLimited        ErrorCode = 509
	ErrCodeTimeout            ErrorCode = 504
	ErrCodeInternalError      ErrorCode = 510

	// Ryn-specific errors (6xx)
	ErrCodeStreamClosed       ErrorCode = 600
	ErrCodeNoStructuredOutput ErrorCode = 601
	ErrCodeInvalidSchema      ErrorCode = 602
	ErrCodeContextCancelled   ErrorCode = 603
	ErrCodeStreamError        ErrorCode = 604
)

// Error represents a detailed error from Ryn or a provider.
type Error struct {
	Code       ErrorCode
	Message    string
	Err        error  // underlying error for error chaining
	Provider   string // which provider failed (if applicable)
	RequestID  string // trace ID for debugging
	Retryable  bool   // whether the operation can be safely retried
	StatusCode int    // HTTP status code (if applicable)
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Provider != "" {
		msg = e.Provider + ": " + msg
	}
	if e.RequestID != "" {
		msg += " (id:" + e.RequestID + ")"
	}
	return msg
}

// Unwrap returns the underlying error for error chaining.
func (e *Error) Unwrap() error {
	return e.Err
}

// Is implements errors.Is for semantic error matching.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return e.Err == target
	}
	return e.Code == t.Code
}

// --- Error constructor helpers ---

// NewError creates a new Error with the given code and message.
func NewError(code ErrorCode, msg string) *Error {
	return &Error{
		Code:      code,
		Message:   msg,
		Retryable: isRetryable(code),
	}
}

// NewErrorf creates a new Error with formatted message.
func NewErrorf(code ErrorCode, format string, args ...interface{}) *Error {
	return &Error{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: isRetryable(code),
	}
}

// WrapError wraps an existing error with context.
func WrapError(code ErrorCode, msg string, err error) *Error {
	return &Error{
		Code:      code,
		Message:   msg,
		Err:       err,
		Retryable: isRetryable(code),
	}
}

// WrapErrorf wraps an error with formatted message.
func WrapErrorf(code ErrorCode, format string, err error, args ...interface{}) *Error {
	return &Error{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Err:       err,
		Retryable: isRetryable(code),
	}
}

// isRetryable determines if an error code represents a retryable failure.
func isRetryable(code ErrorCode) bool {
	switch code {
	case ErrCodeRateLimited, ErrCodeServiceUnavailable, ErrCodeTimeout, ErrCodeStreamError:
		return true
	default:
		return false
	}
}

// --- Predefined errors ---

var (
	ErrClosed             = NewError(ErrCodeStreamClosed, "stream closed")
	ErrNoStructuredOutput = NewError(ErrCodeNoStructuredOutput, "no structured output")
	ErrContextCancelled   = NewError(ErrCodeContextCancelled, "context cancelled")
)

// IsRetryable reports whether an error is retryable.
func IsRetryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

// IsRateLimited reports whether an error is a rate limit error.
func IsRateLimited(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == ErrCodeRateLimited
	}
	return false
}

// IsTimeout reports whether an error is a timeout error.
func IsTimeout(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == ErrCodeTimeout
	}
	return false
}

// IsAuthError reports whether an error is an authentication error.
func IsAuthError(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == ErrCodeAuthenticationFailed
	}
	return false
}

// WithProvider adds provider context to an error.
func (e *Error) WithProvider(provider string) *Error {
	if e == nil {
		return nil
	}
	e.Provider = provider
	return e
}

// WithRequestID adds request/trace ID context to an error.
func (e *Error) WithRequestID(id string) *Error {
	if e == nil {
		return nil
	}
	e.RequestID = id
	return e
}

// WithStatusCode adds HTTP status code context to an error.
func (e *Error) WithStatusCode(code int) *Error {
	if e == nil {
		return nil
	}
	e.StatusCode = code
	return e
}

// ConvertHTTPStatusToCode maps HTTP status codes to ErrorCode.
func ConvertHTTPStatusToCode(statusCode int) ErrorCode {
	switch {
	case statusCode >= 400 && statusCode < 500:
		switch statusCode {
		case 401:
			return ErrCodeAuthenticationFailed
		case 404:
			return ErrCodeModelNotFound
		case 429:
			return ErrCodeRateLimited
		default:
			return ErrCodeInvalidRequest
		}
	case statusCode >= 500:
		switch statusCode {
		case 503:
			return ErrCodeServiceUnavailable
		case 504:
			return ErrCodeTimeout
		default:
			return ErrCodeProviderError
		}
	default:
		return ErrCodeInternalError
	}
}

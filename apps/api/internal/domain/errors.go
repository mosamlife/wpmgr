// Package domain holds cross-cutting domain primitives: typed errors shared by
// all domains, a Clock abstraction, and the canonical error->HTTP mapping used
// by handlers.
package domain

import (
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies a domain error so the HTTP layer can map it to a status code
// without importing every domain package.
type Kind int

const (
	// KindInternal is an unexpected server-side failure (HTTP 500).
	KindInternal Kind = iota
	// KindNotFound indicates a requested resource does not exist (HTTP 404).
	KindNotFound
	// KindConflict indicates a uniqueness/state conflict (HTTP 409).
	KindConflict
	// KindValidation indicates the request failed validation (HTTP 422).
	KindValidation
	// KindForbidden indicates the caller lacks permission (HTTP 403).
	KindForbidden
	// KindUnauthorized indicates the caller is not authenticated (HTTP 401).
	KindUnauthorized
	// KindUnavailable indicates a dependency/feature is disabled (HTTP 501).
	KindUnavailable
	// KindServiceUnavailable indicates a transient inability to serve a request
	// (HTTP 503). Use when optional plumbing isn't wired or a dep is mid-restart,
	// so the caller can retry later. Distinct from KindUnavailable (501 = feature
	// permanently disabled in this build).
	KindServiceUnavailable
	// KindGone indicates a resource was once present but is no longer available
	// (HTTP 410). Used by single-shot consume paths (e.g. autologin nonces) so
	// the caller can distinguish "never existed" from "already used / expired".
	KindGone
	// KindRateLimited indicates the request exceeded a per-key rate-limit budget
	// (HTTP 429). Carries the retry budget in the message.
	KindRateLimited
)

// Error is a domain error carrying a Kind, a stable machine code, a
// human-readable message, and an optional wrapped cause.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.cause)
	}
	return e.Message
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches an underlying cause and returns the error for chaining.
func (e *Error) WithCause(cause error) *Error {
	e.cause = cause
	return e
}

// NotFound builds a KindNotFound error.
func NotFound(code, msg string) *Error {
	return &Error{Kind: KindNotFound, Code: code, Message: msg}
}

// Conflict builds a KindConflict error.
func Conflict(code, msg string) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: msg}
}

// Validation builds a KindValidation error.
func Validation(code, msg string) *Error {
	return &Error{Kind: KindValidation, Code: code, Message: msg}
}

// Forbidden builds a KindForbidden error.
func Forbidden(code, msg string) *Error {
	return &Error{Kind: KindForbidden, Code: code, Message: msg}
}

// Unauthorized builds a KindUnauthorized error.
func Unauthorized(code, msg string) *Error {
	return &Error{Kind: KindUnauthorized, Code: code, Message: msg}
}

// Unavailable builds a KindUnavailable error (e.g. a disabled feature).
func Unavailable(code, msg string) *Error {
	return &Error{Kind: KindUnavailable, Code: code, Message: msg}
}

// ServiceUnavailable builds a KindServiceUnavailable error (HTTP 503). Use for
// transient-or-optional plumbing gaps (e.g. an inspection tier wasn't wired in
// this CP build) where the operator should treat the result as "try again
// later or accept the degraded experience", not as a server bug.
func ServiceUnavailable(code, msg string) *Error {
	return &Error{Kind: KindServiceUnavailable, Code: code, Message: msg}
}

// Internal builds a KindInternal error.
func Internal(code, msg string) *Error {
	return &Error{Kind: KindInternal, Code: code, Message: msg}
}

// Gone builds a KindGone error (HTTP 410). Use for one-shot resources that are
// no longer present (e.g. a single-use nonce that was already consumed).
func Gone(code, msg string) *Error {
	return &Error{Kind: KindGone, Code: code, Message: msg}
}

// RateLimited builds a KindRateLimited error (HTTP 429). The retryAfter is
// stashed on the message for transparency; handlers usually surface it as a
// structured field in the response body too.
func RateLimited(code, msg string) *Error {
	return &Error{Kind: KindRateLimited, Code: code, Message: msg}
}

// HTTPStatus maps an error to an HTTP status code. Non-domain errors are 500.
func HTTPStatus(err error) int {
	var de *Error
	if errors.As(err, &de) {
		switch de.Kind {
		case KindNotFound:
			return http.StatusNotFound
		case KindConflict:
			return http.StatusConflict
		case KindValidation:
			return http.StatusUnprocessableEntity
		case KindForbidden:
			return http.StatusForbidden
		case KindUnauthorized:
			return http.StatusUnauthorized
		case KindUnavailable:
			return http.StatusNotImplemented
		case KindServiceUnavailable:
			return http.StatusServiceUnavailable
		case KindGone:
			return http.StatusGone
		case KindRateLimited:
			return http.StatusTooManyRequests
		default:
			return http.StatusInternalServerError
		}
	}
	return http.StatusInternalServerError
}

// AsDomain returns the underlying *Error if err is (or wraps) one.
func AsDomain(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

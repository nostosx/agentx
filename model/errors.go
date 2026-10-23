package model

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"syscall"
	"time"
)

// APIError is an error returned by a model provider's API.
type APIError struct {
	Provider   string
	StatusCode int
	// Type is the provider's error type, or its code when it reports no
	// type.
	Type string
	// Code is the provider's error code, when it reports one alongside or
	// instead of a type.
	Code    string
	Message string
	// RetryAfter is the delay requested by the provider, when available.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Type != "" {
		return fmt.Sprintf("%s: %d %s: %s", e.Provider, e.StatusCode, e.Type, msg)
	}
	return fmt.Sprintf("%s: %d: %s", e.Provider, e.StatusCode, msg)
}

// quotaErrors are error types and codes that providers send with status
// 429 when billing, spend or usage limits are exhausted. Unlike rate limits,
// retrying does not help.
var quotaErrors = map[string]bool{
	"insufficient_quota":                true,
	"credit_balance_exhausted":          true,
	"organization_spend_limit_exceeded": true,
	"project_spend_limit_exceeded":      true,
	"organization_usage_limit_exceeded": true,
}

// Retryable reports whether the request may succeed if retried.
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests:
		return !quotaErrors[e.Type] && !quotaErrors[e.Code]
	case http.StatusRequestTimeout, http.StatusConflict:
		return true
	}
	// Server errors include the non-standard 529 "overloaded" status.
	return e.StatusCode >= 500
}

// PermanentError marks an error that retrying cannot fix, such as a request
// that fails validation before it is sent.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so that IsRetryable reports false for it. It returns
// nil for a nil err.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsRetryable reports whether err may succeed if the request is retried.
//
//   - Provider errors follow APIError.Retryable.
//   - Transport failures are retryable: errors from http.Client.Do, which
//     come as *url.Error, and failures reading a response body, which the
//     agentx providers also report as *url.Error. This covers refused,
//     reset and dropped connections, HTTP/2 stream resets and GOAWAY,
//     connections closed early and transport timeouts, including
//     http.Client.Timeout and Transport.ResponseHeaderTimeout. Network
//     errors outside a *url.Error, such as *net.OpError, unexpected EOF or
//     a net.Error timeout, are retryable too.
//   - Failures that recur on every attempt are not retryable even then:
//     invalid URLs, certificate verification failures, TLS alerts from the
//     server, other TLS handshake failures, DNS lookups of unknown hosts and
//     a plain HTTP response to an HTTPS request.
//   - The caller's context being cancelled or past its deadline is not
//     retryable. A timeout of the HTTP client is not the caller's deadline
//     and is retryable, although it also matches context.DeadlineExceeded.
//     Retry loops should still stop once their context is done.
//   - Errors marked Permanent and all other errors, such as invalid requests
//     or undecodable responses, are not retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || isContextDeadline(err) {
		return false
	}
	if _, ok := errors.AsType[*PermanentError](err); ok {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Retryable()
	}
	if isPermanentFailure(err) {
		return false
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	return isTransient(err)
}

// isContextDeadline reports whether err holds context.DeadlineExceeded
// itself, as a context past its deadline returns. Timeouts of the HTTP
// client also match context.DeadlineExceeded through errors.Is, but they
// are distinct errors.
func isContextDeadline(err error) bool {
	if err == context.DeadlineExceeded { //nolint:errorlint // Identity, not errors.Is, tells the two apart.
		return true
	}
	switch u := err.(type) { //nolint:errorlint // The chain is walked here.
	case interface{ Unwrap() error }:
		return u.Unwrap() != nil && isContextDeadline(u.Unwrap())
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(u.Unwrap(), isContextDeadline)
	}
	return false
}

// isPermanentFailure reports whether err is a transport failure that recurs
// on every attempt.
func isPermanentFailure(err error) bool {
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr.Op == "parse" {
		return true
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
		return true
	}
	// A TLS alert received from the server, such as a rejected protocol
	// version or client certificate.
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "remote error" {
		return true
	}
	if _, ok := errors.AsType[tls.AlertError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.SystemRootsError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return true
	}
	return errors.Is(err, http.ErrSchemeMismatch)
}

// isTransient reports whether err, which did not come from an HTTP client,
// is a network failure that may not recur.
func isTransient(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	// Failed dials, reads and writes.
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}

// RetryAfter returns the delay requested by the provider in err, or 0.
func RetryAfter(err error) time.Duration {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.RetryAfter
	}
	return 0
}

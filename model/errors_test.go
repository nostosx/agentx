package model_test

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
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/nostosx/agentx/model"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"}, {name: "canceled", err: fmt.Errorf("wrapped: %w", context.Canceled)},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "bad request", err: &model.APIError{StatusCode: 400}},
		{name: "timeout", err: &model.APIError{StatusCode: 408}, want: true},
		{name: "conflict", err: &model.APIError{StatusCode: 409}, want: true},
		{name: "rate limit", err: fmt.Errorf("wrapped: %w", &model.APIError{StatusCode: 429}), want: true},
		{name: "overloaded", err: &model.APIError{StatusCode: 529}, want: true},
		{name: "quota type", err: &model.APIError{StatusCode: 429, Type: "insufficient_quota"}},
		{name: "quota code", err: &model.APIError{StatusCode: 429, Type: "invalid_request_error", Code: "organization_spend_limit_exceeded"}},
		// Local failures do not change on retry.
		{name: "plain", err: errors.New("openai: response has no choices")},
		{name: "decode", err: fmt.Errorf("decode response: %w", json.Unmarshal([]byte("{"), new(any)))},
		{name: "empty body", err: fmt.Errorf("decode response: %w", io.EOF)},
		{name: "bad url", err: &url.Error{Op: "parse", URL: "::", Err: errors.New("missing protocol scheme")}},
		{name: "permanent", err: model.Permanent(&model.APIError{StatusCode: 503})},
		// Transient network failures.
		{name: "truncated", err: fmt.Errorf("decode response: %w", io.ErrUnexpectedEOF), want: true},
		{name: "closed early", err: &url.Error{Op: "Post", URL: "https://x", Err: io.EOF}, want: true},
		{name: "reset", err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, want: true},
		{name: "refused", err: fmt.Errorf("x: %w", syscall.ECONNREFUSED), want: true},
		{name: "net timeout", err: &url.Error{Op: "Post", URL: "https://x", Err: timeoutError{}}, want: true},
		{name: "dns not found", err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}},
		{name: "dns timeout", err: &net.DNSError{Err: "timeout", IsTimeout: true}, want: true},
		// net/http reports these as plain values inside *url.Error.
		{name: "http2 stream reset", err: postErr(errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")), want: true},
		{name: "http2 connection lost", err: postErr(errors.New("http2: client connection lost")), want: true},
		{name: "http2 goaway", err: postErr(errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=\"\"")), want: true},
		{name: "idle connection closed", err: postErr(errors.New("http: server closed idle connection")), want: true},
		{name: "body read reset", err: fmt.Errorf("openai: read stream: %w", &url.Error{Op: "read", URL: "https://x", Err: errors.New("stream error: stream ID 3; INTERNAL_ERROR")}), want: true},
		// Transport failures that recur on every attempt.
		{name: "tls alert", err: postErr(&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")})},
		{name: "quic tls alert", err: postErr(tls.AlertError(42))},
		{name: "tls record header", err: postErr(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})},
		{name: "unknown authority", err: postErr(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}})},
		{name: "hostname mismatch", err: postErr(x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"})},
		{name: "dns not found in url error", err: postErr(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}})},
		{name: "scheme mismatch", err: postErr(http.ErrSchemeMismatch)},
		{name: "json syntax in url error", err: postErr(&json.SyntaxError{})},
		{name: "deadline in url error", err: postErr(context.DeadlineExceeded)},
		{name: "canceled in url error", err: postErr(context.Canceled)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := model.IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func postErr(err error) error {
	return fmt.Errorf("openai: %w", &url.Error{Op: "Post", URL: "https://x/v1/responses", Err: err})
}

// TestIsRetryableHTTP checks the errors net/http actually returns.
func TestIsRetryableHTTP(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/abort":
			// Over HTTP/2 this resets the stream.
			panic(http.ErrAbortHandler)
		case "/slow":
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	do := func(t *testing.T, c *http.Client, ctx context.Context, path string) error {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: no error", path)
		}
		return err
	}
	t.Run("http2 abort", func(t *testing.T) {
		t.Parallel()
		if err := do(t, srv.Client(), t.Context(), "/abort"); !model.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false", err)
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		t.Parallel()
		c := *srv.Client()
		c.Timeout = 50 * time.Millisecond
		err := do(t, &c, t.Context(), "/slow")
		if !errors.Is(err, context.DeadlineExceeded) || !model.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false", err)
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := do(t, srv.Client(), ctx, "/slow")
		if !errors.Is(err, context.DeadlineExceeded) || model.IsRetryable(err) {
			t.Errorf("caller deadline = %v, retryable = %v", err, model.IsRetryable(err))
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		t.Parallel()
		if err := do(t, &http.Client{}, t.Context(), "/"); model.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = true", err)
		}
	})
}

func TestPermanent(t *testing.T) {
	t.Parallel()
	inner := errors.New("invalid request")
	err := model.Permanent(inner)
	if err.Error() != "invalid request" || !errors.Is(err, inner) || model.Permanent(nil) != nil {
		t.Fatalf("Permanent = %v", err)
	}
}

func TestAPIErrorAndRetryAfter(t *testing.T) {
	t.Parallel()
	err := &model.APIError{Provider: "test", StatusCode: 429, Type: "rate_limit", Message: "slow down", RetryAfter: 2 * time.Second}
	if err.Error() != "test: 429 rate_limit: slow down" || model.RetryAfter(fmt.Errorf("wrapped: %w", err)) != 2*time.Second || model.RetryAfter(errors.New("other")) != 0 {
		t.Fatalf("unexpected API error: %v", err)
	}
	if got := (&model.APIError{Provider: "test", StatusCode: 503}).Error(); got != "test: 503: Service Unavailable" {
		t.Errorf("fallback error = %q", got)
	}
}

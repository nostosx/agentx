package workflow

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/nostosx/agentx/model"
)

// ErrNodeTimeout is matched, together with context.DeadlineExceeded, by
// the error of a node attempt that exceeded its Timeout.
var ErrNodeTimeout = errors.New("workflow: node attempt timed out")

// RetryPolicy configures how a failed node is retried; see WithRetry.
type RetryPolicy struct {
	// MaxAttempts includes the first attempt. Defaults to 3.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry, 500ms by default;
	// each further wait is Multiplier, 2 by default, times longer, up to
	// MaxBackoff, 30s by default. A provider's Retry-After is honored, up
	// to MaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	// Jitter randomizes each wait by up to this fraction. Defaults to 0.2;
	// set a negative value to disable.
	Jitter float64
	// RetryIf decides whether an error is retried. By default every error
	// is; Transient retries only errors that may not recur.
	RetryIf func(error) bool

	// linear and backoff implement Retry: waits grow linearly and every
	// error is retried.
	linear  bool
	backoff time.Duration
}

// Transient reports whether a node error may not recur: an attempt that
// exceeded its Timeout, or an error model.IsRetryable accepts, such as a
// rate limit, a server error or a dropped connection.
func Transient(err error) bool {
	return errors.Is(err, ErrNodeTimeout) || model.IsRetryable(err)
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.linear {
		return p
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = 500 * time.Millisecond
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = 30 * time.Second
	}
	if p.Multiplier < 1 {
		p.Multiplier = 2
	}
	if p.Jitter == 0 {
		p.Jitter = 0.2
	}
	return p
}

// retryable reports whether err, from the given attempt, is retried.
func (p RetryPolicy) retryable(attempt int, err error) bool {
	if attempt >= p.MaxAttempts {
		return false
	}
	return p.RetryIf == nil || p.RetryIf(err)
}

// delay returns the wait before the given retry (1-based) after err.
func (p RetryPolicy) delay(retry int, err error) time.Duration {
	if p.linear {
		return p.backoff * time.Duration(retry)
	}
	b := min(float64(p.InitialBackoff)*math.Pow(p.Multiplier, float64(retry-1)), float64(p.MaxBackoff))
	if p.Jitter > 0 {
		b *= 1 + p.Jitter*(2*rand.Float64()-1)
	}
	d := time.Duration(b)
	if after := model.RetryAfter(err); after > d {
		d = min(after, p.MaxBackoff)
	}
	return d
}

// wait sleeps for d or until ctx is done.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NodeOption configures how a graph runs a node.
type NodeOption func(*nodeSpec)

// Retry retries a failed node up to attempts-1 more times, waiting backoff
// multiplied by the attempt number between attempts. Every error is
// retried. WithRetry offers exponential backoff and error classification.
func Retry(attempts int, backoff time.Duration) NodeOption {
	return func(n *nodeSpec) {
		n.retry = RetryPolicy{MaxAttempts: attempts, linear: true, backoff: backoff}
	}
}

// WithRetry retries a failed node according to p, with exponential backoff
// and jitter. Cancellation of the run and suspensions are never retried.
func WithRetry(p RetryPolicy) NodeOption {
	return func(n *nodeSpec) { n.retry = p.withDefaults() }
}

// Timeout bounds each attempt of a node. An attempt that exceeds it fails
// with an error matching ErrNodeTimeout and context.DeadlineExceeded.
func Timeout(d time.Duration) NodeOption { return func(n *nodeSpec) { n.timeout = d } }

// OnError routes to fallback when the node fails after its retries,
// instead of failing the run. The node's writes are discarded, its error
// message is stored under "<node>.error" and only fallback is taken among
// its successors. Cancellation and suspensions do not fall back. The
// fallback edge counts as an edge of the graph: it may not close a cycle.
func OnError(fallback string) NodeOption { return func(n *nodeSpec) { n.fallback = fallback } }

// ErrorKey returns the state key under which OnError stores the error of
// node.
func ErrorKey(node string) string { return node + ".error" }

type nodeSpec struct {
	name     string
	node     Node
	retry    RetryPolicy
	timeout  time.Duration
	fallback string
}

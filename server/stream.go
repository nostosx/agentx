package server

import (
	"context"
	"iter"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nostosx/agentx/loop"
)

// encoder writes the events of a run in one wire format.
type encoder interface {
	// header sets the response headers.
	header(h http.Header)
	// event writes an event of the run.
	event(w *sseWriter, ev *loop.Event)
	// fail writes an error of the run.
	fail(w *sseWriter, err error)
	// end finishes the stream after the run's last event.
	end(w *sseWriter)
	// heartbeat keeps an idle stream open.
	heartbeat(w *sseWriter)
	// inStream reports whether errors before the first event are written
	// to the stream rather than as an HTTP error response.
	inStream() bool
}

type item struct {
	ev  *loop.Event
	err error
}

// pump iterates the events of a run in its own goroutine, so the handler
// can send heartbeats and notice a client that went away while the run
// waits.
type pump struct {
	items chan item
	quit  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// startPump runs seq for a session that the caller marked busy; the pump
// releases it once the run has ended and its session is saved. The
// session's interrupt is recorded whether or not a client still listens.
func (s *Server) startPump(sessionID string, seq iter.Seq2[*loop.Event, error]) *pump {
	p := &pump{items: make(chan item), quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer s.release(sessionID)
		defer close(p.items)
		for ev, err := range seq {
			if ev != nil {
				s.observe(sessionID, ev)
			}
			select {
			case p.items <- item{ev, err}:
			case <-p.quit:
				// Stopping the iteration cancels the run; the runner saves
				// the session before the range statement returns.
				s.forget(sessionID)
				return
			}
		}
	}()
	return p
}

// stop stops the run, if it is still going, and waits until its session
// is saved.
func (p *pump) stop() {
	p.once.Do(func() { close(p.quit) })
	<-p.done
}

// serveStream streams the events of a run of a session that the caller
// marked busy. If the client goes away, the run is stopped as if its
// consumer stopped iterating: it ends as a failed run that POST
// /sessions/{id}/resume continues, given a checkpointer.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, sessionID string, seq iter.Seq2[*loop.Event, error], enc encoder) {
	ctx := r.Context()
	p := s.startPump(sessionID, seq)
	defer p.stop()
	var first item
	select {
	case it, ok := <-p.items:
		if !ok {
			first = item{err: errNoResult}
		} else {
			first = it
		}
	case <-ctx.Done():
		return
	}
	if first.err != nil && !enc.inStream() {
		writeError(w, s.runError(ctx, sessionID, first.err))
		return
	}
	enc.header(w.Header())
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	sw := newSSEWriter(w)
	write := func(it item) {
		if it.err != nil {
			enc.fail(sw, it.err)
		} else {
			enc.event(sw, it.ev)
		}
	}
	write(first)
	if sw.flush() != nil {
		return
	}
	var tick <-chan time.Time
	if s.cfg.heartbeat > 0 {
		t := time.NewTicker(s.cfg.heartbeat)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case it, ok := <-p.items:
			if !ok {
				enc.end(sw)
				_ = sw.flush()
				return
			}
			write(it)
		case <-tick:
			enc.heartbeat(sw)
		case <-ctx.Done():
			return
		}
		if sw.flush() != nil {
			return
		}
	}
}

// runError maps an error that ended a run before its first event.
func (s *Server) runError(ctx context.Context, sessionID string, err error) *Error {
	e := classify(err)
	if e.Status != http.StatusInternalServerError {
		return e
	}
	// The runner reports a session that cannot run or resume in plain
	// errors; the session's state tells which.
	if sess, gerr := s.runner.Store().Get(ctx, sessionID); gerr == nil {
		if p := s.pendingRun(ctx, sess); p != nil && p.Kind == "interrupted" {
			return errorf(http.StatusConflict, CodeRunInterrupted, "%v", err)
		}
	}
	return e
}

// nativeEncoder streams runtime events as Server-Sent Events named after
// their type, with sequence numbers as IDs.
type nativeEncoder struct{ seq int }

func (*nativeEncoder) header(h http.Header) { h.Set("Content-Type", "text/event-stream") }

func (e *nativeEncoder) event(w *sseWriter, ev *loop.Event) {
	e.seq++
	w.event(string(ev.Type), strconv.Itoa(e.seq), wireEvent(ev))
}

func (e *nativeEncoder) fail(w *sseWriter, err error) {
	e.seq++
	w.event("error", strconv.Itoa(e.seq), newErrorRecord(err))
}

func (*nativeEncoder) end(*sseWriter)         {}
func (*nativeEncoder) heartbeat(w *sseWriter) { w.comment("ping") }
func (*nativeEncoder) inStream() bool         { return false }

// jsonlEncoder streams runtime events as JSON Lines, like WriteJSONL.
type jsonlEncoder struct{}

func (jsonlEncoder) header(h http.Header) { h.Set("Content-Type", "application/x-ndjson") }

func (jsonlEncoder) event(w *sseWriter, ev *loop.Event) { writeLine(w, wireEvent(ev)) }

func (jsonlEncoder) fail(w *sseWriter, err error) { writeLine(w, newErrorRecord(err)) }

func (jsonlEncoder) end(*sseWriter)       {}
func (jsonlEncoder) heartbeat(*sseWriter) {}
func (jsonlEncoder) inStream() bool       { return false }

func writeLine(w *sseWriter, v any) {
	b, err := marshal(v)
	if err != nil {
		w.fail(err)
		return
	}
	w.write(string(b) + "\n")
}

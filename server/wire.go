package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/nostosx/agentx/loop"
)

// wireEvent returns the event as it is sent to clients: Result.Messages,
// the whole conversation, and the run state in interrupts, which holds the
// system prompt, are left out, and the arguments of a tool call delta, a
// fragment of JSON text, are a JSON string. ev is not modified.
func wireEvent(ev *loop.Event) *loop.Event {
	d := ev.Delta
	argsDelta := d != nil && d.Delta != nil && d.Delta.ToolCall != nil && len(d.Delta.ToolCall.Arguments) > 0
	if ev.Result == nil && ev.Interrupt == nil && !argsDelta {
		return ev
	}
	out := *ev
	out.Interrupt = wireInterrupt(ev.Interrupt)
	out.Result = wireResult(ev.Result)
	if argsDelta {
		se, part, call := *d, *d.Delta, *d.Delta.ToolCall
		call.Arguments, _ = json.Marshal(string(call.Arguments))
		part.ToolCall = &call
		se.Delta = &part
		out.Delta = &se
	}
	return &out
}

// wireResult returns res without Messages, interrupt state and run
// values, which hold middleware state such as permission grants.
func wireResult(res *loop.Result) *loop.Result {
	if res == nil {
		return nil
	}
	out := *res
	out.Messages, out.Values = nil, nil
	out.Interrupt = wireInterrupt(res.Interrupt)
	return &out
}

// wireInterrupt returns intr without its state.
func wireInterrupt(intr *loop.Interrupt) *loop.Interrupt {
	if intr == nil {
		return nil
	}
	out := *intr
	out.State = nil
	return &out
}

// errorRecord is the record of an error in event streams and JSON Lines.
type errorRecord struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func newErrorRecord(err error) errorRecord {
	e := classify(err)
	code := e.Code
	if code == CodeInternal {
		code = CodeRunFailed
	}
	return errorRecord{Type: "error", Code: code, Message: e.Message}
}

// WriteJSONL writes the events of a run to w as JSON Lines, one event per
// line in the wire format of the package documentation, for example for a
// command's machine-readable output. An error of the run is written as a
// line {"type":"error","code":...,"message":...} and returned with the
// run's result, like loop.Collect.
func WriteJSONL(w io.Writer, seq iter.Seq2[*loop.Event, error]) (*loop.Result, error) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var res *loop.Result
	for ev, err := range seq {
		if err != nil {
			if werr := enc.Encode(newErrorRecord(err)); werr != nil {
				return res, errors.Join(err, werr)
			}
			if res == nil {
				res = &loop.Result{Status: loop.StatusFailed, Err: err}
			}
			return res, err
		}
		if ev.Type == loop.EventRunEnd && ev.Depth == 0 {
			res = ev.Result
		}
		if err := enc.Encode(wireEvent(ev)); err != nil {
			return res, err
		}
	}
	if res == nil {
		return nil, errors.New("server: run ended without a result")
	}
	return res, res.Err
}

// sseWriter writes Server-Sent Events and remembers the first write error.
type sseWriter struct {
	w   *bufio.Writer
	rc  *http.ResponseController
	err error
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: bufio.NewWriter(w), rc: http.NewResponseController(w)}
}

// event writes an event; name and id are omitted when empty.
func (s *sseWriter) event(name, id string, data any) {
	b, err := marshal(data)
	if err != nil {
		s.fail(err)
		return
	}
	s.eventRaw(name, id, string(b))
}

func (s *sseWriter) eventRaw(name, id, data string) {
	if s.err != nil {
		return
	}
	if id != "" {
		s.write("id: " + id + "\n")
	}
	if name != "" {
		s.write("event: " + name + "\n")
	}
	for line := range strings.SplitSeq(data, "\n") {
		s.write("data: " + line + "\n")
	}
	s.write("\n")
}

// comment writes a comment line, which clients ignore.
func (s *sseWriter) comment(text string) { s.write(": " + text + "\n\n") }

func (s *sseWriter) write(text string) {
	if s.err != nil {
		return
	}
	if _, err := s.w.WriteString(text); err != nil {
		s.fail(err)
	}
}

func (s *sseWriter) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

// flush sends buffered events to the client.
func (s *sseWriter) flush() error {
	if s.err == nil {
		if err := s.w.Flush(); err != nil {
			s.fail(err)
		} else if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			s.fail(err)
		}
	}
	return s.err
}

// marshal encodes v as compact JSON without HTML escaping.
func marshal(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

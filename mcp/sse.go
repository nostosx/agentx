package mcp

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"
)

// maxSSELine bounds a single line of an event stream.
const maxSSELine = 16 << 20

// sseCursor is the reconnection state of an event stream: the last event ID
// and the reconnection time the server set.
type sseCursor struct {
	lastID string
	retry  time.Duration
	// events counts the dispatched events, including those without data.
	events int
}

// readEvents parses a text/event-stream body as the SSE standard
// (https://html.spec.whatwg.org/multipage/server-sent-events.html) does.
// It updates cur with the last event ID, which every dispatched event sets,
// with or without data, and with any reconnection time a retry field sets.
// It calls fn with the event type and data of each dispatched event that has
// data, and stops when fn returns false. An event cut off by the end of the
// body is discarded.
func readEvents(r io.Reader, cur *sseCursor, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	sc.Split(scanEventLines)
	var event string
	var data []string
	hasData := false
	idBuf := cur.lastID
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			cur.lastID = idBuf
			cur.events++
			send := hasData
			ev, d := event, strings.Join(data, "\n")
			event, data, hasData = "", data[:0], false
			if send && !fn(ev, d) {
				return nil
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				idBuf = value
			}
		case "retry":
			if ms, err := strconv.ParseUint(value, 10, 31); err == nil {
				cur.retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	return sc.Err()
}

// scanEventLines splits on \n, \r\n or a lone \r, as the SSE format
// requires.
func scanEventLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if atEOF {
			return i + 1, data[:i], nil
		}
		// A \r at the end of the buffer may start a \r\n.
		return 0, nil, nil
	}
	if atEOF {
		// A final line without a terminator is not a line.
		return len(data), nil, nil
	}
	return 0, nil, nil
}

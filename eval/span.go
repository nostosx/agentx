package eval

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/telemetry"
)

// Span is a span recorded by Tracer while a task ran.
type Span struct {
	Name       string             `json:"name"`
	Kind       telemetry.SpanKind `json:"kind"`
	Attributes map[string]any     `json:"attributes,omitempty"`
	Start      time.Time          `json:"start"`
	// Duration is zero for a span that did not end.
	Duration time.Duration `json:"duration"`
	// Error is the text of the error recorded on the span, if any.
	Error    string      `json:"error,omitempty"`
	Events   []SpanEvent `json:"events,omitempty"`
	Children Spans       `json:"children,omitempty"`
	// Parent is nil for a root span.
	Parent *Span `json:"-"`
}

// SpanEvent is an event recorded on a span, such as a retry.
type SpanEvent struct {
	Name       string         `json:"name"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Spans is a list of spans with their descendants.
type Spans []*Span

// All yields the spans and their descendants, depth first.
func (s Spans) All() iter.Seq[*Span] {
	return func(yield func(*Span) bool) {
		var walk func(Spans) bool
		walk = func(spans Spans) bool {
			for _, sp := range spans {
				if !yield(sp) || !walk(sp.Children) {
					return false
				}
			}
			return true
		}
		walk(s)
	}
}

// Find returns the spans, among s and their descendants, that match q.
func (s Spans) Find(q SpanQuery) []*Span {
	var out []*Span
	for sp := range s.All() {
		if q.Matches(sp) {
			out = append(out, sp)
		}
	}
	return out
}

// Any reports whether a span among s and their descendants matches q.
func (s Spans) Any(q SpanQuery) bool {
	for sp := range s.All() {
		if q.Matches(sp) {
			return true
		}
	}
	return false
}

// SpanQuery selects spans; every condition set must hold. The spans of
// middleware.Tracing are named after the OpenTelemetry GenAI conventions,
// such as "invoke_agent {agent}", "chat {model}" and
// "execute_tool {tool}", with attributes such as gen_ai.tool.name.
type SpanQuery struct {
	Name         string `json:"name,omitempty"`
	NameContains string `json:"name_contains,omitempty"`
	// NamePattern is a regular expression the name must match.
	NamePattern string `json:"name_pattern,omitempty"`
	// Attributes must all be present with equal values; numbers compare by
	// value.
	Attributes map[string]any `json:"attributes,omitempty"`
	// Error, if set, requires the span to have failed (true) or not.
	Error *bool `json:"error,omitempty"`
	// MinDuration and MaxDuration bound the span's duration when positive.
	MinDuration time.Duration `json:"min_duration,omitempty"`
	MaxDuration time.Duration `json:"max_duration,omitempty"`
	// Child and Descendant require a direct child or any descendant to
	// match the query.
	Child      *SpanQuery `json:"child,omitempty"`
	Descendant *SpanQuery `json:"descendant,omitempty"`
	// Ancestor requires any ancestor to match the query.
	Ancestor *SpanQuery `json:"ancestor,omitempty"`
	// Not requires the query not to match.
	Not *SpanQuery `json:"not,omitempty"`
}

// Matches reports whether sp matches q.
func (q *SpanQuery) Matches(sp *Span) bool {
	switch {
	case q.Name != "" && sp.Name != q.Name,
		q.NameContains != "" && !strings.Contains(sp.Name, q.NameContains),
		q.Error != nil && *q.Error != (sp.Error != ""),
		q.MinDuration > 0 && sp.Duration < q.MinDuration,
		q.MaxDuration > 0 && sp.Duration > q.MaxDuration,
		q.Not != nil && q.Not.Matches(sp):
		return false
	}
	if q.NamePattern != "" {
		re, err := regexp.Compile(q.NamePattern)
		if err != nil || !re.MatchString(sp.Name) {
			return false
		}
	}
	for k, want := range q.Attributes {
		got, ok := sp.Attributes[k]
		if !ok || !sameValue(got, want) {
			return false
		}
	}
	if q.Child != nil && !sp.Children.anyDirect(*q.Child) {
		return false
	}
	if q.Descendant != nil && !sp.Children.Any(*q.Descendant) {
		return false
	}
	if q.Ancestor != nil {
		found := false
		for p := sp.Parent; p != nil && !found; p = p.Parent {
			found = q.Ancestor.Matches(p)
		}
		if !found {
			return false
		}
	}
	return true
}

func (s Spans) anyDirect(q SpanQuery) bool {
	return slices.ContainsFunc(s, q.Matches)
}

// sameValue compares attribute values, numbers by value and other values
// deeply or by their formatting, so a query written in JSON matches the
// attributes the tracing middleware records.
func sameValue(a, b any) bool {
	if x, ok := toFloat(a); ok {
		y, ok := toFloat(b)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b) || fmt.Sprint(a) == fmt.Sprint(b)
}

func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

// HasSpan returns an evaluator with an assertion, named "HasSpan", that
// passes when a span recorded during the task matches q. Record spans by
// tracing the agent with Tracer.
func HasSpan(q SpanQuery) Evaluator {
	return Check("HasSpan", func(ec *EvaluatorContext) (bool, string) {
		if n := len(ec.Spans.Find(q)); n > 0 {
			return true, fmt.Sprintf("%d matching spans", n)
		}
		return false, "no matching span among " + spanNames(ec.Spans)
	})
}

// NoSpan returns an evaluator with an assertion, named "NoSpan", that
// passes when no span recorded during the task matches q, for example to
// check that a dangerous tool was not called.
func NoSpan(q SpanQuery) Evaluator {
	return Check("NoSpan", func(ec *EvaluatorContext) (bool, string) {
		found := ec.Spans.Find(q)
		if len(found) == 0 {
			return true, ""
		}
		return false, fmt.Sprintf("%d matching spans, the first %q", len(found), found[0].Name)
	})
}

func spanNames(s Spans) string {
	var names []string
	for sp := range s.All() {
		names = append(names, sp.Name)
	}
	if len(names) == 0 {
		return "no spans (trace the agent with eval.Tracer)"
	}
	return strings.Join(names, ", ")
}

// Tracer returns a tracer that records the spans of each case run, for
// span-based evaluation. Trace the agent under test with it, for example
// with agent.WithMiddleware(middleware.Tracing(eval.Tracer())), and the
// spans of each run appear in EvaluatorContext.Spans. Outside an
// evaluation, it discards spans.
func Tracer() telemetry.Tracer { return spanTracer{} }

type spanTracer struct{}

type (
	collectorKey struct{}
	spanKey      struct{}
)

// collector gathers the spans of one case run.
type collector struct {
	mu    sync.Mutex
	spans Spans
}

func withCollector(ctx context.Context, c *collector) context.Context {
	return context.WithValue(ctx, collectorKey{}, c)
}

func (c *collector) roots() Spans {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneSpans(c.spans, nil)
}

// cloneSpans copies spans, so evaluators see a snapshot even if an
// abandoned task goes on recording.
func cloneSpans(s Spans, parent *Span) Spans {
	if s == nil {
		return nil
	}
	out := make(Spans, len(s))
	for i, sp := range s {
		c := *sp
		c.Parent = parent
		c.Attributes = maps.Clone(sp.Attributes)
		c.Events = append([]SpanEvent(nil), sp.Events...)
		c.Children = cloneSpans(sp.Children, &c)
		out[i] = &c
	}
	return out
}

func (spanTracer) Start(ctx context.Context, name string, kind telemetry.SpanKind, attrs ...telemetry.Attr) (context.Context, telemetry.Span) {
	col, _ := ctx.Value(collectorKey{}).(*collector)
	if col == nil {
		return telemetry.NoopTracer{}.Start(ctx, name, kind, attrs...)
	}
	sp := &Span{Name: name, Kind: kind, Start: time.Now(), Attributes: map[string]any{}}
	rec := &recordingSpan{col: col, span: sp}
	col.mu.Lock()
	setAttrs(sp, attrs)
	if parent, ok := ctx.Value(spanKey{}).(*recordingSpan); ok && parent.col == col {
		sp.Parent = parent.span
		parent.span.Children = append(parent.span.Children, sp)
	} else {
		col.spans = append(col.spans, sp)
	}
	col.mu.Unlock()
	return context.WithValue(ctx, spanKey{}, rec), rec
}

func setAttrs(sp *Span, attrs []telemetry.Attr) {
	for _, a := range attrs {
		sp.Attributes[a.Key] = a.Value
	}
}

// recordingSpan implements telemetry.EventSpan.
type recordingSpan struct {
	col  *collector
	span *Span
}

func (r *recordingSpan) SetAttributes(attrs ...telemetry.Attr) {
	r.col.mu.Lock()
	defer r.col.mu.Unlock()
	setAttrs(r.span, attrs)
}

func (r *recordingSpan) RecordError(err error) {
	if err == nil {
		return
	}
	r.col.mu.Lock()
	defer r.col.mu.Unlock()
	r.span.Error = err.Error()
}

func (r *recordingSpan) AddEvent(name string, attrs ...telemetry.Attr) {
	ev := SpanEvent{Name: name}
	if len(attrs) > 0 {
		ev.Attributes = make(map[string]any, len(attrs))
		for _, a := range attrs {
			ev.Attributes[a.Key] = a.Value
		}
	}
	r.col.mu.Lock()
	defer r.col.mu.Unlock()
	r.span.Events = append(r.span.Events, ev)
}

func (r *recordingSpan) End() {
	r.col.mu.Lock()
	defer r.col.mu.Unlock()
	if r.span.Duration == 0 {
		r.span.Duration = max(time.Since(r.span.Start), time.Nanosecond)
	}
}

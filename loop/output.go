package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// DefaultOutputRetries is how often an answer that does not match
// Config.Output is sent back to the model when Output.Retries is zero.
const DefaultOutputRetries = 2

// Output asks the model for a final answer in JSON matching Schema and
// validates it.
//
// Unless Config.ResponseFormat is set, the runtime requests Schema as the
// response format in strict mode; see tool.Schema.StrictJSON. When the
// model answers without tool calls, the answer is checked against Schema
// before BeforeFinish middleware, such as Stop hooks, runs. An answer in a
// single Markdown code block, such as one fenced with ```json, is checked
// without the fence. An answer that does not match is sent back to the
// model with the problems found, and the run continues; each attempt takes
// a turn. Once the retries are used up, the run fails with an *OutputError;
// resuming it gives the model the same number of retries again. Answers cut
// off by the token limit, withheld by a content filter or refused are not
// checked: they end the run as they would without Output. Each answer sent
// back is reported with an EventOutputRetry.
type Output struct {
	// Name names the schema for providers. It defaults to "output" and may
	// hold letters, digits, underscores and dashes, up to 64.
	Name string
	// Schema describes the answer. It must be an object schema with
	// properties, such as tool.SchemaOf of a struct type.
	Schema *tool.Schema
	// Retries bounds how often an answer that does not match Schema is sent
	// back to the model. Zero means DefaultOutputRetries; a negative value
	// means none.
	Retries int
}

func (o *Output) retries() int {
	switch {
	case o.Retries == 0:
		return DefaultOutputRetries
	case o.Retries < 0:
		return 0
	}
	return o.Retries
}

// OutputError reports that the final answer of a run did not match
// Config.Output after all retries. It is the error of the failed run.
type OutputError struct {
	// Output is the text of the last answer. It is empty when an
	// AnswerHolder holds answers: it was rejected before answer-holder checks.
	Output string
	// Problems lists what does not match the schema. When an AnswerHolder
	// holds answers, it reports only a generic mismatch, since detailed
	// diagnostics may quote text that has not passed answer-holder checks.
	Problems []string
	// Attempts is the number of answers checked.
	Attempts int
}

func (e *OutputError) Error() string {
	return fmt.Sprintf("loop: the answer does not match the output schema after %d attempts: %s",
		e.Attempts, strings.Join(e.Problems, "; "))
}

// OutputRetry describes an answer that did not match Config.Output and was
// sent back to the model, for EventOutputRetry.
type OutputRetry struct {
	// Output is the text of the answer. It is empty when an AnswerHolder
	// holds answers: it was rejected before answer-holder checks.
	Output string `json:"output"`
	// Problems lists what does not match the schema. When an AnswerHolder
	// holds answers, it reports only a generic mismatch; see OutputError.
	Problems []string `json:"problems,omitempty"`
	// Attempt numbers the answer among those the run checked, from 1. The
	// count restarts when a run that failed with an *OutputError is
	// resumed.
	Attempt int `json:"attempt"`
	// Retries is the number of retries allowed; the run fails with an
	// *OutputError if answer Retries+1 does not match either.
	Retries int `json:"retries"`
}

var outputNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// configureOutput checks Config.Output and derives the response format
// from it.
func configureOutput(cfg *Config) error {
	if cfg.Output == nil {
		return nil
	}
	o := *cfg.Output
	if o.Schema == nil || o.Schema.Type != "object" || len(o.Schema.Properties) == 0 {
		return errors.New("loop: output schema must be an object with properties, such as the schema of a struct type")
	}
	if o.Name == "" {
		o.Name = "output"
	} else if !outputNamePattern.MatchString(o.Name) {
		return fmt.Errorf("loop: invalid output name %q", o.Name)
	}
	cfg.Output = &o
	if cfg.ResponseFormat == nil {
		cfg.ResponseFormat = &model.ResponseFormat{Name: o.Name, Schema: o.Schema.StrictJSON(), Strict: true}
	}
	return nil
}

// checkOutput checks a final answer against Config.Output. It reports
// whether the run may finish with the answer; otherwise the answer was sent
// back to the model, and the run continues, or the run failed.
func (l *execution) checkOutput(ctx context.Context, resp *model.Response) bool {
	o := l.r.cfg.Output
	if o == nil {
		return true
	}
	switch resp.FinishReason {
	case model.FinishLength, model.FinishContentFilter, model.FinishRefusal:
		return true
	}
	text := resp.Message.Text()
	problems := outputProblems(o.Schema, text)
	if len(problems) == 0 {
		return true
	}
	// Validation diagnostics can quote model-controlled text, such as an
	// unexpected property name. Keep those details in the reminder for the
	// model, but do not report them before the answer holders checked it.
	output, reportedProblems := text, problems
	if l.r.stages.holdAnswer {
		l.drop()
		output = ""
		reportedProblems = []string{"the answer does not match the output schema"}
	}
	st := l.st
	if st.OutputRetries < o.retries() {
		st.OutputRetries++
		st.Messages = append(st.Messages, model.UserText(model.WrapReminder(outputReminder(o.Schema, problems))))
		l.r.checkpoint(ctx, st)
		retry := &OutputRetry{Output: output, Problems: reportedProblems, Attempt: st.OutputRetries, Retries: o.retries()}
		l.emit(&Event{Type: EventOutputRetry, OutputRetry: retry})
		return false
	}
	attempts := st.OutputRetries + 1
	// A resumed run gets its retries again.
	st.OutputRetries = 0
	l.fail(ctx, &OutputError{Output: output, Problems: reportedProblems, Attempts: attempts})
	return false
}

// outputProblems returns what is wrong with an answer, or nil if it matches
// schema.
func outputProblems(schema *tool.Schema, text string) []string {
	raw := json.RawMessage(outputJSON(text))
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []string{"the answer is not a single JSON value: " + err.Error()}
	}
	if err := schema.Validate(raw); err != nil {
		if ve, ok := errors.AsType[*tool.ValidationError](err); ok {
			return ve.Problems
		}
		return []string{err.Error()}
	}
	return nil
}

// outputReminder asks the model to answer again.
func outputReminder(schema *tool.Schema, problems []string) string {
	var b strings.Builder
	b.WriteString("Your answer is not valid output. Answer again with only a JSON value that matches the output schema, without any other text.\n\nProblems:\n")
	for _, p := range problems {
		b.WriteString("- " + p + "\n")
	}
	b.WriteString("\nOutput schema:\n")
	b.Write(schema.JSON())
	return b.String()
}

// outputJSON returns the JSON of an answer: its text without surrounding
// whitespace or, if the text is a single Markdown code block, such as
// ```json ... ```, the content of the block.
func outputJSON(text string) string {
	s := strings.TrimSpace(text)
	if len(s) < 6 || !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") {
		return s
	}
	info, body, ok := strings.Cut(s[3:len(s)-3], "\n")
	if !ok || strings.Contains(info, "`") {
		return s
	}
	return strings.TrimSpace(body)
}

// Decode unmarshals the output of a completed run into v with
// encoding/json. Output in a single Markdown code block is decoded from
// inside the block, as Config.Output checks it. It returns an error if the
// run did not complete or the model refused to answer.
func (r *Result) Decode(v any) error {
	switch {
	case r == nil:
		return errors.New("loop: no result to decode")
	case r.Err != nil:
		return r.Err
	case r.Status != StatusCompleted:
		return fmt.Errorf("loop: run %s ended with status %s: %s", r.RunID, r.Status, r.Reason)
	case r.FinishReason == model.FinishRefusal:
		return fmt.Errorf("loop: run %s: the model refused to answer: %s", r.RunID, r.Output)
	}
	if err := json.Unmarshal([]byte(outputJSON(r.Output)), v); err != nil {
		return fmt.Errorf("loop: decode the output of run %s: %w", r.RunID, err)
	}
	return nil
}

// DecodeAs decodes the output of a completed run as a T, as Decode does.
// Agents configured with agent.WithOutput[T] answer with JSON of a T.
func (r *Result) DecodeAs[T any]() (T, error) {
	var v T
	err := r.Decode(&v)
	return v, err
}

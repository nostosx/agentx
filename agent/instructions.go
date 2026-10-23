package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// InstructionFunc returns instructions for a run, for example from the
// run's dependencies (loop.Deps) or session state (SessionState). An
// error fails the run.
type InstructionFunc func(ctx context.Context, run *loop.Run) (string, error)

// Instruction is a function that extends an agent's instructions; see
// WithInstructionsFunc.
type Instruction struct {
	Func InstructionFunc
	// EveryTurn evaluates Func before each model call instead of once per
	// run.
	EveryTurn bool
}

// InstructionOption configures an instruction function.
type InstructionOption func(*Instruction)

// EveryTurn evaluates an instruction function before each model call, for
// instructions that change during a run, such as ones that depend on
// session state that tools write. Its text is added to each request
// without being saved in the run's system prompt. Instructions that
// change between calls defeat the providers' prompt caching of what
// follows them, so prefer the default of once per run.
func EveryTurn() InstructionOption { return func(i *Instruction) { i.EveryTurn = true } }

// WithInstructionsFunc adds instructions that f returns, in the style of
// Pydantic AI's @agent.instructions and the OpenAI Agents SDK's dynamic
// instructions. By default f runs once when a run starts, after the hooks
// accepted its input, and its text follows the static instructions of
// WithInstructions, and the text of earlier functions, separated by a
// blank line; the system prompt then stays the same for the whole run,
// which keeps it cacheable, and a resumed run reuses it. Empty text adds
// nothing. EveryTurn evaluates f before each model call instead. The text
// is used as it is; see RenderTemplate to fill in session state.
//
// When a run is handed off, the instructions of the agent it is handed
// off to apply.
func WithInstructionsFunc(f InstructionFunc, opts ...InstructionOption) Option {
	return func(c *Config) {
		in := Instruction{Func: f}
		for _, opt := range opts {
			opt(&in)
		}
		c.InstructionFuncs = append(c.InstructionFuncs, in)
	}
}

// WithInstructionTemplate fills placeholders in the static instructions
// of WithInstructions from the session state when a run starts, like
// Google ADK's {key} templating; see RenderTemplate. A required key that
// the state lacks fails the run. Text of WithInstructionsFunc is not
// templated, as ADK's instruction providers are not.
func WithInstructionTemplate() Option { return func(c *Config) { c.InstructionTemplate = true } }

// RenderTemplate fills the placeholders of s from the session state of
// ctx; see SessionState and ContextWithSessionState. {key} is replaced
// with the value of key: a string as it is, any other value as JSON. It
// is an error if the state, or a state at all, is missing for {key}; {key?}
// is replaced with nothing then. Keys start with a letter or underscore,
// followed by letters, digits and _ . : -, so braces around anything else,
// such as JSON, stay as they are. {{ and }} stand for literal { and }, for
// example {{key}} for the text {key}.
func RenderTemplate(ctx context.Context, s string) (string, error) {
	if !strings.ContainsAny(s, "{}") {
		return s, nil
	}
	st, _ := SessionState(ctx)
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '{' && strings.HasPrefix(s[i:], "{{"), c == '}' && strings.HasPrefix(s[i:], "}}"):
			b.WriteByte(c)
			i += 2
			continue
		case c != '{':
			b.WriteByte(c)
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		key, optional := "", false
		if end > 0 {
			key = s[i+1 : i+end]
			key, optional = strings.CutSuffix(key, "?")
		}
		if !validTemplateKey(key) {
			b.WriteByte(c)
			i++
			continue
		}
		i += end + 1
		var v any
		ok := false
		if st != nil {
			v, ok = st.Get(key)
		}
		switch {
		case !ok && optional:
		case !ok:
			return "", fmt.Errorf("instruction template: the session state has no value for {%s}; use {%s?} if it is optional", key, key)
		default:
			if text, isText := v.(string); isText {
				b.WriteString(text)
				continue
			}
			j, err := json.Marshal(v)
			if err != nil {
				return "", fmt.Errorf("instruction template: encode {%s}: %w", key, err)
			}
			b.Write(j)
		}
	}
	return b.String(), nil
}

// validTemplateKey reports whether key may name a template placeholder.
func validTemplateKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		other := '0' <= r && r <= '9' || r == '.' || r == ':' || r == '-'
		if !letter && (i == 0 || !other) {
			return false
		}
	}
	return true
}

// agentMiddleware implements the agent's own runtime features: dynamic
// instructions, instruction templates and the handoff input filter.
type agentMiddleware struct {
	cfg *Config
}

func (m *agentMiddleware) Name() string { return "agentx.agent" }

// BeforeRun implements loop.BeforeRunner. It renders the template of
// the static instructions and adds the text of the per-run functions.
func (m *agentMiddleware) BeforeRun(ctx context.Context, run *loop.Run, in *loop.RunInput) error {
	if base := strings.TrimSpace(m.cfg.Instructions); m.cfg.InstructionTemplate && base != "" && strings.HasPrefix(in.System, base) {
		rendered, err := RenderTemplate(ctx, base)
		if err != nil {
			return err
		}
		in.System = rendered + in.System[len(base):]
	}
	extra, err := m.instructions(ctx, run, false)
	if err != nil {
		return err
	}
	in.System = joinInstructions(in.System, extra)
	return nil
}

// BeforeModel implements loop.BeforeModeler. It adds the text of the
// per-turn functions and applies the input filter of a handoff.
func (m *agentMiddleware) BeforeModel(ctx context.Context, run *loop.Run, req *model.Request) error {
	extra, err := m.instructions(ctx, run, true)
	if err != nil {
		return err
	}
	req.System = joinInstructions(req.System, extra)
	filterHandoff(ctx, run, req)
	return nil
}

// instructions returns the text of the per-run or per-turn functions.
func (m *agentMiddleware) instructions(ctx context.Context, run *loop.Run, everyTurn bool) (string, error) {
	var parts []string
	for _, in := range m.cfg.InstructionFuncs {
		if in.Func == nil || in.EveryTurn != everyTurn {
			continue
		}
		text, err := in.Func(ctx, run)
		if err != nil {
			return "", fmt.Errorf("agent %s: instructions: %w", m.cfg.Name, err)
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// joinInstructions appends extra to system, separated by a blank line.
func joinInstructions(system, extra string) string {
	switch {
	case extra == "":
		return system
	case system == "":
		return extra
	}
	return system + "\n\n" + extra
}

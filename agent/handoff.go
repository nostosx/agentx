package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// DefaultMaxHandoffs bounds the handoffs of a run; see WithMaxHandoffs.
const DefaultMaxHandoffs = 10

// HandoffToolPrefix starts the names of handoff tools.
const HandoffToolPrefix = "transfer_to_"

// Run values of a run handed off to an agent. Result.Values omits them.
const (
	// handoffPrefix starts the keys.
	handoffPrefix = "agentx.handoff."
	// handoffFromKey and handoffToolKey name the agent that handed the run
	// off and the tool it used, so the input filter applies after a
	// resume.
	handoffFromKey = loop.HandoffFromKey
	handoffToolKey = handoffPrefix + "tool"
	// handoffCountKey counts the run's handoffs.
	handoffCountKey = handoffPrefix + "count"
	// handoffPriorKey counts the messages the run added before the agent
	// took over, for Result.NewMessages.
	handoffPriorKey = handoffPrefix + "prior"
	// handoffTurnKey is the turn whose handoff call was planned, so the
	// model hands off once per turn.
	handoffTurnKey = handoffPrefix + "turn"
	// skipInputKey is guard.SkipInputKey.
	skipInputKey = "guard.skip_input"
)

// handoffNote is the input of the agent a run is handed off to.
const handoffNote = "The %s agent handed this conversation off to you, the %s agent, with %s. Continue it from here."

// Handoff is an agent the model can hand a run off to; see WithHandoffs.
type Handoff struct {
	// Agent receives the run.
	Agent Agent
	// ToolName is the name of the tool that hands off; it defaults to
	// "transfer_to_" and the agent's name.
	ToolName string
	// Description describes the tool; it defaults to one built from the
	// agent's description.
	Description string
	// InputFilter, if set, rewrites the conversation the agent sees, up
	// to and including the handoff call's result, for example with
	// RemoveToolCalls or NestHandoffHistory. It gets a copy of the
	// messages and must not modify them. It applies to the model requests
	// of the run, not to the conversation saved, so later runs of a
	// session see all of it.
	InputFilter func([]*model.Message) []*model.Message

	params  *tool.Schema
	onInput func(ctx context.Context, args json.RawMessage) error
}

// HandoffOption configures a handoff.
type HandoffOption func(*Handoff)

// HandoffName names the handoff's tool.
func HandoffName(name string) HandoffOption { return func(h *Handoff) { h.ToolName = name } }

// HandoffDescription describes the handoff's tool to the model.
func HandoffDescription(d string) HandoffOption { return func(h *Handoff) { h.Description = d } }

// HandoffInputFilter sets the handoff's Handoff.InputFilter.
func HandoffInputFilter(f func([]*model.Message) []*model.Message) HandoffOption {
	return func(h *Handoff) { h.InputFilter = f }
}

// HandoffInput makes the model send arguments of type T, whose schema is
// derived with tool.SchemaOf, when it hands off, such as a reason or a
// priority, like the OpenAI Agents SDK's input_type. fn, if not nil, is
// called with them before the handoff; an error refuses the handoff and is
// reported to the model. The arguments are added to the input of the
// agent that takes over and reported in loop.Handoff.Input.
func HandoffInput[T any](fn func(ctx context.Context, in T) error) HandoffOption {
	return func(h *Handoff) {
		h.params = tool.SchemaOf[T]()
		h.onInput = func(ctx context.Context, args json.RawMessage) error {
			var in T
			if len(args) > 0 && string(args) != "null" {
				if err := json.Unmarshal(args, &in); err != nil {
					return fmt.Errorf("invalid arguments: %w", err)
				}
			}
			if fn == nil {
				return nil
			}
			return fn(ctx, in)
		}
	}
}

// WithHandoffs lets the model hand the run off to agents, in the style of
// the OpenAI Agents SDK's handoffs and Google ADK's agent transfer. Each
// agent appears as a tool named "transfer_to_" and its name. When the
// model calls one, the calls of its turn run, the agent's part of the run
// ends, and the run continues on the agent handed off to, with its own
// instructions, tools, middleware, guardrails and limits: it sees the
// conversation so far, and a note about the handoff as its input, and
// emits a loop.EventHandoff before its events. The run keeps its ID,
// usage and turn count, so limits apply to the whole run, and
// Result.Agent names the agent that ran last. Only the first handoff call
// of a turn hands off; the others are denied. A run is handed off at most
// WithMaxHandoffs times.
//
// Handoff tools are read-only (tool.Info.ReadOnly): a call only checks
// the handoff, which happens after the turn, so permission plan mode
// allows it.
//
// The agent handed off to does not run its input guardrails, and its
// UserPromptSubmit hooks see the note as the prompt. PreToolUse and
// PostToolUse hooks with a matcher such as "transfer_to_.*" observe and
// can deny handoffs.
//
// A Runner remembers the agent its session was handed off to (see
// ActiveAgentKey) and sends the session's next message to it. An
// interrupted run resumes on the agent that was running; agents that hand
// off to each other should share a checkpointer. To hand off to an agent
// that is built later, such as back to this one, use Ref.
func WithHandoffs(agents ...Agent) Option {
	return func(c *Config) {
		for _, a := range agents {
			c.Handoffs = append(c.Handoffs, Handoff{Agent: a})
		}
	}
}

// WithHandoff is like WithHandoffs for a single agent with options.
func WithHandoff(a Agent, opts ...HandoffOption) Option {
	return func(c *Config) {
		h := Handoff{Agent: a}
		for _, opt := range opts {
			opt(&h)
		}
		c.Handoffs = append(c.Handoffs, h)
	}
}

// WithMaxHandoffs bounds the handoffs of the runs started on the agent;
// the default is DefaultMaxHandoffs. A handoff beyond it is refused with
// an error result to the model.
func WithMaxHandoffs(n int) Option { return func(c *Config) { c.MaxHandoffs = n } }

// Ref returns a reference to the agent named name, for WithHandoffs, so
// agents can hand off to each other although each must be built before
// the agents that hand off to it. The reference resolves, when the model
// hands off, to the agent of that name among the agents the run's first
// agent hands off to, directly or through others, including that agent
// itself. description describes the agent to the model. A reference
// cannot run on its own.
func Ref(name, description string) Agent { return agentRef{name: name, desc: description} }

type agentRef struct{ name, desc string }

func (r agentRef) Name() string        { return r.name }
func (r agentRef) Description() string { return r.desc }

func (r agentRef) Run(context.Context, *Input) iter.Seq2[*loop.Event, error] {
	return func(yield func(*loop.Event, error) bool) {
		yield(nil, fmt.Errorf("agent %s is a reference for handoffs and cannot run", r.name))
	}
}

// chain holds the handoffs of a run: its first agent, which resolves
// references, and the number of handoffs so far.
type chain struct {
	root  *LLMAgent
	limit int
	count int
}

type chainKey struct{}

func chainOf(ctx context.Context) *chain {
	c, _ := ctx.Value(chainKey{}).(*chain)
	return c
}

// handoffTools completes the handoffs of cfg and returns their tools.
func handoffTools(cfg *Config) ([]tool.Tool, error) {
	var out []tool.Tool
	// A copy, as agents rebuilt from cfg share its slice.
	cfg.Handoffs = slices.Clone(cfg.Handoffs)
	for i := range cfg.Handoffs {
		h := &cfg.Handoffs[i]
		if h.Agent == nil {
			return nil, errors.New("handoff has no agent")
		}
		name := h.Agent.Name()
		h.ToolName = cmp.Or(h.ToolName, HandoffToolPrefix+name)
		desc := h.Description
		if desc == "" {
			desc = "Hand the conversation off to the " + name + " agent, which takes over and continues it with the user."
			if d := h.Agent.Description(); d != "" {
				desc += " " + d
			}
		}
		params := h.params
		if params == nil {
			params = tool.SchemaOf[struct{}]()
		}
		// The call only checks the handoff, which happens after the turn,
		// so it is read-only: permission plan mode allows it.
		out = append(out, tool.New(h.ToolName, desc, params, handoffHandler(*h), tool.ReadOnly()))
	}
	return out, nil
}

// handoffHandler returns the handler of a handoff's tool. It checks that
// the handoff can happen; the run hands off once the call succeeded.
func handoffHandler(h Handoff) tool.Handler {
	return func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
		name := h.Agent.Name()
		ch := chainOf(ctx)
		limit, count := DefaultMaxHandoffs, 0
		if ch != nil {
			limit, count = ch.limit, ch.count
		}
		if count >= limit {
			return tool.Errorf("The conversation was not handed off to %s: the run reached its limit of %d handoffs.", name, limit), nil
		}
		if _, ok := h.Agent.(agentRef); ok && (ch == nil || ch.root.lookup(name) == nil) {
			return tool.Errorf("The conversation was not handed off: agent %s is not available.", name), nil
		}
		if h.onInput != nil {
			if err := h.onInput(ctx, args); err != nil {
				return tool.Errorf("The conversation was not handed off to %s: %v", name, err), nil
			}
		}
		return tool.Text("Transferred to " + name + "."), nil
	}
}

// lookup returns the agent named name among a and the agents it hands off
// to, directly or through others, or nil. References are not returned.
func (a *LLMAgent) lookup(name string) Agent {
	var found Agent
	a.walk(func(x Agent) bool {
		if x.Name() == name {
			found = x
			return false
		}
		return true
	})
	return found
}

// walk calls f for a and the agents it hands off to, breadth first, until
// f returns false. References are skipped.
func (a *LLMAgent) walk(f func(Agent) bool) {
	seen := map[*LLMAgent]bool{a: true}
	queue := []*LLMAgent{a}
	if !f(a) {
		return
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, h := range cur.cfg.Handoffs {
			switch t := h.Agent.(type) {
			case agentRef:
				continue
			case *LLMAgent:
				if seen[t] {
					continue
				}
				seen[t] = true
				queue = append(queue, t)
			}
			if !f(h.Agent) {
				return
			}
		}
	}
}

// handoffByTool returns the handoff whose tool is name, or nil.
func (a *LLMAgent) handoffByTool(name string) *Handoff {
	for i := range a.cfg.Handoffs {
		if a.cfg.Handoffs[i].ToolName == name {
			return &a.cfg.Handoffs[i]
		}
	}
	return nil
}

// target returns the agent a handoff transfers to.
func (ch *chain) target(h *Handoff) Agent {
	if ref, ok := h.Agent.(agentRef); ok {
		return ch.root.lookup(ref.name)
	}
	return h.Agent
}

// handoffCall is a handoff a finished part of a run asked for.
type handoffCall struct {
	h    *Handoff
	call *model.ToolCall
}

// requestedHandoff returns the handoff the run of res asked for: the
// first successful handoff call of its last turn, if the run completed
// with it.
func (a *LLMAgent) requestedHandoff(res *loop.Result) *handoffCall {
	if a == nil || res == nil || res.Status != loop.StatusCompleted || len(a.cfg.Handoffs) == 0 || len(res.Messages) == 0 {
		return nil
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != model.RoleTool {
		return nil
	}
	for _, r := range last.ToolResults() {
		h := a.handoffByTool(r.Name)
		if r.IsError || h == nil {
			continue
		}
		hc := &handoffCall{h: h, call: &model.ToolCall{ID: r.CallID, Name: r.Name}}
		for _, msg := range slices.Backward(res.Messages) {
			if msg.Role != model.RoleAssistant {
				continue
			}
			for _, c := range msg.ToolCalls() {
				if c.ID == r.CallID {
					hc.call.Arguments = c.Arguments
				}
			}
			break
		}
		return hc
	}
	return nil
}

// filterHandoff applies the input filter of the handoff that started the
// run, if any, to the conversation before the run's input.
func filterHandoff(ctx context.Context, run *loop.Run, req *model.Request) {
	from, _ := run.Value(handoffFromKey)
	toolName, _ := run.Value(handoffToolKey)
	ch := chainOf(ctx)
	if from == "" || ch == nil {
		return
	}
	src, _ := ch.root.lookup(from).(*LLMAgent)
	if src == nil {
		return
	}
	h := src.handoffByTool(toolName)
	k := len(run.Messages()) - len(run.NewMessages())
	if h == nil || h.InputFilter == nil || k <= 0 || k > len(req.Messages) {
		return
	}
	filtered := h.InputFilter(slices.Clone(req.Messages[:k]))
	req.Messages = slices.Concat(filtered, req.Messages[k:])
}

// BeforeTool denies the handoff calls of a turn after the first.
func (m *agentMiddleware) BeforeTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	if !slices.ContainsFunc(m.cfg.Handoffs, func(h Handoff) bool { return h.ToolName == plan.Call.Name }) {
		return nil
	}
	if d, _ := plan.Decision(); d == loop.Denied {
		return nil
	}
	turn := strconv.Itoa(run.Turn())
	if v, _ := run.Value(handoffTurnKey); v == turn {
		plan.Deny("Only one handoff can be made at a time; this call was ignored.")
		return nil
	}
	run.SetValue(handoffTurnKey, turn)
	return nil
}

// atoi parses a run value, or returns zero.
func atoi(values map[string]string, key string) int {
	n, _ := strconv.Atoi(values[key])
	return n
}

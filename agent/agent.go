// Package agent provides LLM agents built on the runtime loop, composable
// as sub-agents, and a Runner that persists conversations in sessions.
//
// # Tool call pipeline
//
// New installs its loop middleware in this order: input guardrails, hooks
// (WithHooks), skills (WithSkills), the middleware of WithMiddleware and
// WithPermissions in the order those options are given, handoffs, context
// engineering, and finally tool and output guardrails. Each tool call the
// model requests then passes through these steps:
//
//  1. BeforeTool, in that order: PreToolUse hooks, the allowed and
//     disallowed tools of an active skill, the WithMiddleware and
//     WithPermissions middleware, the handoff check, and tool input
//     guardrails. Each stage sees the arguments as earlier stages rewrote
//     them and may allow the call, ask for approval or deny it. The most
//     restrictive decision wins whatever the order, so a hook that allows a
//     call does not lift a permission rule's ask or deny, and the first
//     denial skips the remaining stages. A permission.Policy also receives
//     the decision so far, which changes its result in dontAsk mode;
//     register middleware that decides calls before WithPermissions. A tool
//     input guardrail that trips ends the run instead of denying the call.
//  2. Approval, when a stage asked, or when no stage decided and the tool
//     requires approval (tool.Info.RequiresApproval, per call for a
//     tool.CallApprover). The Approver of WithApprover decides; without one
//     the run is interrupted and resumed with the approvals. An allowed
//     call skips approval.
//  3. RevalidateTool, when an approval changed the arguments: the
//     permission policy, tool input guardrails and any other
//     loop.ToolRevalidator check them again. BeforeTool, including hooks,
//     does not run again.
//  4. Execution. The arguments are validated against the tool's schema,
//     then the call runs inside the WrapTool middleware, the first in the
//     order above outermost, so tool output guardrails wrap the call most
//     closely. Inside those wrappers run the tool middleware of
//     WithToolMiddleware, the first outermost, and then the tool.
//  5. AfterTool, in reverse order: the guardrails first, which end the run
//     if a tool output guardrail tripped during execution, then the
//     WithMiddleware and WithPermissions middleware, skills, and PostToolUse
//     or PostToolUseFailure hooks last. Denied calls pass AfterTool too, but
//     fire no hooks.
//
// Choose the mechanism by purpose:
//
//   - permission.Policy (WithPermissions): rules and modes for tools, paths
//     and commands, with remembered approvals.
//   - hook.Manager (WithHooks): Claude Code compatible command and HTTP
//     hooks, for policies kept outside the program.
//   - Tool guardrails (WithToolGuardrail): checks whose failure must stop
//     the run, on the arguments that will run and the result the tool
//     returned.
//   - A loop.Middleware with BeforeTool (WithMiddleware): policies written
//     in Go. Middleware that enforces an argument policy must also
//     implement loop.ToolRevalidator.
//   - tool.Middleware (WithToolMiddleware): execution concerns such as
//     timeouts and output truncation that need no access to the run; use a
//     loop.ToolWrapper when the wrapper needs it.
//   - WithApprover, tool.CallApprover or tool.Info.RequiresApproval: which
//     calls need approval and who grants it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

// Input is the input of an agent run.
type Input struct {
	// Message is the new user message.
	Message *model.Message
	// History is the conversation before Message.
	History    []*model.Message
	SessionID  string
	RunID      string
	NewSession bool
	// MaxTurns, if positive, and Limits, if non-nil, override the agent's
	// turn limit and usage limits for this run.
	MaxTurns int
	Limits   *loop.Limits
	// Values are the initial run values; see loop.Request.Values.
	Values map[string]string
	// Skill, if set, is a skill the user invokes at the start of the run,
	// as with /name arguments in Claude Code; see RunSkill and
	// skill.WithCall.
	Skill *skill.Call
	// Agent, if set, names the agent of the handoff graph that handles the
	// input instead of this one, as a Runner sets it to the session's
	// active agent; see WithHandoffs and Result.Agent.
	Agent string
	// Deps add dependencies to the context of the run; see RunDeps.
	Deps []func(context.Context) context.Context
}

// Text returns an input holding a single user text message.
func Text(s string) *Input { return &Input{Message: model.UserText(s)} }

// Agent runs tasks and reports progress as runtime events.
type Agent interface {
	Name() string
	Description() string
	Run(ctx context.Context, in *Input) iter.Seq2[*loop.Event, error]
}

// Resumer is implemented by agents that can resume interrupted runs.
type Resumer interface {
	Resume(ctx context.Context, runID string, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error]
}

// StateResumer is implemented by agents that can resume a run from its
// state, without a checkpoint. Sub-agents are resumed this way.
type StateResumer interface {
	ResumeState(ctx context.Context, st *loop.State, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error]
}

// Discarder is implemented by agents that can delete the saved state of a
// run that will not be resumed.
type Discarder interface {
	Discard(ctx context.Context, runID string) error
}

// LLMAgent is an agent driven by a language model.
type LLMAgent struct {
	cfg Config
	rt  *loop.Runtime
	// hooks is cfg.Hooks, or a copy of it with the dispatchers of skill
	// hooks.
	hooks *hook.Manager
}

// New returns an LLM agent.
func New(name string, opts ...Option) (*LLMAgent, error) {
	cfg := Config{Name: name}
	for _, opt := range opts {
		opt(&cfg)
	}
	return build(cfg)
}

// build returns the agent cfg configures.
func build(cfg Config) (*LLMAgent, error) {
	name := cfg.Name
	if err := tool.ValidateName(name); err != nil {
		return nil, fmt.Errorf("agent name: %w", err)
	}
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent %s: model is required", name)
	}

	reg := &tool.Registry{}
	add := func(ts ...tool.Tool) error {
		if err := reg.Register(ts...); err != nil {
			return fmt.Errorf("agent %s: %w", name, err)
		}
		return nil
	}
	if err := add(cfg.Tools...); err != nil {
		return nil, err
	}

	ccfg := cfg.Context
	ccfg.Providers = slices.Concat(ccfg.Providers, cfg.ContextProviders)
	hooks := cfg.Hooks
	var skills *skill.Manager
	if cfg.Skills.Len() > 0 || cfg.forkedSkill {
		var err error
		if skills, hooks, err = newSkillManager(&cfg); err != nil {
			return nil, fmt.Errorf("agent %s: %w", name, err)
		}
	}
	if cfg.Skills.Len() > 0 {
		ccfg.Providers = append(ccfg.Providers, actx.ProviderFunc(func(_ context.Context, in *actx.Input) ([]actx.Section, error) {
			return []actx.Section{{Name: "skills", Content: skills.Prompt(in.Messages), Placement: actx.PlaceSystem, Priority: 10}}, nil
		}))
		if err := add(skills.Tool()); err != nil {
			return nil, err
		}
	}
	if cfg.Memory != nil {
		scope := cfg.MemoryScopeFunc
		if scope == nil {
			scope = memory.FixedScope(cfg.MemoryScope)
		}
		if !cfg.DisableMemoryTools {
			if err := add(memory.ScopedTools(cfg.Memory, scope)...); err != nil {
				return nil, err
			}
		}
		if cfg.MemoryRecall > 0 {
			ccfg.Providers = append(ccfg.Providers, scopedMemoryProvider(cfg.Memory, scope, cfg.MemoryRecall))
		}
	}
	handoffs, err := handoffTools(&cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	if err := add(handoffs...); err != nil {
		return nil, err
	}
	stopAt := slices.Clone(cfg.StopAtTools)
	for _, h := range cfg.Handoffs {
		stopAt = append(stopAt, h.ToolName)
	}
	for _, sub := range cfg.SubAgents {
		opts := ToolOptions{Hooks: hooks, Events: cfg.SubAgentEvents, Concurrent: cfg.ConcurrentSubAgents, MaxDepth: cfg.MaxDepth}
		if err := add(AsToolWith(sub, opts)); err != nil {
			return nil, err
		}
	}

	// Hooks run first so a rejected input skips context providers such as
	// memory recall; context runs last so its reminder leads the input.
	// Skills come next, so hooks can reject an input before a skill the
	// user invoked with it is rendered.
	// Guardrails run first, on the input as the user sent it, and last,
	// on the answer the run finishes with; see guard.Middleware.
	var mws []loop.Middleware
	var guardLast loop.Middleware
	if !cfg.Guardrails.Empty() {
		var first loop.Middleware
		first, guardLast = guard.Middleware(cfg.Guardrails)
		mws = append(mws, first)
	}
	if hooks != nil {
		// Hooks receive the permission mode of a policy such as
		// permission.Policy, as Claude Code's permission_mode.
		for _, mw := range cfg.Middlewares {
			if pm, ok := mw.(interface{ PermissionMode(*loop.Run) string }); ok {
				hooks.SetPermissionMode(pm.PermissionMode)
				break
			}
		}
		mws = append(mws, hook.Middleware(hooks))
	}
	if skills != nil {
		mws = append(mws, skills.Middleware())
	}
	mws = append(mws, cfg.Middlewares...)
	// Any agent may be handed off to, so it applies handoff input filters.
	mws = append(mws, &agentMiddleware{cfg: &cfg})
	mws = append(mws, actx.Middleware(actx.NewManager(ccfg),
		actx.WithCompactGuard(hook.PreCompactGuard(hooks)), actx.WithCompactObserver(hook.PostCompactObserver(hooks))))
	if guardLast != nil {
		mws = append(mws, guardLast)
	}

	rt, err := loop.New(loop.Config{
		Name:            name,
		Model:           cfg.Model,
		Instructions:    cfg.Instructions,
		Tools:           reg,
		Middlewares:     mws,
		ToolMiddlewares: cfg.ToolMiddlewares,
		Checkpointer:    cfg.Checkpointer,
		Approver:        cfg.Approver,
		MaxTurns:        cfg.MaxTurns,
		MaxTokens:       cfg.MaxTokens,
		Temperature:     cfg.Temperature,
		TopP:            cfg.TopP,
		Reasoning:       cfg.Reasoning,
		ToolChoice:      cfg.ToolChoice,
		ResponseFormat:  cfg.ResponseFormat,
		Extra:           cfg.Extra,
		Stream:          cfg.Stream,
		Parallelism:     cfg.Parallelism,
		Logger:          cfg.Logger,
		Output:          cfg.Output,
		Limits:          cfg.Limits,
		Budget:          cfg.Budget,
		StopAtTools:     stopAt,
		StopWhen:        cfg.StopWhen,
	})
	if err != nil {
		return nil, err
	}
	return &LLMAgent{cfg: cfg, rt: rt, hooks: hooks}, nil
}

// MustNew is like New but panics on error.
func MustNew(name string, opts ...Option) *LLMAgent {
	a, err := New(name, opts...)
	if err != nil {
		panic(err)
	}
	return a
}

// Name implements Agent.
func (a *LLMAgent) Name() string { return a.cfg.Name }

// Description implements Agent.
func (a *LLMAgent) Description() string { return a.cfg.Description }

// Runtime returns the underlying runtime.
func (a *LLMAgent) Runtime() *loop.Runtime { return a.rt }

// Hooks returns the agent's hook manager, or nil if it has no hooks. A
// Runner dispatches its SessionStart and SessionEnd events to it.
func (a *LLMAgent) Hooks() *hook.Manager { return a.hooks }

// Tools returns the tools available to the model, including skill, memory
// and sub-agent tools.
func (a *LLMAgent) Tools() *tool.Registry { return a.rt.Config().Tools }

// Run implements Agent. A run the model hands off continues on the agent
// it hands off to; see WithHandoffs.
func (a *LLMAgent) Run(ctx context.Context, in *Input) iter.Seq2[*loop.Event, error] {
	if in == nil || in.Message == nil {
		return func(yield func(*loop.Event, error) bool) { yield(nil, errors.New("agent: input has no message")) }
	}
	return a.run(ctx, in)
}

// Resume implements Resumer. It requires a checkpointer. With handoffs,
// the run resumes on the agent that was running it, found in the
// checkpointers of the agents the agent hands off to.
func (a *LLMAgent) Resume(ctx context.Context, runID string, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	if len(a.cfg.Handoffs) == 0 {
		return a.rt.Resume(withDeps(ctx, a.cfg.Deps), runID, approvals)
	}
	return a.resume(ctx, runID, approvals)
}

// ResumeState implements StateResumer. With handoffs, the run resumes on
// the agent the state names.
func (a *LLMAgent) ResumeState(ctx context.Context, st *loop.State, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	if len(a.cfg.Handoffs) == 0 {
		return a.rt.ResumeState(withDeps(ctx, a.cfg.Deps), st, approvals)
	}
	return a.resumeState(ctx, st, approvals)
}

// Discard implements Discarder by deleting the run's checkpoint, in the
// checkpointers of the agents it hands off to too.
func (a *LLMAgent) Discard(ctx context.Context, runID string) error {
	if len(a.cfg.Handoffs) == 0 {
		return a.rt.Discard(ctx, runID)
	}
	return a.discard(ctx, runID)
}

// Ask runs the agent on a single prompt without history and returns its
// final answer.
func (a *LLMAgent) Ask(ctx context.Context, prompt string) (string, error) {
	res, err := loop.Collect(a.Run(ctx, Text(prompt)), nil)
	if err != nil {
		return "", err
	}
	if res.Status != loop.StatusCompleted {
		return res.Output, fmt.Errorf("agent %s: run %s: %s", a.cfg.Name, res.Status, res.Reason)
	}
	return res.Output, nil
}

// MemoryProvider recalls memories relevant to each input and adds them as a
// reminder.
func MemoryProvider(m memory.Memory, scope string, limit int) actx.Provider {
	return scopedMemoryProvider(m, memory.FixedScope(scope), limit)
}

// scopedMemoryProvider is MemoryProvider with the scope of each input
// chosen by scope.
func scopedMemoryProvider(m memory.Memory, scope memory.ScopeFunc, limit int) actx.Provider {
	return actx.ProviderFunc(func(ctx context.Context, in *actx.Input) ([]actx.Section, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, nil
		}
		s, err := scope(ctx)
		if err != nil {
			return nil, err
		}
		hits, err := memory.RecallIn(ctx, m, s, in.Query, limit)
		if err != nil || len(hits) == 0 {
			return nil, err
		}
		return []actx.Section{{
			Name:      "memory",
			Content:   "Possibly relevant memories from earlier sessions:\n" + memory.Format(hits),
			Placement: actx.PlaceReminder,
		}}, nil
	})
}

// AsTool exposes an agent as a tool named after it. Each call runs the
// agent without history and returns its final answer. hooks, if non-nil,
// receives SubagentStart and SubagentStop events. Like Stop, a SubagentStop
// hook that denies with a reason, or returns AdditionalContext, makes a
// sub-agent that completed continue its conversation with them as input,
// up to hooks.MaxStopBlocks times in a row (8 by default); a continuation
// that calls tools resets the count. StopHookActive is set on the events
// that follow. The count and StopHookActive start over when the call
// resumes after the sub-agent interrupted for approvals. Hook errors are
// logged with the calling run's logger.
//
// The sub-agent's token usage is added to the run that called the tool.
// When the sub-agent needs approval for a tool call, the calling run is
// interrupted with the sub-agent's requests; resuming it resumes the
// sub-agent with the decisions.
//
// AsTool uses the defaults of ToolOptions; see AsToolWith.
func AsTool(a Agent, hooks *hook.Manager) tool.Tool {
	return AsToolWith(a, ToolOptions{Hooks: hooks})
}

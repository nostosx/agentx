package agent

import (
	"log/slog"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/tool/builtin"
)

// WithDescription describes the agent to parent agents.
func WithDescription(d string) Option { return func(c *Config) { c.Description = d } }

// WithModel sets the model.
func WithModel(m model.Model) Option { return func(c *Config) { c.Model = m } }

// WithInstructions sets the base system prompt.
func WithInstructions(s string) Option { return func(c *Config) { c.Instructions = s } }

// WithTools adds tools.
func WithTools(tools ...tool.Tool) Option {
	return func(c *Config) { c.Tools = append(c.Tools, tools...) }
}

// WithToolMiddleware adds tool middleware.
func WithToolMiddleware(mws ...tool.Middleware) Option {
	return func(c *Config) { c.ToolMiddlewares = append(c.ToolMiddlewares, mws...) }
}

// WithSkills adds skills.
func WithSkills(skills ...skill.Skill) Option {
	return func(c *Config) {
		if c.Skills == nil {
			c.Skills = skill.NewRegistry()
		}
		for _, s := range skills {
			c.Skills.Register(s)
		}
	}
}

// WithSkillRegistry adds the skills of a registry. They are copied into the
// agent's own registry, so r is not modified by other options, and skills
// registered in r later are not added.
func WithSkillRegistry(r *skill.Registry) Option { return WithSkills(r.List()...) }

// WithMemory enables long-term memory. recall is the number of memories
// recalled automatically for each input; zero disables automatic recall.
// Every run of the agent reads and writes memories in scope; an agent
// that serves several users, such as one behind a server, chooses the
// scope per run with WithMemoryScopeFunc instead.
func WithMemory(m memory.Memory, scope string, recall int) Option {
	return func(c *Config) { c.Memory, c.MemoryScope, c.MemoryRecall = m, scope, recall }
}

// WithMemoryScopeFunc enables long-term memory like WithMemory, with the
// scope of each recall and memory tool call taken from its context by
// scope, for example the user an HTTP middleware authenticated and stored
// in the request context the server runs the agent with. When scope fails,
// the recall or the tool call fails, so memories never leak across users
// whose scope is unknown.
func WithMemoryScopeFunc(m memory.Memory, scope memory.ScopeFunc, recall int) Option {
	return func(c *Config) { c.Memory, c.MemoryScopeFunc, c.MemoryRecall = m, scope, recall }
}

// WithoutMemoryTools hides the memory tools from the model.
func WithoutMemoryTools() Option { return func(c *Config) { c.DisableMemoryTools = true } }

// WithSubAgents exposes agents as tools.
func WithSubAgents(agents ...Agent) Option {
	return func(c *Config) { c.SubAgents = append(c.SubAgents, agents...) }
}

// WithHooks adds the hooks of a manager, including its FailClosed setting.
// They are copied into the agent's own manager, so m is not modified by
// other options, and hooks registered in m later are not added.
func WithHooks(m *hook.Manager) Option {
	return func(c *Config) {
		if c.Hooks == nil {
			c.Hooks = hook.NewManager()
		}
		c.Hooks.Merge(m)
	}
}

// WithHook registers a hook. Invalid matchers panic.
func WithHook(event hook.EventType, matcher string, h hook.Hook) Option {
	return func(c *Config) {
		if c.Hooks == nil {
			c.Hooks = hook.NewManager()
		}
		if err := c.Hooks.On(event, matcher, h); err != nil {
			panic(err)
		}
	}
}

// WithMiddleware adds runtime middleware, for example from the middleware
// package.
func WithMiddleware(mws ...loop.Middleware) Option {
	return func(c *Config) { c.Middlewares = append(c.Middlewares, mws...) }
}

// WithContext configures context engineering.
func WithContext(cfg actx.Config) Option { return func(c *Config) { c.Context = cfg } }

// WithContextProvider adds a context provider.
func WithContextProvider(ps ...actx.Provider) Option {
	return func(c *Config) { c.ContextProviders = append(c.ContextProviders, ps...) }
}

// WithCheckpointer persists run state for resumption.
func WithCheckpointer(cp loop.Checkpointer) Option { return func(c *Config) { c.Checkpointer = cp } }

// WithApprover decides tool approvals synchronously.
func WithApprover(a loop.Approver) Option { return func(c *Config) { c.Approver = a } }

// WithMaxTurns bounds model calls per run.
func WithMaxTurns(n int) Option { return func(c *Config) { c.MaxTurns = n } }

// WithMaxTokens bounds output tokens per model call.
func WithMaxTokens(n int) Option { return func(c *Config) { c.MaxTokens = n } }

// WithTemperature sets the sampling temperature.
func WithTemperature(t float64) Option { return func(c *Config) { c.Temperature = new(t) } }

// WithTopP sets nucleus sampling.
func WithTopP(p float64) Option { return func(c *Config) { c.TopP = new(p) } }

// WithReasoning configures model reasoning.
func WithReasoning(r *model.Reasoning) Option { return func(c *Config) { c.Reasoning = r } }

// WithToolChoice constrains tool use.
func WithToolChoice(tc *model.ToolChoice) Option { return func(c *Config) { c.ToolChoice = tc } }

// WithResponseFormat requests structured output.
func WithResponseFormat(f *model.ResponseFormat) Option {
	return func(c *Config) { c.ResponseFormat = f }
}

// WithExtra passes provider-specific request fields.
func WithExtra(key string, value any) Option {
	return func(c *Config) {
		if c.Extra == nil {
			c.Extra = map[string]any{}
		}
		c.Extra[key] = value
	}
}

// WithStreaming streams model output as delta events.
func WithStreaming() Option { return func(c *Config) { c.Stream = true } }

// WithParallelism bounds concurrent calls of read-only and concurrent
// tools.
func WithParallelism(n int) Option { return func(c *Config) { c.Parallelism = n } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(c *Config) { c.Logger = l } }

// WithInstructionFiles adds the instruction files, such as AGENTS.md, of
// dir and its parents up to the project root to the system prompt, with the
// defaults of actx.InstructionFiles.
func WithInstructionFiles(dir string) Option {
	return WithContextProvider(actx.InstructionFiles(actx.InstructionConfig{Dir: dir}))
}

// WithLimits bounds the usage of each run; Input.Limits overrides it for a
// run. See loop.Limits.
func WithLimits(l loop.Limits) Option { return func(c *Config) { c.Limits = l } }

// WithBudget ends a run with loop.StatusLimitExceeded when f returns an
// error for the run's usage so far, for example a cost limit. f is called
// before each model call and each batch of tool calls.
func WithBudget(f func(model.Usage) error) Option { return func(c *Config) { c.Budget = f } }

// WithStopAtTools ends a run as completed once one of the named tools
// returns a result that is not an error; the result's content is the run's
// output, and the model is not called again.
func WithStopAtTools(names ...string) Option {
	return func(c *Config) { c.StopAtTools = append(c.StopAtTools, names...) }
}

// WithStopWhen ends a run as completed when any condition holds after a
// turn's tool calls have run. See loop.StopCondition.
func WithStopWhen(conds ...loop.StopCondition) Option {
	return func(c *Config) { c.StopWhen = append(c.StopWhen, conds...) }
}

// WithSubAgentEvents forwards the events of the agent's sub-agents, and of
// their sub-agents, to the consumer of its runs. Forwarded events have a
// Depth above zero and ParentCallID set to the call that started the
// sub-agent; consumers that switch on Event.Type should check Depth. The
// sub-agents' events are not forwarded by default.
func WithSubAgentEvents() Option { return func(c *Config) { c.SubAgentEvents = true } }

// WithConcurrentSubAgents lets calls of the agent's sub-agents run in
// parallel with each other and with adjacent read-only calls, bounded by
// WithParallelism. By default sub-agents run one at a time, since they may
// have side effects whose order matters. Sub-agents interrupted for
// approval together are resumed together.
func WithConcurrentSubAgents() Option { return func(c *Config) { c.ConcurrentSubAgents = true } }

// WithMaxDepth bounds how deeply sub-agents can be nested below the agent;
// the default is DefaultMaxDepth. A sub-agent call beyond it returns an
// error result to the model.
func WithMaxDepth(n int) Option { return func(c *Config) { c.MaxDepth = n } }

// WithPermissions decides tool calls with a permission policy: rules and
// modes in Claude Code's syntax, see the permission package. The policy
// runs after the hooks, skills and WithMiddleware middleware, whatever the
// order of the options, so its deny and ask decisions hold whatever they
// allow; see the package documentation. A later WithPermissions replaces
// the policy, and nil removes it.
func WithPermissions(p *permission.Policy) Option {
	return func(c *Config) { c.Permissions = p }
}

// WithSkillConfig configures how the agent's skills run, for example
// enabling command injection with skill.Config.Shell; see
// Config.SkillConfig.
func WithSkillConfig(c skill.Config) Option { return func(cfg *Config) { cfg.SkillConfig = c } }

// WithSkillAgents makes agents available to context: fork skills, whose
// agent field names one of them. Without a match, general-purpose,
// Explore and Plan run on a new agent with the agent's model, tools and
// approver; see Config.SkillAgents.
func WithSkillAgents(agents ...Agent) Option {
	return func(c *Config) { c.SkillAgents = append(c.SkillAgents, agents...) }
}

// WithFileHistory gives the agent the file history its file tools save
// to, the same as builtin.Config.History, so a Runner starts a checkpoint
// of it for each run and Runner.Rewind can restore the files; see
// builtin.FileHistory.
func WithFileHistory(h builtin.History) Option { return func(c *Config) { c.FileHistory = h } }

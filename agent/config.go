package agent

import (
	"context"
	"log/slog"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/tool/builtin"
)

// Config configures an LLMAgent. Build it with options passed to New.
type Config struct {
	Name         string
	Description  string
	Model        model.Model
	Instructions string
	Tools        []tool.Tool
	// ToolMiddlewares wrap every tool call; the first is outermost.
	ToolMiddlewares []tool.Middleware

	// Skills are advertised in the system prompt and loaded with the skill
	// tool.
	Skills *skill.Registry
	// SkillConfig configures how skills run; see skill.Config. The agent
	// fills in what it leaves empty: Fork runs context: fork skills as
	// sub-agents (see WithSkillAgents), Effort defaults to Reasoning's,
	// and HookOptions.Model to Model.
	SkillConfig skill.Config
	// SkillAgents are the agents context: fork skills can name in their
	// agent field.
	SkillAgents []Agent
	// forkedSkill is set on the sub-agents of context: fork skills, so
	// they apply the skill's tool rules and effort without skills of their
	// own.
	forkedSkill bool

	// Memory enables long-term memory tools and automatic recall.
	Memory memory.Memory
	// MemoryScope scopes memory reads and writes, for example to a user.
	MemoryScope string
	// MemoryScopeFunc, if set, chooses the scope of each run instead of
	// MemoryScope, for an agent that serves several users.
	MemoryScopeFunc memory.ScopeFunc
	// MemoryRecall is the number of memories recalled automatically for each
	// input. Zero disables automatic recall; the tools remain available.
	MemoryRecall int
	// DisableMemoryTools hides the memory tools from the model.
	DisableMemoryTools bool

	// SubAgents are exposed to the model as tools named after the agents.
	SubAgents []Agent

	Hooks *hook.Manager
	// Middlewares extend the agent loop. They run after the hook middleware
	// and before the context middleware.
	Middlewares []loop.Middleware
	// Context configures context engineering. The agent adds its own
	// providers for skills and memory.
	Context          actx.Config
	ContextProviders []actx.Provider

	Checkpointer loop.Checkpointer
	Approver     loop.Approver

	MaxTurns       int
	MaxTokens      int
	Temperature    *float64
	TopP           *float64
	Reasoning      *model.Reasoning
	ToolChoice     *model.ToolChoice
	ResponseFormat *model.ResponseFormat
	Extra          map[string]any
	Stream         bool
	Parallelism    int
	Logger         *slog.Logger

	// Output, Limits, Budget, StopAtTools and StopWhen configure the
	// runtime fields of the same names.
	Output      *loop.Output
	Limits      loop.Limits
	Budget      func(model.Usage) error
	StopAtTools []string
	StopWhen    []loop.StopCondition
	// SubAgentEvents forwards the events of sub-agents; see
	// ToolOptions.Events.
	SubAgentEvents bool
	// ConcurrentSubAgents lets sub-agent calls run concurrently; see
	// ToolOptions.Concurrent.
	ConcurrentSubAgents bool
	// MaxDepth bounds sub-agent nesting; see ToolOptions.MaxDepth.
	MaxDepth int
	// FileHistory is the history Runner.Rewind restores files from; see
	// WithFileHistory.
	FileHistory builtin.History

	// Handoffs are the agents the model can hand the run off to; see
	// WithHandoffs. MaxHandoffs bounds the handoffs of a run started on
	// this agent; zero means DefaultMaxHandoffs.
	Handoffs    []Handoff
	MaxHandoffs int
	// Guardrails check the input, output and tool calls of runs; see
	// WithInputGuardrail.
	Guardrails guard.Config
	// Deps add default dependencies to the context of runs; see WithDeps.
	Deps []func(context.Context) context.Context
	// InstructionFuncs extend Instructions for each run or turn; see
	// WithInstructionsFunc.
	InstructionFuncs []Instruction
	// InstructionTemplate fills {key} placeholders in Instructions from
	// the session state; see WithInstructionTemplate.
	InstructionTemplate bool
}

// Option configures an agent.
type Option func(*Config)

// Package agentx is a modular Go framework for building LLM agents.
//
// The framework is organised as a set of small, composable packages that
// depend only on the standard library and kitx:
//
//   - model:     provider-neutral messages, requests, responses and streaming
//   - model/modeltest: a scripted model for tests
//   - tool:      tool definitions, JSON Schema, registry and execution
//   - tool/builtin: file, search, shell, web and todo tools
//   - loop:      the agent loop, tool scheduling, events, checkpoints and
//     the middleware interface that everything below plugs into
//   - middleware: tracing and metrics of agent runs and the types other
//     middleware builds on
//   - agent:     agent construction, sub-agents and the session-aware runner
//   - actx:      context-window management and compaction strategies
//   - session:   conversation sessions and their stores
//   - memory:    long-term memory with keyword and vector retrieval
//   - rag:       retrieval-augmented generation: loaders, splitters, indexes
//     and retrievers
//   - skill:     Claude Code style skills loaded from SKILL.md files
//   - hook:      lifecycle hooks, including shell command hooks
//   - permission: permission rules and modes for tool calls
//   - guard:     input, output and tool guardrails
//   - workflow:  DAG workflows that orchestrate agents and functions
//   - mcp:       Model Context Protocol client, server and tool adapter
//   - server:    HTTP serving of runners: session API, Vercel AI SDK, AG-UI
//     and A2A
//   - eval:      evaluation of agents, and eval/replay to record and replay
//     HTTP interactions
//
// Each contract comes with an in-memory reference implementation. Model
// providers (OpenAI, Anthropic, Gemini, Bedrock), file and SQL stores,
// OpenTelemetry, logging and retry middleware live in separate modules
// under github.com/nostosx/agentx-contrib.
package agentx

// Version is the framework version reported to remote services such as MCP
// servers.
const Version = "1.0.1"

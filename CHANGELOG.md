# Changelog

Notable changes to agentx are recorded here. Patch releases preserve the
public API. Since v1.1.0, minor releases may also contain breaking changes;
each is listed under "Breaking changes" with its migration. The release
workflow uses each version section as its release notes.

Go 1.27.3 is the toolchain requirement, independent of the library version.

## Unreleased

### Breaking changes

Compatibility with data and APIs of earlier releases is removed:

- `actx.WrapReminder` is removed; use `model.WrapReminder`.
- Command hooks no longer accept the `updatedOutput` field of
  `hookSpecificOutput`; return `updatedToolOutput` (or
  `updatedMCPToolOutput`).
- The Stop hook continuation counter no longer reads the `"true"` value that
  checkpoints of earlier releases stored; such a value counts as zero.
- `Runtime.Resume` and `ResumeState` accept only states of the current
  `loop.StateVersion`; states without a version are rejected. New in-memory
  states carry the version from the start.
- Rewind checkpoint records without a stored message are no longer
  recognized as legacy records.

## v1.1.0

Release date: 2026-10-27.

Upgrade the agentx-contrib `provider`, `otel` and `middleware` modules to
v1.1.0 together with this release: their v1.0.0 versions do not build with
agentx v1.1.0. `filestore` and `sqlstore` v1.0.0 keep working.

### Breaking changes

- **Telemetry interfaces moved to the new `telemetry` package.** `Attr`,
  `SpanKind`, `Span`, `EventSpan`, `Tracer`, `NoopTracer`, `StartSpan`,
  `SpanFromContext`, `AddEvent`, `RecordSpanError`, `ErrorType`,
  `Instrument`, `Int64Counter`, `Int64Histogram`, `Float64Histogram`,
  `Meter` and `NoopMeter` are no longer in `middleware`. Replace
  `middleware.X` with `telemetry.X` and write `telemetry.Attr{Key: k,
  Value: v}`. `middleware.MarkCompacted(ctx, run)` becomes
  `telemetry.MarkCompacted(ctx, run.ID())`. `middleware` keeps `Tracing`,
  `Metrics`, `TraceModel`, `TraceTools`, `MeterModel`, `MeterTools`,
  `Model`, `Wrap` and `Funcs`.
- **Option structs are passed by value.** `mcp.NewServer(info, opts)`,
  `(*mcp.Server).HTTPHandler(opts)`, `mcp.AgentTool(a, opts)` and
  `mcp.WithOAuth(cfg)` take `ServerOptions`, `HTTPOptions`,
  `AgentToolOptions` and `OAuthConfig` values; pass the zero value where
  `nil` was passed. `WithOAuth` keeps a copy, so call
  `LoopbackRedirect.Configure(&cfg)` before it.
- **One agent-as-tool adapter.** `agent.AsTool(a, hooks)` becomes
  `agent.AsTool(a, agent.ToolOptions{Hooks: hooks})`; `AsToolWith` is
  renamed `AsTool`.
- **Shared run settings.** The runtime fields of `loop.Config` and
  `agent.Config` (name, model, request options, checkpointer, approver,
  limits and stop conditions) moved into the embedded `loop.Settings`.
  Keyed literals and field access keep working; unkeyed literals must name
  their fields.
- **Fixed permission position.** `WithPermissions` sets the new
  `agent.Config.Permissions` instead of appending to `Middlewares`. The
  policy runs after all `WithMiddleware` middleware, whatever the order of
  the options; a later `WithPermissions` replaces it and `nil` removes it.
  `agent.New` fails if the same policy is also passed with
  `WithMiddleware`.
- **Provider options by provider.** `model.Request.Extra`,
  `loop.Settings.Extra` and `agent.WithExtra` are removed. Use
  `ProviderOptions`, keyed by the provider adapter (`"openai"`,
  `"anthropic"`, `"gemini"` or `"bedrock"`), and
  `agent.WithProviderOptions(provider, map[string]any{...})`. A seed moves
  to the portable `Request.Seed` and `agent.WithSeed`.
- **Typed reasoning options.** `model.Reasoning.Effort` has type
  `model.Effort` and `Display` has type `model.ReasoningDisplay`; untyped
  string constants still assign, string variables need a conversion.
- **`server.Handler` is removed.** Use `server.New`, whose `*Server` is an
  `http.Handler` with `Close` and the new `Shutdown`.
- **Checkpoint errors.** `workflow.ErrCheckpointNotFound` and
  `builtin.ErrCheckpointNotFound` now wrap `loop.ErrCheckpointNotFound`, so
  `errors.Is(err, loop.ErrCheckpointNotFound)` matches all three. The text
  of the file history error is now "file history: checkpoint not found".

### Added

- `server.(*Server).Shutdown(ctx)` bounds the wait for background A2A
  tasks; tasks requested after it started run in their request with a
  canceled context.
- `model.Request.Seed`, `agent.WithSeed`, `model.CloneProviderOptions` and
  the `model.Effort` and `model.ReasoningDisplay` constants.
- `loop.Decision.String`, and the `agent.Event` and `agent.Result` aliases
  of `loop.Event` and `loop.Result`.
- CONTRIBUTING documents when option structs are passed by value.

### Downstream migration

Modules that adopt v1.1.0 need these changes:

- agentx-contrib `provider`: read `req.ProviderOptions[providerName]`
  instead of `req.Extra` (OpenAI Responses keeps its `include` merge), map
  `req.Seed` (OpenAI `seed`, Gemini `generationConfig.seed`) and convert
  `Reasoning.Effort` and `Display` to strings where compared.
- agentx-contrib `otel`: implement `telemetry.Tracer`, `telemetry.Meter`
  and the instrument interfaces, and import the `telemetry` types instead
  of `middleware`'s.
- agentx-contrib `middleware`: retry uses `telemetry.AddEvent`,
  `telemetry.Attr` and `telemetry.ErrorType`.
- agentx-examples: its own code needs no change; update agentx and the
  provider module together.

### Compatibility

- Requires Go 1.27.3 and kitx v1.0.0; no new module dependencies.

## v1.0.1

Release date: 2026-10-27.

- Security: `context: fork` skills run by the default `general-purpose`
  sub-agent now apply the parent agent's permission policy and tool
  guardrails. Previously the fork received the parent's tools without them,
  so a forked skill could run calls the parent's rules deny or ask about.
  Agents passed to `WithSkillAgents` keep their own configuration.
- Fix MCP version negotiation with a slowly starting server of the current
  revision: when the server rejected the fallback `initialize` before
  answering the probe, or both answers arrived together, the client could
  fail with the `initialize` error instead of using the probe's answer. An
  `initialize` error now waits for a pending probe.
- Deprecate `server.Handler` in favor of `server.New`. The `http.Handler` it
  returns cannot be closed, so background A2A tasks kept running until the
  process exited; `(*server.Server).Close` cancels them and waits for their
  sessions to be saved. `Handler` keeps working.
- MCP OAuth metadata, client registration and token responses larger than
  1 MiB now fail with an explicit "response larger than" error instead of
  being truncated and reported as malformed JSON. Read errors of successful
  responses are reported instead of ignored, and an invalid registration
  response is quoted only up to 300 bytes.
- Document the tool call pipeline in the `agent` package and both READMEs:
  the order in which hooks, skills, middleware, permissions, guardrails,
  approval and tool middleware see a call, and which mechanism to use for
  which purpose. Regression tests pin the documented order.
- Complete the standalone quick-start steps, including module initialization,
  the program entry point and provider credentials and execution requirements.
- Remove obsolete sibling-checkout instructions for agentx-examples.
- Existing v1 APIs remain unchanged. Requires Go 1.27.3 and kitx v1.0.0;
  no new module dependencies.

## v1.0.0

Release date: 2026-10-23.

Initial release of `github.com/nostosx/agentx`, requiring Go
1.27.3 and the published `github.com/nostosx/kitx v1.0.0` module.

- Streaming agent runs with typed tools, parallel tool scheduling, structured
  output, usage limits, approvals, resumable checkpoints and agent handoffs.
- Built-in file, search, patch, shell, web-fetch and todo tools; file tools
  confined by `os.Root`, read-before-write checks and optional file history.
- Lifecycle hooks, skills, instruction files, permission rules and modes;
  approved argument edits are revalidated before tool execution.
- Optional macOS and Linux shell sandboxes. Platform support and remaining
  limitations are described in [SECURITY.md](SECURITY.md).
- Session state and rewind, context compaction, long-term memory and
  retrieval-augmented generation, with in-memory stores and conformance
  suites for alternative implementations.
- Input, output and tool guardrails, including held output for applications
  that must validate final answers before displaying them. Conversation and
  checkpoint fields can retain drafts and must be treated as internal state.
- DAG workflows, tracing and metrics interfaces, evaluation, and HTTP
  recording and replay for provider tests.
- MCP clients and servers, OAuth, and HTTP session serving with Vercel AI
  SDK, AG-UI and A2A protocol adapters.
- A core dependency boundary limited to the standard library and kitx,
  enforced for production code and tests. Provider adapters, persistent
  stores, OpenTelemetry, logging and retry implementations are separate
  [agentx-contrib](https://github.com/nostosx/agentx-contrib) modules.
- Runnable applications in
  [agentx-examples](https://github.com/nostosx/agentx-examples), with library
  Example tests kept alongside their APIs.
- Make entry points for formatting, modernization, module tidiness, vet,
  build, dependency boundaries, shuffled race tests, lint and vulnerability
  scans; CI definitions for Linux, macOS and Windows.

# Changelog

Notable changes to agentx are recorded here. Versions follow
[semantic versioning](https://semver.org). Published v1 releases preserve
public API compatibility. The release workflow uses each version section
as its release notes.

Go 1.27.3 is the toolchain requirement, independent of the library version.

## v1.0.1

Release date: 2026-10-27.

- Security: `context: fork` skills run by the default `general-purpose`
  sub-agent now apply the parent agent's permission policy and tool
  guardrails. Previously the fork received the parent's tools without them,
  so a forked skill could run calls the parent's rules deny or ask about.
  Agents passed to `WithSkillAgents` keep their own configuration.
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

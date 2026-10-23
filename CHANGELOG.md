# Changelog

Notable changes to agentx are recorded here. Versions follow
[semantic versioning](https://semver.org). Published v1 releases preserve
public API compatibility. The release workflow uses each version section
as its release notes.

Go 1.27.3 is the toolchain requirement, independent of the library version.

## Unreleased

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

# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/nostosx/agentx/security/advisories/new)
for this repository, or for
[agentx-contrib](https://github.com/nostosx/agentx-contrib/security/advisories/new)
when the issue is in one of its modules. If private reporting is unavailable,
open an issue asking for a private reporting channel without disclosing the
vulnerability or exploit details.

Include the version or commit, a description of the impact and, if you
can, a command, program or test that reproduces it. Keep reproduction data
free of credentials and personal information. Coordinate disclosure privately
until a fix or mitigation is available.

## Supported versions

The first release is in preparation; there is no supported published agentx
version yet. Report issues against a specific commit. Starting with v1.0.0,
security fixes will target the latest minor release in the v1 series.

## Scope and known limitations

agentx runs model-chosen tool calls on your machine. The permission
package, the sandbox of the built-in shell tools and the guardrails reduce
what a misbehaving or prompt-injected model can do; they are not a
security boundary on their own. Run agents that act on untrusted input
with the least privilege, in a container or VM where it matters.

The following limitations remain. Account for them when deciding whether
and how to enable these capabilities for untrusted input:

- The Linux sandbox (bubblewrap) leaves Unix sockets in the file system
  reachable, such as a D-Bus session bus or an ssh-agent socket, and does
  not stop a sandboxed command from creating a protected path that does not
  exist yet. Set `AskSandboxed` so sandboxed commands still ask, when
  that matters.
- `web_fetch` is read-only, so it runs without approval in default and
  plan modes. A model that read a secret can send it to any URL it fetches;
  deny the tool or limit it with `WebFetch(domain:...)` rules when that
  matters.
- The state of the built-in tools (the shell's working directory, the files
  read before an edit, the todo list) belongs to the tool instance. Give
  each session its own tools when one process serves several users.
- Usage limits are checked before model calls and tool batches; they do not
  interrupt individual calls that are already running. Sub-agent usage is
  accounted for when the sub-agent returns. Apply timeouts and downstream
  resource limits when a strict ceiling is required.
- Custom middleware that enforces argument policies in `BeforeTool` must
  also implement `loop.ToolRevalidator` to check arguments edited during
  approval. The built-in permission policy and tool guardrails implement it.

## Dependencies

The agentx module depends only on the Go standard library and kitx. CI
rejects other module dependencies, including test-only dependencies. The
modules of agentx-contrib list their dependencies in their go.mod files.

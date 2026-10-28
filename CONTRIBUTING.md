# Contributing to agentx

Bug reports, fixes and documentation improvements are welcome. Discuss new
features and public API changes in an issue before implementing them.

Report security issues privately; see [SECURITY.md](SECURITY.md).

Install [Task](https://taskfile.dev/docs/installation/) v3.53.1 outside the module,
matching CI, and put its binary on `PATH`:

```sh
go install github.com/go-task/task/v3/cmd/task@v3.53.1
```

Run `task --list` to discover tasks. Tool overrides accept environment variables or
Task arguments, for example `task check GO="/path with spaces/go"`.
Checks force `GOWORK=off`; `check`, `lint` and `vulncheck` do not apply source fixes.

## Where code belongs

- **agentx** (this repository) holds the contracts and one in-memory
  reference implementation of each, and depends only on the Go standard
  library and kitx, tests included. The dependency checks inside `task check` enforce it.
- **[agentx-contrib](https://github.com/nostosx/agentx-contrib)** holds the
  other implementations: model providers, file and SQL stores,
  OpenTelemetry, logging and retry middleware, and anything that needs a
  third-party module or targets one service. Each directory there is its own
  module.

A new implementation of a store contract must pass the conformance suite of
its contract: `session/sessiontest`, `loop/checkpointtest`,
`workflow/checkpointtest`, `memory/memorytest` or `rag/ragtest`.

Internal helpers have specific owners: `internal/process` handles tool and hook
command lifetimes, `internal/media` adapts model parts, `internal/httpclient`
assembles requests, and `internal/fsmatch` preserves permission matching rules.
Generic synchronized maps, data URL codecs and text truncation live in kitx.
Use standard-library UUID generation directly; do not recreate a utils package.

## API conventions

- Pass option and configuration structs by value when their zero value means
  the defaults and the callee copies them at construction, such as
  `mcp.ServerOptions`, `mcp.HTTPOptions`, `mcp.OAuthConfig` and
  `agent.ToolOptions`. Use pointers only for values with shared identity or
  mutable state, such as `*hook.Manager`, `*permission.Policy` and
  `*http.Client`.
- Do not add a second constructor or adapter for the same concept that only
  differs by its options; extend the options struct instead.

## Development

Use Go 1.27.3, Task v3.53.1 and Bash. The module resolves kitx v1.0.0 from
the Go module proxy; no sibling kitx checkout is needed. For linting, install
[golangci-lint](https://golangci-lint.run) v2.15.0 built with Go 1.27.3;
for vulnerability scans, install govulncheck v1.9.0 outside this module:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
```

`task --list` lists all targets. `GO`, `GOFMT`, `GOLANGCI_LINT` and `GOVULNCHECK`
can select installed executables.

```sh
task check      # format, modernizer and tidy checks, vet, build, dependency boundary, race tests
task lint       # golangci-lint
task vulncheck  # reachable vulnerability scan
task test       # unit tests without the race detector
task test RACE=true   # race tests with shuffled execution order
task test COVER=true  # race tests, coverage.out and function coverage
```

`task check` rejects changes suggested by gofmt, `go fix` or `go mod tidy`;
it does not apply them. Use `task fmt`, `task fix` or `task tidy` to apply
the corresponding changes, then review the diff. All targets set `GOWORK=off`
so a personal workspace does not change dependency resolution.

The public commands are `check`, `test`, `build`, `lint`, `vulncheck`, `fmt`,
`fix` and `tidy`. Running `task` selects `check`. `RACE` and `COVER` accept
`true` or `false`; coverage implies the race detector. Fine-grained quality
steps are internal, so `task --list` and `task --list-all` show the same eight
commands.

CI runs `task check` on Linux, macOS and Windows, with independent lint and
vulnerability jobs. Before requesting review, run all three local gates.
Passing local checks does not prove the remote CI run or a module release.

## Pull requests

- Keep a pull request to one change, with tests that fail without it.
- Follow the style of the surrounding code: English doc comments on every
  exported identifier, errors prefixed with the package name, no global
  mutable state. Keep both READMEs (English and Chinese) in step when you
  change documented behavior.
- Write concise Chinese commit subjects with a conventional prefix such as
  `fix:`, `feat:` or `docs:`. Explain the reason in the body when the change
  needs context.
- Add a line to the "Unreleased" section of [CHANGELOG.md](CHANGELOG.md)
  for changes users notice.

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).

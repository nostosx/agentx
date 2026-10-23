# agentx

English | [简体中文](README.zh-CN.md)

agentx is a Go framework for agents that call tools, maintain conversations
and resume interrupted work. It provides a streaming agent loop, typed tools,
permissions, checkpoints and context management through composable packages.

- **Small dependency boundary.** The module uses the Go standard library and
  [kitx](https://github.com/nostosx/kitx) for shared primitives. It
  holds the contracts and one in-memory reference implementation of each;
  model providers, file and SQL stores, OpenTelemetry, logging and retries
  live in [agentx-contrib](https://github.com/nostosx/agentx-contrib).
- **Small interfaces.** `model.Model`, `tool.Tool`, `hook.Hook`,
  `session.Store` and the other core types are small interfaces, so you can
  replace any one of them on its own.
- **Iterator-based streaming.** Runs are `iter.Seq2[*loop.Event, error]`
  and model streams `iter.Seq2[*model.StreamEvent, error]`, so you read them
  with a plain `for range` loop.

> Requires Go 1.27.3 or later. The first release, v1.0.0, is in preparation.
> Published v1 releases preserve public API compatibility; see
> [CHANGELOG.md](CHANGELOG.md).

## Installation

The first release is being prepared; the `v1.0.0` notes do not indicate a
published version. Use the installation command below after the core and
provider module are published. For current development, follow
[agentx-contrib's sibling-checkout setup](https://github.com/nostosx/agentx-contrib#development).
Go 1.27.3 is the toolchain requirement; library versions follow semantic
versioning independently. The core uses the published kitx v1.0.0;
a local kitx checkout is not required.

```sh
go get github.com/nostosx/agentx@v1.0.0 github.com/nostosx/agentx-contrib/provider@v1.0.0
```

The core module defines `model.Model`; the OpenAI, Anthropic, Gemini and
Bedrock implementations are in the `provider` module of agentx-contrib,
which calls provider APIs directly over `net/http` without provider SDKs.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nostosx/agentx-contrib/provider/anthropic"
	"github.com/nostosx/agentx/agent"
)

func main() {
	m, err := anthropic.New() // reads ANTHROPIC_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New("assistant",
		agent.WithModel(m),
		agent.WithInstructions("You are a concise assistant."),
	)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	answer, err := a.Ask(ctx, "What is an agent loop?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(answer)
}
```

## Features

[Tools](#typed-tools) · [Runs](#the-agent-loop-and-events) ·
[Permissions](#permissions) · [Sessions](#sessions) ·
[RAG](#retrieval-augmented-generation) · [Workflows](#workflows) ·
[MCP](#mcp) · [Serving](#serving)

### Typed tools

`tool.Func` builds the JSON Schema from the argument struct and checks the
model's arguments against it before your function runs. The `jsonschema`
struct tag refines the schema in either of the Go ecosystem's two forms. A
plain tag is the field's description, as in
[google/jsonschema-go](https://github.com/google/jsonschema-go), which the
official MCP Go SDK and ADK Go use. A tag that starts with a keyword uses the
`key=value` form of [invopop/jsonschema](https://github.com/invopop/jsonschema):
`title`, `description`, `enum` (repeatable), `default`, `format`, `pattern`,
`minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `minLength`,
`maxLength`, `minItems`, `maxItems` and the flags `required` and `nullable`.
Escape a comma inside a value as `\\,`; `jsonschema:"-"` leaves a field out.

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"City name, e.g. Tokyo"`
	Unit string `json:"unit,omitempty" jsonschema:"enum=celsius,enum=fahrenheit,default=celsius"`
	Days int    `json:"days,omitempty" jsonschema:"minimum=1,maximum=7,description=Forecast days\\, 1 for today"`
}

weather := tool.Func("get_weather", "Get the current weather for a city.",
	func(ctx context.Context, in weatherArgs) (string, error) {
		return in.City + ": 22°C, partly cloudy", nil
	}, tool.ReadOnly())

a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(weather))
```

- Calls to `ReadOnly` or `Concurrent` tools that come back to back in one
  model response run concurrently. A `Concurrent` tool may have side
  effects, so a resumed run does not repeat a call that was cut off.
- Errors, invalid arguments and panics come back to the model as error
  results. They do not stop the run.
- Arguments must be I-JSON (RFC 7493): JSON that repeats a member name or
  holds invalid UTF-8 is invalid, since hooks, permission rules, tools and
  MCP servers could otherwise read it differently. For the same reason a
  member that spells a declared property in another case, such as
  `Command` next to `command`, is invalid: `encoding/json` would decode it
  into the field while hooks and rules read the exact key.
- `tool.Media(text, parts...)` returns images or documents with the text;
  see [Multimodal content](#multimodal-content). `Result.Metadata` reaches
  `model.ToolResult.Metadata` and events, not the model.

### Built-in tools

`tool/builtin` provides general-purpose tools modeled on Claude Code's and
Codex's:

| Tool | Purpose | Approval |
| --- | --- | --- |
| `read_file` | Read a text file with line numbers, page by page; images (PNG, JPEG, GIF, WebP up to 5 MB) and PDFs (up to 20 MB) come back as parts for the model to view | No (read-only) |
| `write_file` | Create or overwrite a file; an overwrite returns the diff | Yes |
| `edit_file` | Replace exact text; the match must be unique unless `replace_all` is set. `edits` makes several replacements in order, all or none. Returns the diff | Yes |
| `apply_patch` | Apply a Codex-format patch that adds, updates, renames and deletes files, all or none. Returns a summary and the diffs | Yes |
| `glob` | Find files by pattern, with `**` and `{a,b}`; newest first | No (read-only) |
| `grep` | Search contents with a regular expression, with optional context lines; skips binary files | No (read-only) |
| `bash` | Run a shell command with a timeout, or in the background with `run_in_background`; long output keeps its head and tail | Yes, unless sandboxed |
| `bash_output` | Read a background command's new output, optionally filtered by a regular expression, and its status | No |
| `kill_shell` | Stop a background command and every process it started | No |
| `web_fetch` | Fetch a URL and convert HTML to text | No (read-only) |
| `todo_write` | Maintain a task list the application can display | No |

```go
todos := &builtin.TodoList{}
a := agent.MustNew("coder", agent.WithModel(m),
	agent.WithTools(builtin.Tools(builtin.Config{Dir: ".", Todos: todos})...))
```

- **File confinement.** File tools use `os.Root`, so neither `..` nor a
  symbolic link can reach outside `Config.Dir`.
- **Read before write.** The tools returned by `builtin.Tools` share a
  `FileTracker`: `write_file`, `edit_file` and `apply_patch` change or
  delete an existing file only after `read_file` has read it, and refuse if
  it changed since then.
- **Atomic edits.** Files are replaced through a temporary file in the same
  directory, keeping their permissions; a symbolic link is written through
  in place. `apply_patch` checks every operation before writing anything
  and restores the files it changed if a later write fails.
- **Edit formats.** `edit_file` follows Claude Code's Edit tool; its
  `edits` argument replaces the former MultiEdit tool. `apply_patch` takes
  the patch format GPT models are trained on and locates changes like
  Codex: exactly, then ignoring trailing whitespace, surrounding whitespace
  and typographic punctuation. The OpenAI Responses API receives it as a
  freeform tool constrained by Codex's Lark grammar; other providers send
  the patch as the JSON argument `input`. `Config.Edit` selects
  `builtin.EditAll` (default), `EditReplace` (only `edit_file`) or
  `EditPatch` (only `apply_patch`). Use `tool.Freeform` for freeform tools
  of your own.
- **Ignored files.** `glob` and `grep` always skip `.git` and
  `node_modules`, and skip paths excluded by `.gitignore` unless the model
  sets `include_ignored`. They also honor `.git/info/exclude` and the
  `.gitignore` files of parent directories up to the repository root. A
  `.git` file or `commondir` is followed only to a local git directory
  containing `HEAD`, and network paths, such as UNC shares, are never
  looked up.
- **Shell access.** `bash` runs with bash when it is on `PATH`, otherwise
  `sh` (`cmd.exe` on Windows). It starts in `Config.Dir` but, without a
  sandbox, is not confined to it. It requires approval unless you set
  `Config.NoApproval`. When the
  command exits or times out, its whole process tree is killed, including
  background processes. As in Claude Code, a `cd` carries over to later
  calls while it stays inside `Config.Dir` (links resolved); otherwise the
  next call starts in `Config.Dir` and the result says `Shell cwd was reset
  to <dir>`. The shell reports its directory from an `EXIT` trap on
  descriptor 3; `Config.ResetDir` turns this off, and Windows never tracks it.
- **Background commands.** With `run_in_background`, `bash` returns an ID
  such as `bash_1` at once. `bash_output` returns what the command wrote
  since the last read and whether it is running or how it ended;
  `kill_shell` kills its process tree. A `builtin.Shells` holds them (Tools
  creates one): at most 8 run at once, each keeps its last 1 MiB of output
  and is killed after 30 minutes unless `timeout_ms` asks for up to 2 hours.
  Call `builtin.Close(tools)` or `Shells.Close` when the agent is done. They
  match Claude Code's `BashOutput`/`TaskOutput` and `KillShell`/`TaskStop`
  in hooks and permission rules.
- **Sandbox.** `Config.Sandbox` runs every `bash` command, background ones
  included, inside an OS sandbox. `builtin.NewSandbox(policy)` uses Seatbelt
  (`/usr/bin/sandbox-exec`) on macOS with a deny-by-default profile modeled
  on Codex's, and bubblewrap (`bwrap`) on Linux with Codex's mounts
  (`--ro-bind / /`, writable roots bound back, fresh user, PID, IPC and
  network namespaces, `--die-with-parent`). Commands read everything (or
  only `ReadableRoots`) except `DenyRead`, write only to `Config.Dir`,
  `WritableRoots` and the temporary directories, never to `.git`, `.agents`,
  `.codex`, `.aws` or `.claude` at the top of those roots, and have no
  network unless `AllowNetwork` is set or `AllowedDomains` lists hosts,
  which a local HTTP/CONNECT proxy enforces by name through `HTTP_PROXY`
  and `HTTPS_PROXY` (socat bridges it on Linux). Like Claude Code's
  auto-allow mode, sandboxed commands need no approval (`AskSandboxed`
  restores it); a command retried with `dangerously_disable_sandbox` runs
  outside and always needs approval, which `NoSandboxEscape` forbids.
  Permission rules still apply, for example
  `Bash(dangerously_disable_sandbox:true)`. When no backend works,
  `SandboxRequired` (the default) fails the command and `SandboxBestEffort`
  runs it unsandboxed after approval. Limitations: the proxy does not
  inspect TLS, programs that ignore the proxy variables have no network,
  setuid programs such as `ps` do not run on macOS, Linux uses no seccomp or
  Landlock, so only container daemon sockets are hidden from commands and
  only existing paths are protected. Linux installs protection mounts after
  every writable bind, including overlapping parent roots. Windows has no
  backend.

  ```go
  sb := builtin.NewSandbox(builtin.SandboxPolicy{
      DenyRead:       []string{"~/.ssh", "~/.aws"},
      AllowedDomains: []string{"proxy.golang.org", "*.githubusercontent.com"},
  })
  defer sb.Close()
  tools := builtin.Tools(builtin.Config{Dir: ".", Sandbox: sb})
  defer builtin.Close(tools)
  ```
- **Network access.** `web_fetch` refuses loopback, private, link-local and
  shared (CGNAT) addresses, including after redirects and, for direct
  connections, DNS changes. A proxy from the environment still works, but
  then destinations are checked by name before each request only, since the
  proxy resolves them again. Fetches time out after 60 seconds. Set
  `Config.AllowPrivateNetwork` to allow these addresses, or pass your own
  `HTTPClient`, which replaces these checks.
- **Read-only subset.** `builtin.ReadOnlyTools` returns only the tools
  without side effects, which suits research sub-agents.
- **File history.** With `Config.History`, `write_file`, `edit_file` and
  `apply_patch` save each file's state before changing it, so a session
  can be rewound; see [Checkpoints and rewind](#checkpoints-and-rewind).
  Changes made by `bash`, sandboxed or not, are not tracked.

### The agent loop and events

`Run` returns a stream of events:

- `run_start` and `turn_start`
- `compacted`, when the conversation was compacted before the model call
- `delta`, only when streaming is enabled
- `message`
- `tool_call` and `tool_result`
- `output_retry`, when the final answer did not match the output schema and
  was sent back (`Event.OutputRetry` holds the problems and attempt)
- `handoff`, when the run moves to another agent (see Handoffs)
- `interrupted`
- `run_end`

```go
for ev, err := range a.Run(ctx, agent.Text("Plan a trip to Kyoto")) {
	if err != nil {
		log.Fatal(err)
	}
	switch ev.Type {
	case loop.EventDelta:
		fmt.Print(ev.Text())
	case loop.EventToolCall:
		fmt.Println("->", ev.ToolCall.Name)
	case loop.EventRunEnd:
		fmt.Println("\nstatus:", ev.Result.Status)
	}
}
```

`loop.Collect` drains the events and returns a `*loop.Result`. The
result holds the output text, the full conversation, token usage and the
number of turns.

### Structured output

`agent.WithOutput[T]` asks the model for JSON matching the schema of the
struct `T` and checks the final answer against it. `agent.AskAs[T]` runs a
prompt and decodes the answer; the methods `Result.DecodeAs[T]` and
`Result.Decode` of `loop.Result` decode the result of any run.

```go
type Forecast struct {
	City  string `json:"city"`
	TempC int    `json:"temp_c" jsonschema:"Temperature in Celsius"`
}

a := agent.MustNew("weather", agent.WithModel(m), agent.WithTools(weather),
	agent.WithOutput[Forecast]())
f, err := agent.AskAs[Forecast](ctx, a, "What's the weather in Kyoto?")
```

- **Request.** The schema is sent as the response format in strict mode,
  even when tools are present, unless `WithResponseFormat` sets one.
- **Provider subsets.** Constraints a provider rejects or ignores, such as
  `minimum` on Anthropic or `minLength` on Gemini, are moved into
  the field's description; strict tool schemas are adapted the same way.
  Claude models before 4.5, and Gemini 2.x when tools are present, get the
  schema as a system instruction instead. The answer is still checked
  against the full schema.
- **Retries.** An answer that does not match goes back to the model with the
  problems found, twice by default (`agent.OutputRetries(n)`); each attempt
  takes a turn and emits an `output_retry` event. After that the run fails
  with a `*loop.OutputError`, and resuming it grants the retries again.
- **Code fences.** An answer that is a single Markdown code block, such as
  ```` ```json ````, is checked and decoded without the fence. Any other text
  around the JSON fails the check.
- **Not checked.** Answers cut off by the token limit, withheld by a content
  filter or refused end the run as usual; decoding them returns an error.

### Multimodal content

Messages and tool results carry images, documents, audio and video. Parts
hold data, a URL or a provider file ID; checkpoints and sessions store data
as base64.

```go
msg := model.NewMessage(model.RoleUser,
	model.TextPart("Compare the report with the chart."),
	model.DocumentPart("application/pdf", pdf, "report.pdf"),
	model.ImagePart("image/png", chart))

shot := tool.Func("screenshot", "Capture the screen.",
	func(ctx context.Context, _ struct{}) (*tool.Result, error) {
		return tool.Media("Main display", model.ImagePart("image/png", capture())), nil
	})
```

- **Providers.** Which content each provider accepts and returns is listed
  in the [agentx-contrib README](https://github.com/nostosx/agentx-contrib#multimodal-content).
- **Errors.** Content a provider or model cannot take fails before the
  request with a permanent error, such as audio on Anthropic or a document
  URL on Chat Completions.
- **Options.** `FileID` names a Files API upload (OpenAI, Anthropic);
  Gemini files go in `URL` by their URI. `Detail` is OpenAI's `detail` and
  Gemini 3's per-part `mediaResolution`. `Citations` enables citations on
  Anthropic and Bedrock documents; Anthropic text keeps its citations in
  `Part.Raw` and sends them back.
- **Context.** Token estimates count media; `ClearToolResults` clears a
  result's media with its text.

### Usage limits

`agent.WithLimits` bounds each run; `Input.Limits` and `Input.MaxTurns`
override the limits and the turn limit for one run, as do
`agent.RunLimits` and `agent.RunMaxTurns` passed to `Runner.Run` or
`Runner.RunText`. Zero fields are unlimited.

```go
agent.WithLimits(loop.Limits{Requests: 20, ToolCalls: 50, TotalTokens: 200_000}),
agent.WithBudget(func(u model.Usage) error {
	if cost(u) > 0.50 {
		return errors.New("over $0.50")
	}
	return nil
}),
```

- **Checks.** `Requests` is checked before each model call, `ToolCalls`
  before each batch of tool calls, and token limits and the budget before
  both. A batch that would exceed `ToolCalls` does not start.
- **Counting.** `Usage.Requests` and `Usage.ToolCalls` count model calls and
  executed tool calls. A sub-agent's usage counts toward the parent's
  limits once the sub-agent returns.
- **Ending.** The run ends with status `limit_exceeded`, and `Result.Reason`
  names the limit. Calls that did not run are answered as such, and a final
  answer that crossed a token limit is kept. Like `max_turns`, the status is
  final: continue the conversation with a new run.

### Stop conditions

`agent.WithStopAtTools("submit")` ends the run as completed once the
`submit` tool returns a result that is not an error. The result is the run's
`Output`, and the model is not called again. `agent.WithStopWhen` takes
conditions that see each `loop.Step` after its tool calls ran, such as
`func(s *loop.Step) bool { return s.Turn >= 5 }`; the output is then the
text of the model's last message. If the consumer stops reading after the
tool results, resuming the run ends it the same way without calling the
model.

### Hooks

Hooks follow Claude Code's lifecycle events:

| Event | Purpose |
| --- | --- |
| `SessionStart` | Add context when a session starts: `startup`, `resume`, `clear`, `fork` (via `Runner`) or after `compact`ion |
| `SessionEnd` | Clean up when `Runner.EndSession` ends a session; cannot block |
| `UserPromptSubmit` | Check user input before it reaches the model, or attach context to it |
| `PreModel` / `PostModel` | Modify a model request, or inspect the response |
| `PreToolUse` | Allow, deny, ask or defer before a tool call, or rewrite its arguments |
| `PostToolUse` | Replace the result of a successful tool call or append to it |
| `PostToolUseFailure` | Append to the result of a tool call that failed |
| `Stop` / `SubagentStop` | Block a stop so the agent or sub-agent keeps working (see [Multi-agent](#multi-agent)) |
| `SubagentStart` | Add context to a sub-agent's task before it starts |
| `PreCompact` / `PostCompact` | Skip automatic compaction, or see its summary |
| `Notification` | Be notified when a run is interrupted for tool approval |

Matchers follow Claude Code's rules:

- An empty matcher or `*` matches everything.
- A matcher of only letters, digits, `_`, `-`, spaces, `,` and `|` lists
  exact names, such as `Edit|Write` or `Edit, Write`.
- Any other matcher is a Go regular expression that may match anywhere:
  `^Edit$` matches only `Edit`, and `mcp__github__.*` every tool of the
  `github` MCP server.

Tool events match the tool name. Claude Code's built-in names also match
the agentx tools with the same role: `Bash`, `Read`, `Write`, `Edit` and
`MultiEdit`, `Glob`, `Grep`, `WebFetch` and `TodoWrite` match `bash`,
`read_file`, `write_file`, `edit_file`, `glob`, `grep`, `web_fetch` and
`todo_write`, so `Edit|Write` matches `edit_file` and `write_file`. `Edit`
and `Write` also match `apply_patch`; `BashOutput` and `TaskOutput` match
`bash_output`, and `KillShell` and `TaskStop` match `kill_shell`. Other
events match `Event.Reason`: the source for `SessionStart`, the reason
(`clear`, `logout`, `prompt_input_exit`, `other`, ...) for `SessionEnd`, the
sub-agent's name for `SubagentStart` and `SubagentStop`, `auto` for
`PreCompact` and `PostCompact`, and `approval` or `permission_prompt` for
`Notification`.

Matching hooks run in parallel, as in Claude Code (at most 12 `Prompt` and
`Agent` hooks at once; `hook.Parallelism(n)` bounds all of them), and
identical handlers run once. Results merge in registration order: the most
restrictive decision wins (deny, then defer, then ask, then allow), contexts
accumulate, and the last `UpdatedInput` wins. `PreModel` hooks run one at a
time because they modify the request. `Stop` and `SubagentStop` hooks can
keep the agent working 8 times in a row (`hook.StopBlockCap(n)`, 0 for no
limit); a turn with tool calls resets the count. Calls a hook or approval
denied fire neither `PostToolUse` nor `PostToolUseFailure`.

A `Runner` fires `SessionStart` once per session: `startup` for a new
session, `resume` the first time it runs or resumes an existing one, and
`clear` or `fork` for sessions made by `Runner.Clear` and `Runner.Fork`.
`Runner.EndSession(ctx, id, reason)` fires `SessionEnd` within a 1.5-second
budget (`hook.SessionEndTimeout`). After automatic compaction,
`SessionStart` fires with `compact` and its context follows the summary.
`WithHooks` copies the manager you pass, so hooks added to it later do not
reach the agent.

```go
agent.WithHook(hook.PreToolUse, "bash", hook.Func(
	func(ctx context.Context, ev *hook.Event) (*hook.Result, error) {
		if strings.Contains(string(ev.ToolCall.Arguments), "rm -rf") {
			return &hook.Result{Decision: hook.Deny, Reason: "destructive command"}, nil
		}
		return nil, nil
	}))
```

`hook.Command` runs a command as a hook, the way Claude Code command hooks
work:

- The event is written to the command's stdin as JSON with Claude Code's
  fields, such as `cwd`, `tool_input`, `source`, `trigger` and
  `last_assistant_message`. `tool_name` and `tool_input` are agentx's, and
  `tool_response` is `{"content": ..., "is_error": ...}`.
- `CLAUDE_PROJECT_DIR` is set to the hook's `Dir`, or else the working
  directory. With `Args`, the command runs without a shell and
  `${CLAUDE_PROJECT_DIR}` is replaced in it and in `Args`.
- Exit code 2 denies the action, with stderr as the reason.
- JSON on stdout can return a decision, extra context, rewritten arguments
  or `updatedToolOutput`. `systemMessage` is logged as a warning, and
  `defer` asks for approval like `ask`.
- The timeout defaults to 10 minutes, 30 seconds for `UserPromptSubmit`
  and 1.5 seconds for `SessionEnd`.
- Background processes the command leaves behind are killed once it exits
  on Unix. On Windows they keep running unless the hook times out or is
  cancelled.

The other Claude Code handler types are hooks too:

- `hook.HTTP` POSTs the same JSON to a URL. `$VAR` in `Headers` expands
  only for `AllowedEnvVars`. A 2xx JSON body is read like command output;
  other bodies, non-2xx statuses and failures are non-blocking errors.
- `hook.Prompt` asks a `model.Model` with `$ARGUMENTS` replaced by the event
  JSON; `{"ok": false, "reason": ...}` blocks as Claude Code does per event
  (`ContinueOnBlock`, `impossible`). Default timeout: 30 seconds.
- `hook.Agent` hands the prompt to a `hook.Verifier`, such as
  `agent.HookVerifier(m, agent.WithTools(builtin.ReadOnlyTools(cfg)...))`,
  which works with tools for up to 50 turns. Default timeout: 60 seconds.
- `hook.MCPTool` calls an MCP tool through a `hook.MCPCaller`, such as
  `mcp.HookCaller(clients)`, with `${tool_input.file_path}`-style input.
- `hook.Async` runs a hook in the background. Its `additionalContext` and
  `systemMessage` reach the model with the next tool result or run input;
  with `Rewake`, exit code 2 also calls `hook.OnAsyncResult` so you can wake
  the session. `Manager.Close` cancels them.

`hook.LoadSettings` reads the `hooks` block of Claude Code settings files,
so existing hook configurations work unchanged:

```go
hooks, warnings, err := hook.LoadSettings(
	filepath.Join(home, ".claude", "settings.json"),
	".claude/settings.json",
	".claude/settings.local.json",
)
if err != nil {
	return err
}
for _, w := range warnings {
	log.Print(w) // handlers agentx skipped
}
a := agent.MustNew("assistant", agent.WithModel(m), agent.WithHooks(hooks))
```

Files that do not exist are skipped, a handler repeated across files runs
once, `disableAllHooks` is honored, `onFailure: "block"` makes a failing
hook deny the action, and `async`/`asyncRewake` commands run in the
background. `command` and `http` handlers always run; `prompt`, `agent` and
`mcp_tool` handlers need `hook.SettingsOptions{Model, Verifier, MCP}.Load(...)`.
`CLAUDE_CODE_STOP_HOOK_BLOCK_CAP` and
`CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS` are honored. Handlers without
their dependency, types an event does not support in Claude Code, `if` and
PowerShell hooks, and events agentx does not fire are skipped with a
warning.

### Tool approval and resumable runs

You can mark a tool with `tool.RequireApproval()`, or a hook can return
`hook.Ask`. A tool implementing `tool.CallApprover` decides per call from
the arguments, as sandboxed `bash` does. Either way the call needs approval
before it runs:

- **With an Approver.** If the agent has an `Approver`, it decides right
  away. This suits interactive terminals.
- **Without one.** The run stops with status `interrupted` and saves its
  state through the `Checkpointer`. Resume it after you have decisions. The
  example keeps checkpoints and sessions in files with the `filestore`
  module of agentx-contrib; `loop.NewMemoryCheckpointer` and
  `session.NewMemoryStore` keep them in memory:

```go
cp, _ := filestore.Checkpoints(".agentx/checkpoints")
store, _ := filestore.Sessions(".agentx/sessions")
a := agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy),
	agent.WithCheckpointer(cp))
runner := agent.NewRunner(a, store)

res, _ := runner.RunText(ctx, "s1", "Deploy the latest build")
if res.Status == loop.StatusInterrupted {
	approvals := map[string]loop.Approval{}
	for _, req := range res.Interrupt.Requests {
		approvals[req.Call.ID] = loop.Approve // or loop.Deny
	}
	for ev, err := range runner.Resume(ctx, "s1", approvals) {
		// ...
	}
}
```

Resuming also covers runs that failed, for example on a provider outage,
and runs whose process stopped mid-way: `Runtime.Resume` and
`LLMAgent.Resume` accept every status for which `Status.Resumable` is true.
`runner.Resume` resumes the session's interrupted run, or else its last
failed run (see `agent.FailedRunKey`). A run whose process crashed is
resumed by run ID with the agent's `Resume`, which needs a `Checkpointer`.
Without a `Checkpointer`, the runner resumes interrupted runs from state it
keeps in memory, which does not survive a restart; failed runs then cannot
be resumed.

- **Stopping early.** If you stop iterating a run's events, the run stops
  before its next step. With a `Checkpointer` it can be resumed from where
  it stopped; if it stopped right after the model's final answer, resuming
  handles that answer instead of calling the model again. `Result.Reason`,
  as `AfterRun` middleware sees it, says whether the run can be resumed.
  Stopping at the first event runs nothing and leaves a resumed run's
  checkpoint as it was.
- **Approvals on resume.** Approvals passed to `Resume` answer only the
  requests of the interrupt being resumed. Resuming a run that was not
  interrupted ignores them, and requests raised later go to the `Approver`
  or interrupt the run again.
- **Tool calls are not repeated.** The state is checkpointed before each
  batch of tool calls, together with the results that are already in.
  Finished calls keep their results. A read-only call that was cut off runs
  again. A call with side effects that was cut off is reported to the model
  as possibly done instead of running twice.
- **Discarding.** `runner.Discard(ctx, "s1")` gives up the session's
  interrupted or failed run, for example when its checkpoint is gone. It
  answers the pending calls as not run, so the session can continue.
- **Status.** A final answer cut off by the token limit or withheld by a
  content filter ends with status `incomplete`. `Result.FinishReason` tells
  the cases apart and also reports refusals.
- **Changed arguments.** The model's tool calls stay as it sent them. When
  a hook or an approval changes the arguments, the tool result tells the
  model which arguments actually ran.

### Permissions

The `permission` package decides tool calls with Claude Code's
[permission rules](https://code.claude.com/docs/en/permissions) and
[modes](https://code.claude.com/docs/en/permission-modes). Load the
`permissions` block of Claude Code settings files, or write the rules
yourself:

```go
s, warnings, _ := permission.LoadSettings(
	filepath.Join(home, ".claude/settings.json"), ".claude/settings.json")
cfg := s.Config(dir) // allow, ask, deny, defaultMode, additionalDirectories
cfg.Rules = append(cfg.Rules, permission.Rules{
	Allow: []string{"Bash(npm run *)", "Edit(src/**)", "WebFetch(domain:go.dev)"},
	Deny:  []string{"Read(./.env)", "Bash(git push *)", "mcp__github__*"},
})
p, _ := permission.New(cfg)
a := agent.MustNew("dev", agent.WithModel(m),
	agent.WithTools(builtin.Tools(builtin.Config{Dir: dir, CheckRedirect: p.CheckRedirect})...),
	agent.WithPermissions(p))
```

- **Rules.** Deny wins over ask, ask over allow. `Bash`, `Read`, `Edit` and
  `WebFetch` select the built-in tools of that role; Edit rules check every
  file an `apply_patch` call touches. Bash rules match each
  command of a compound command, including those in subshells and `$(...)`.
  Read and Edit rules are gitignore patterns anchored with `//`, `~/`, `/`
  or `./`. Deny and ask rules of the form `Tool(param:value)` match another
  parameter, such as `Bash(dangerously_disable_sandbox:true)`, which asks
  before every command that leaves the sandbox.
- **Recursive searches.** With `WithPermissions`, glob and grep omit
  discovered files denied by path rules or requiring approval, even with
  `include_ignored`. Name a file explicitly to request its approval.
  Path carve-outs still apply to files beneath restricted directories.
- **Modes.** `default` leaves undecided calls to the tool, but runs
  read-only commands such as `ls` and `git status` whose paths are inside
  the working directories; `cat ~/.ssh/id_rsa` asks. `acceptEdits` also allows
  edits inside the working directories, `plan` denies everything that is not
  read-only, `dontAsk` denies whatever would ask, and `bypassPermissions`
  allows all but deny and ask rules. `agent.RunValue(permission.ModeKey,
  "plan")` sets one run's mode; middleware can call `p.SetMode`. Hooks of
  the same agent receive the mode as `permission_mode`.
- **Plan mode and agents.** Like Claude Code, which runs `BashOutput`,
  `KillShell` and `TodoWrite` without permission, `plan` allows
  `bash_output`, `kill_shell` and `todo_write`, which change only the
  agent's own state. Handoff tools are read-only, so a planning agent can
  hand off; the mode is kept in the run value `permission.mode`, which the
  handoff carries, so the agent handed off to plans too if it has a policy.
  Sub-agent tools are not read-only and stay denied.
- **Always allow.** Approve with `loop.Approval{Approved: true, Remember:
  loop.RememberSession}` and later calls like it run without approval for
  the session (`RememberRun`: for the run). The Runner keeps these grants
  with the session.
- **Limits.** The shell lexer is not a shell parser: commands it cannot
  split are never auto-approved, and rules match command text, not a
  program run by path or through `sh -c`. Use a sandbox where that matters.

### Sessions

`agent.Runner` stores the conversation in a `session.Store` and runs each
session's runs one at a time, within that Runner. Three stores are
available:

- `session.NewMemoryStore()` keeps sessions in memory.
- `filestore.Sessions(dir)` of agentx-contrib writes each session to a
  JSON file atomically.
- `sqlstore.Sessions(db)` of agentx-contrib keeps them in a SQL database;
  see [File and SQL storage](#file-and-sql-storage).

All detect concurrent modification: if another Runner or process changed
the session, `Save` returns `session.ErrConflict`. Reload before deciding how
to recover. A failed save does not roll back model requests or tool side
effects; do not replay the entire run automatically.

Use separate built-in tool instances for separate users or sessions: their
working directory, file tracker and todo list are mutable state. Close tools
with `builtin.Close` when their owner stops. Custom models, tools, hooks and
middleware shared across runs must support concurrent calls and honor context
cancellation.

Generated identifiers use the standard `uuid` package. Session IDs from
`session.NewID`, which the Runner and server use for sessions without an
ID, are random version 4 UUIDs. Run, workflow run, A2A task and message,
memory entry and rewind IDs are version 7 UUIDs behind a short prefix such
as `run_`, so they sort by creation time.

The session is saved when a run ends, even if the caller's context was
cancelled. If the caller stops iterating early, the run is cancelled and
the session is saved once it ends. Unfinished tool calls are answered, so
the conversation stays valid.

Tools, hooks, middleware and instruction functions read and write the
session's state during a Runner's run with `agent.SessionState(ctx)`. The
state is saved with the session when the run ends, including when it fails
or is interrupted, and is shared with sub-agents. Store JSON values and
read them back typed with `session.State.Value[T]`; keys starting with `agentx.`
belong to the Runner. Outside a Runner, `agent.ContextWithSessionState`
supplies a state. Middleware keeps typed per-run state with
`loop.Run.EncodeValue` and `loop.Run.DecodeValue[T]`, which store JSON in
run values and so survive checkpoints.

### Instructions and dependencies

- **Dynamic instructions.** `WithInstructionsFunc(f)` appends the text `f`
  returns to the static instructions. It runs once per run, so the system
  prompt stays cacheable and a resumed run reuses it; `agent.EveryTurn()`
  runs it before each model call instead. An error fails the run.
- **Templates.** `WithInstructionTemplate()` fills `{key}` in the static
  instructions from the session state when a run starts, like Google ADK:
  strings as they are, other values as JSON. A missing `{key}` fails the
  run and a missing `{key?}` becomes empty. Only names (a letter or `_`,
  then letters, digits, `_ . : -`) are placeholders, so JSON stays as it
  is; `{{` and `}}` are literal braces. Function output is not templated;
  call `agent.RenderTemplate` there.
- **Dependencies.** Typed values, such as a database handle or the current
  user, travel in the context: `loop.WithDeps(ctx, v)`,
  `agent.WithDeps(v)` as a default and `agent.RunDeps(v)` per Runner run.
  Tools, hooks, middleware and instruction functions read them with
  `loop.Deps[T](ctx)`. They are never sent to the model or saved in
  checkpoints: pass them again when resuming, for example
  `runner.Resume(ctx, id, approvals, agent.RunDeps(db))`.

```go
a := agent.MustNew("assistant",
	agent.WithModel(m),
	agent.WithInstructions("Answer in {lang?}."),
	agent.WithInstructionTemplate(),
	agent.WithInstructionsFunc(func(ctx context.Context, run *loop.Run) (string, error) {
		u, _ := loop.Deps[*User](ctx)
		return "The user is " + u.Name + ".", nil
	}),
)
res, err := runner.RunText(ctx, sessionID, "Hi", agent.RunDeps(currentUser))
```

### Checkpoints and rewind

Like Claude Code's
[checkpointing](https://code.claude.com/docs/en/checkpointing), a `Runner`
records a checkpoint before the user message of each run.
`Runner.Checkpoints` lists them, and `Runner.Rewind` returns a session to
one: `agent.RewindCode` restores the files, `RewindConversation` removes
the message and everything after it and returns the message to send
again, and `RewindBoth` does both. The returned message is the caller's
original input, before context providers and middleware append reminders.
A checkpoint's ID is its run's ID, which the run's events carry.

```go
hist, err := builtin.NewFileHistory(builtin.FileHistoryConfig{Dir: ".agentx/history"})
tools := builtin.Tools(builtin.Config{Dir: ".", History: hist})
a := agent.MustNew("coder", agent.WithModel(m), agent.WithTools(tools...),
	agent.WithFileHistory(hist))
runner := agent.NewRunner(a, store)

cps, err := runner.Checkpoints(ctx, id)
res, err := runner.Rewind(ctx, id, cps[len(cps)-1].ID, agent.RewindBoth)
// res.Message is the removed prompt; res.Restored lists the restored files.
```

- **What is tracked.** Before changing a file, `write_file`, `edit_file`
  and `apply_patch` save its state: content by SHA-256, mode and symbolic
  link target, for creations, deletions and moves too. If the save fails,
  the tool changes nothing. Edits by sub-agents count for the calling
  session. As in Claude Code, changes made by `bash`, by other tools or
  outside the agent are not tracked.
- **Restore.** Rewinding the code returns each file changed since the
  checkpoint to its state before the first change after it, through
  `os.Root` and all or nothing, like `apply_patch`. A deleted symbolic
  link is not created again, and files larger than `MaxFileSize` are
  reported in `Skipped`. The model must read restored files again before
  changing them. The files' state before the rewind is kept as the
  checkpoint `RewindResult.Undo`, which `History.Restore` restores.
- **Rules.** `Rewind` returns `ErrSessionBusy` while the session runs and
  refuses a session with an interrupted run until it is resumed or
  discarded; a failed run is given up. After compaction removed a
  checkpoint's message, only the code can be rewound to it. New checkpoints
  identify messages independently of their content, so repeated prompts
  cannot match an older compacted checkpoint. Legacy records without stable
  message identity cannot rewind the conversation; code rewind is unaffected.
- **Session state.** Rewinding the conversation makes the agent that was
  active at the checkpoint (`Checkpoint.Agent`) active again, undoing later
  handoffs. Other session state, such as `agent.SessionState` values and
  remembered permission grants, is not rewound, as Claude Code keeps
  permissions across a rewind.
- **Storage and retention.** `FileHistory` stores each content once under
  `objects/` and each session's checkpoints in `sessions/<id>.json`, with
  mode 0600. As Claude Code does by default, it keeps a session's 100 most
  recent checkpoints and removes its history 30 days after the last
  change; it also keeps at most 1 GiB per session and files up to 16 MiB.
  `FileHistoryConfig` changes the limits.
- **Forks.** `Runner.Fork` copies the checkpoints and the history, so
  either session can rewind to checkpoints before the fork, and later
  changes are recorded only for the session that made them. Both sessions
  still work on the same files.

### Context engineering

`actx` builds the system prompt and a reminder for each new input from
providers, which run once per run, before it starts. It also tracks the
context window and compacts the conversation once it passes a threshold.

```go
import "github.com/nostosx/agentx/actx"

agent.WithContext(actx.Config{
	Window: actx.Window{Limit: 200_000, ReserveOutput: 16_000},
	Strategy: actx.Chain(
		actx.ClearToolResults{KeepRecent: 3},   // clear old tool results first
		actx.ClearToolArguments{KeepRecent: 3}, // then large old tool arguments
		actx.Summarize{Model: m},               // then summarize older turns
		actx.Truncate{},                        // finally drop the oldest turns
	),
}),
agent.WithContextProvider(actx.Static("rules", "Always answer in English.", actx.PlaceSystem)),
```

Sections are placed in one of two ways:

- **`PlaceSystem`** appends stable content to the system prompt, which keeps
  the prompt prefix cacheable.
- **`PlaceReminder`** puts content that changes per request into a
  `<system-reminder>` block on the current user input.

`actx.InstructionFiles` loads project instruction files the way Codex and
Claude Code do (`agent.WithInstructionFiles(dir)` uses its defaults):

```go
agent.WithContextProvider(actx.InstructionFiles(actx.InstructionConfig{
	Dir:       ".",                             // default
	UserFiles: []string{"~/.codex/AGENTS.md"}, // optional personal files, read first
}))
```

- The project root is the nearest ancestor of `Dir` containing a
  `RootMarkers` entry (default `.git`). From the root down to `Dir`, each
  directory contributes the first file of `Names` that exists (default
  `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md`), then any `LocalNames`
  (none by default; for example `CLAUDE.local.md`). Without a root, only
  `Dir` is searched.
- Each file becomes a `PlaceSystem` section labeled with its path; the files
  are read again on each run.
- `@path` imports outside code spans and fenced code blocks are expanded
  after the importing file, relative to it, up to `ImportDepth` hops
  (default 4) and once per file.
- The files are untrusted: they and their imports are read only inside the
  project root (a user file's imports only inside its directory; both may
  also read inside `ImportDirs`), and paths that leave these directories
  through `..` or symbolic links are skipped.
- `MaxBytes` bounds the total (default 32 KiB); the rest is dropped with a
  note.

Without a `Strategy`, compaction uses `ClearToolResults`, then
`ClearToolArguments`, then `Truncate`. It keeps the task a run is working
on:

- Reminders added mid-run, such as a Stop hook's continuation, stay in the
  turn they continue.
- The turn in progress keeps its signed reasoning, so Anthropic extended
  thinking stays valid.
- Tool results the model has not seen yet are never cleared, so a single
  oversized batch can exceed the window; the middleware then logs a
  warning. Bound tool output, for example with `tool.TruncateOutput`.

Editing earlier history invalidates Anthropic thinking replayed after it on
models with the thinking prefix check (Claude Fable 5.1, Opus 5.5, Sonnet
5.5 and Haiku 5.5). The Anthropic provider of agentx-contrib asks the API
to drop those blocks, so requests succeed and the model thinks again.

### Skills

Skills use the same `SKILL.md` format as Claude Code: YAML front matter with
a `description` and an optional `name`, which defaults to the directory
name, followed by a Markdown body. The model sees only the name and
description at first and loads the full text through a tool when it needs
it. This is progressive disclosure: the full text enters the context only
when the model uses it.

- **Front matter.** All fields of Claude Code's
  [frontmatter reference](https://code.claude.com/docs/en/skills#frontmatter-reference)
  are typed in `skill.Metadata`, including the Agent Skills fields
  `license`, `compatibility` and `metadata`; unknown keys stay in `Extra`.
  The YAML subset covers nested block and flow maps and lists, quoted
  scalars that span lines, block scalars and comments.
- **Names.** `Validate` follows Claude Code, which also accepts names such
  as `Release Notes`; the `skill.Strict()` load option enforces the
  [Agent Skills specification](https://agentskills.io/specification).
- **Discovery.** `skill.Discover` loads `.claude/skills` and
  `.agents/skills` from the working directory up to the project root.
  Managed, home (opt-in), additional and nested directories are optional.
  Higher precedence wins: managed, user, project, then additional;
  conflicts are reported.
- **Invocation.** The `skill` tool takes `arguments` and expands
  `$ARGUMENTS`, `$N`, `$name` (from `arguments`), `${CLAUDE_SKILL_DIR}`,
  `${CLAUDE_SESSION_ID}` and the other substitutions. `` !`command` ``
  injection runs only with `skill.Config{Shell: true}`, because it executes
  shell commands written in skill files; otherwise the text stays as is.
- **Behavior.** `disable-model-invocation` skills are hidden from the model;
  users invoke them with `agent.RunSkill` or `Registry.Invoke`. Until the
  run ends, `allowed-tools` skip approval, `disallowed-tools` are removed,
  and `model` and `effort` apply. `context: fork` runs the skill in a
  sub-agent: `general-purpose` by default, or an agent passed to
  `WithSkillAgents`. Skill `hooks` last for the session, and skills with
  `paths` appear once a matching file is read or edited.
- **Base directory.** Skills from disk show the model their directory, so
  it can find bundled files. Embedded skills have none.

```go
//go:embed skills
var skillFS embed.FS

skills, err := skill.LoadFS(skillFS, "skills", "")
if err != nil {
	log.Fatal(err)
}
found, err := skill.Discover(skill.DiscoverConfig{}) // project skills
if err != nil {
	log.Print(err) // skills that failed to load; the others are in found
}
a := agent.MustNew("dev", agent.WithModel(m),
	agent.WithSkills(append(skills, found.Skills...)...))
res, err := agent.NewRunner(a, nil).RunText(ctx, "", "ship it", agent.RunSkill("deploy", "staging"))
```

### Long-term memory

`memory` stores facts across sessions. Retrieval is keyword-based by
default. With an `Embedder` configured, `memory.Hybrid` combines keyword and
vector results. If one retriever fails, it uses the others and logs the
error with `slog.Default` unless you set `OnError`. `memory.NewMemoryStore`
keeps entries in memory; `filestore.Memory` and `sqlstore.Memory` of
agentx-contrib persist them.

```go
store, _ := filestore.Memory(".agentx/memory.json")
mem := memory.New(store)
// Recall 3 memories per input, scoped to user-42.
a := agent.MustNew("assistant", agent.WithModel(m), agent.WithMemory(mem, "user-42", 3))
```

By default the model also gets tools to remember, recall and forget facts
(`memory_remember`, `memory_recall` and `memory_forget`).
`agent.WithoutMemoryTools()` hides them.

`WithMemory` gives every run of the agent the same scope. An agent that
serves several users, for example behind `server.New`, must choose the
scope per run instead: `agent.WithMemoryScopeFunc(mem, scope, 3)` calls
`scope(ctx)` for each recall and memory tool call, with the context the
run was started with, such as the request context where an HTTP
middleware stored the authenticated user. When it returns an error, the
call fails, so a run whose user is unknown reads and writes no memories.

The providers of agentx-contrib ship embedders: `openai.NewEmbedder`
(`/embeddings`, including compatible services such as `openai.Ollama()`)
and `gemini.NewEmbedder` (`batchEmbedContents`). `retry.Embedder` of
agentx-contrib retries them like model calls. Gemini can embed queries and
stored entries for different tasks:

```go
docs, _ := gemini.NewEmbedder(gemini.WithTaskType(gemini.TaskRetrievalDocument))
queries, _ := gemini.NewEmbedder(gemini.WithTaskType(gemini.TaskRetrievalQuery))
policy := retry.Policy{}
mem := memory.New(store,
	memory.WithEmbedder(retry.Embedder(policy, docs)),
	memory.WithQueryEmbedder(retry.Embedder(policy, queries)))
```

### Retrieval-augmented generation

`rag` loads documents, splits them into chunks, embeds and indexes the
chunks, and connects the index to agents as a search tool and as automatic
retrieval for each run.

```go
docs, _ := (&rag.FSLoader{FS: os.DirFS("docs"), Include: []string{"**/*.md"}, GitIgnore: true}).Load(ctx)
emb, _ := openai.NewEmbedder()
store, _ := filestore.Vectors(".agentx/rag")
ix := rag.NewIndex(rag.Config{Store: store, Embedder: emb})
// Embed new and changed sources only, and delete removed ones.
if _, err := ix.Sync(ctx, docs); err != nil {
	log.Fatal(err)
}
a := agent.MustNew("assistant", agent.WithModel(m),
	agent.WithTools(rag.Tool(ix, rag.ToolOptions{Description: "Search the product docs."})),
	agent.WithContextProvider(rag.Provider(ix, 3)))
```

- **Loaders.** `FSLoader` reads any `fs.FS` with glob filters and optional
  `.gitignore` rules; `URLLoader` fetches URLs. Parsers cover text,
  Markdown, HTML (with the `web_fetch` converter), JSON, JSON Lines and CSV.
  PDF needs a library outside the standard library: implement `rag.Parser`
  and register it in `FSLoader.Parsers`.
- **Splitters.** Sizes are measured in tokens with the estimate of the
  `actx` package, or any `LengthFunc`. `RecursiveSplitter` splits at
  separators with overlap, `MarkdownSplitter` keeps the header path in
  `MetaHeaders`, `CodeSplitter` splits Go, Python and JavaScript at
  declarations, and `AutoSplitter`, the default, picks one per document.
  Chunk IDs hash the source, document and offset, so they are stable.
- **Indexing.** `Index` embeds chunks in batches and stores them in a
  `VectorStore`: `rag.NewMemoryStore()`, or `filestore.Vectors` of
  agentx-contrib with one JSON file per source.
  A content hash per source lets `Sync` skip unchanged sources; change
  `Config.Version` after changing the splitter or embedder.
- **Retrieval.** By default, `Hybrid` fuses BM25 (`Keyword`) and cosine
  similarity (`Vector`) with reciprocal rank fusion. `Config.Reranker` adds
  a second stage such as `LLMReranker`. `ContextualEnricher` implements
  [contextual retrieval](https://www.anthropic.com/news/contextual-retrieval):
  a model writes a short, cached context that is embedded and searched with
  each chunk.
- **Agents.** `rag.Tool` is read-only and returns numbered chunks with their
  sources, IDs and scores, bounded in size; the result metadata carries the
  citations.

### File and SQL storage

The core keeps state in memory only: `session.NewMemoryStore`,
`loop.NewMemoryCheckpointer`, `workflow.NewMemoryCheckpointer`,
`memory.NewMemoryStore`, `rag.NewMemoryStore` and
`mcp.NewMemoryTokenStore`. Persistent stores are in agentx-contrib:

- `filestore` writes JSON files: `Sessions`, `Checkpoints`,
  `WorkflowCheckpoints`, `Memory`, `Vectors` and `MCPTokens`.
- `sqlstore` keeps sessions, run checkpoints, workflow checkpoints and
  memories in SQLite, PostgreSQL or MySQL through `database/sql`, with
  the driver of your choice.

```go
sessions, _ := filestore.Sessions(".agentx/sessions")
runner := agent.NewRunner(a, sessions)
```

See the [agentx-contrib README](https://github.com/nostosx/agentx-contrib#storage)
for their options, schema and tests.

Conformance suites check any store against the contract the built-in
stores follow: `session/sessiontest`, `loop/checkpointtest`,
`workflow/checkpointtest`, `memory/memorytest` and `rag/ragtest`, whose
`ragtest.Embedder` is a deterministic embedder for tests.

```go
func TestStore(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) session.Store { return newStore(t) })
}
```

### Multi-agent

Pass sub-agents to `WithSubAgents` and the parent can call them like tools.

- **Hooks.** The parent's `SubagentStart` hooks fire before a sub-agent
  starts a task; their `AdditionalContext` is added in front of the task.
  `SubagentStop` hooks fire when a sub-agent finishes. One that denies with
  a reason, or adds context, makes the sub-agent continue, under the same
  cap as `Stop`. Both match the sub-agent's name, and `Event.RunID` is the
  sub-agent's run.
- **Usage.** A sub-agent's token usage, requests and tool calls are added
  to the parent run's `Usage`, so they count toward its limits.
- **Approvals.** When a sub-agent without its own `Approver` needs
  approval, the parent's `Approver` decides. Without one, the parent run is
  interrupted with the sub-agent's
  requests; `ApprovalRequest.Agent` names the agent that asked. Resuming the
  parent resumes the sub-agent with the decisions. The sub-agent needs no
  checkpointer of its own.
- **Events.** `WithSubAgentEvents()` forwards the events of sub-agents, at
  every depth, into the parent's stream. Forwarded events keep the
  sub-agent's `RunID` and `Agent`, have `Depth` 1 for a sub-agent, 2 for
  its sub-agents, and `ParentCallID` set to the tool call that started the
  sub-agent. Their `run_end` ends the sub-agent, not the parent, so check
  `Depth == 0` when you switch on `Type`. Off by default.
- **Concurrency.** Sub-agents run one at a time by default, since they may
  have side effects. With `WithConcurrentSubAgents()` sub-agent calls in
  one model response run in parallel, bounded by `WithParallelism`.
  Sub-agents that need approval at the same time interrupt the parent
  together and resume together.
- **Depth.** Sub-agents can be nested `agent.DefaultMaxDepth` (5) levels
  deep; `WithMaxDepth` changes the bound, and the smallest bound above a
  sub-agent applies. A call beyond it returns an error to the model.

```go
coordinator := agent.MustNew("coordinator",
	agent.WithModel(m),
	agent.WithSubAgents(researcher, writer),
	agent.WithSubAgentEvents(),
	agent.WithConcurrentSubAgents(),
)

for ev, err := range coordinator.Run(ctx, agent.Text("Write about MCP")) {
	if err != nil {
		log.Fatal(err)
	}
	if ev.Depth > 0 && ev.Type == loop.EventToolCall {
		fmt.Printf("%s (via %s) -> %s\n", ev.Agent, ev.ParentCallID, ev.ToolCall.Name)
	}
}
```

`agent.AsToolWith` builds the tool for a single agent with its own name,
description and these options, for use with `WithTools`.

### Handoffs

`WithHandoffs` lets the model hand the conversation to another agent, as in
the OpenAI Agents SDK and Google ADK. Each agent becomes a
`transfer_to_<name>` tool. When the model calls one, the calls of its turn
run and the run continues on the other agent with its own instructions,
tools, guardrails and middleware.

- **One run.** The run keeps its ID, usage and turn count, so limits cover
  every agent. Events of the next agent follow a `loop.EventHandoff`
  (`Event.Handoff` names `From`, `To` and the call), and `Result.Agent`
  names the agent that answered.
- **Sessions.** A Runner saves the active agent in the session state
  (`agent.ActiveAgentKey`) and sends the next message to it. Rewinding the
  conversation restores the agent active at the checkpoint.
- **Permissions.** Handoff tools are read-only, so `plan` mode allows them;
  see [Permissions](#permissions).
- **History.** The next agent sees the whole conversation and a note about
  the handoff. A filter (`HandoffInputFilter`) rewrites what its model
  sees, not what the session saves; `agent.RemoveToolCalls` and
  `agent.NestHandoffHistory` are built in.
- **Options.** `WithHandoff(a, ...)` takes `HandoffName`,
  `HandoffDescription`, `HandoffInputFilter` and `HandoffInput[T](fn)`, a
  typed argument the model sends, checked by `fn`. `agent.Ref(name, desc)`
  refers to an agent built later, such as the one handing off, so agents
  can hand back. `WithMaxHandoffs` bounds handoffs per run (10 by default),
  and only the first handoff call of a turn counts.
- **Approvals.** An agent that was handed the run can be interrupted like
  any other. `Resume` continues on that agent, located through the
  checkpointers of the agent graph. Agents that hand off to each other
  should share a checkpointer.
- **Hooks.** No hook event is specific to handoffs. `PreToolUse` and
  `PostToolUse` with the matcher `transfer_to_.*` observe and can deny
  them.

```go
billing := agent.MustNew("billing", agent.WithModel(m),
	agent.WithDescription("Invoices and refunds."),
	agent.WithHandoffs(agent.Ref("triage", "Anything else.")))
triage := agent.MustNew("triage", agent.WithModel(m),
	agent.WithHandoff(billing, agent.HandoffInputFilter(agent.RemoveToolCalls)))
res, err := agent.NewRunner(triage, nil).RunText(ctx, id, "My invoice is wrong")
fmt.Println(res.Agent, res.Output) // billing ...
```

### Guardrails

Package `guard` checks the input, the final answer and tool calls. A check
returns `guard.Result{Tripwire, Info, Message}`. A tripped guardrail ends
the run with `StatusBlocked` and `Result.Guardrail`, and the result's
`Output` is empty. An error fails the run.

- **Input.** `WithInputGuardrail(g)` runs before the run starts, before
  hooks. With `guard.Parallel()` it runs alongside the first model call
  instead: a trip cancels the call, the first response waits for all
  parallel guardrails, so no tool runs first, and a completed call's usage
  is still counted. A resumed run that had not passed them runs them again.
  Input guardrails do not run for the agent a run is handed off to.
- **Output.** `WithOutputGuardrail(g)` checks the answer after `Stop` hooks
  let the run finish. A blocked answer is withheld: the result, the
  checkpoint and the session keep `[response withheld by guardrail <name>]`
  in its place, which keeps the conversation valid for the next run. The
  answer streams as the model writes it, before the check, unless
  `WithOutputGuardrail(g, guard.HoldOutput())` holds each response's
  deltas and message until it calls tools (then they stream as usual) or
  the guardrails pass; a held answer arrives at once when its response
  ends. A held answer that does not end the run is withheld from output:
  one a Stop hook continues, one the provider paused, one that fails the
  output schema or one of a run that stops or fails first. Its text is
  absent from `Result.Output`, `OutputError.Output` and `output_retry`;
  schema diagnostics are generic because details may quote unchecked text.
  Drafts remain in the conversation, including `Result.Messages`,
  `Result.NewMessages` and checkpoints, for continuation and recovery.
  Treat those conversation fields as internal state when displaying output.
- **Tools.** `WithToolGuardrail(g)` checks calls before approval (`Before`)
  and results after they ran (`After`), before tracing, hooks and events
  see them. A tripped `After` replaces the result, its media and metadata
  included, so the withheld content is neither sent nor kept in the
  conversation, and `PostToolUse` hooks do not run for it.

```go
agent.WithInputGuardrail(guard.InputGuardrail{
	Name: "topic",
	Check: func(ctx context.Context, run *loop.Run, in *model.Message) (guard.Result, error) {
		return guard.Result{Tripwire: isHomework(in.Text()), Message: "Not supported."}, nil
	},
}, guard.Parallel())
```

### Workflows

`workflow` runs agents and functions as a graph. It supports:

- parallel branches
- joins that wait for every incoming branch
- conditional routes, and loops back through them
- dynamic fan-out over a list with `Map`
- per-key reducers for merging state
- retries with exponential backoff, and fallback nodes
- persistence, crash recovery and human-in-the-loop suspensions
- streaming events and Mermaid rendering
- `{{key}}` templates filled from the shared state

```go
g := workflow.NewGraph().
	AddNode("draft", workflow.AgentNode(writer, workflow.Template("Write about {{topic}}"), "draft")).
	AddNode("review", workflow.AgentNode(reviewer, workflow.Template("{{draft}}"), "final"),
		workflow.Retry(2, time.Second)).
	AddChain(workflow.Start, "draft", "review", workflow.End)

wf, err := g.Compile()
if err != nil {
	log.Fatal(err)
}
st, err := wf.Run(ctx, workflow.NewState(map[string]any{"topic": "Go iterators"}))
if err != nil {
	log.Fatal(err)
}
fmt.Println(st.String("final"))
```

**State merging.** Each node works on its own view of the state. Its
writes are committed together when it succeeds, and dropped when it fails
or suspends. Two nodes that the graph does not order must not set the same
key unless the key has a reducer; otherwise the run fails with a
`ConflictError`. `State.Update` changes are replayed and never conflict.

**Persistence and resuming.** With `WithCheckpointer`, the run is saved
when a node attempt starts and after each node, as a `Checkpoint`. Use
`NewMemoryCheckpointer`, or `filestore.WorkflowCheckpoints` or
`sqlstore.WorkflowCheckpoints` of agentx-contrib. State values must then be
encodable as JSON; restored values are plain JSON values, which
`State.Value[T]` reads back typed. `Resume` continues a run that crashed, failed or
suspended:

- Finished nodes do not run again.
- Nodes that were running run again with the same attempt number. Node
  code therefore runs at least once, but state writes take effect exactly
  once.

Checkpoint save or cleanup failures fail the run and are returned by
`Run`/`Resume` and the stream, including failures during suspension or
completion. An existing execution error is preserved alongside the storage
error; a failed suspension save is not reported as a resumable suspension.

**Human in the loop.** A node calls `Suspend` and returns its error. The
run then ends with a `*Suspension` that lists the `Pending` keys. Resume
the run with a value for each key. The node runs again from its start, and
this time `Suspend` returns the value.

An `AgentNode` whose agent needs tool approvals suspends the same way. Its
`Pending.Interrupt` holds the requests. Resume it with
`map[string]loop.Approval`. Each agent run gets a predictable ID,
`SubRunID(run, node, attempt, item)`. After a crash, an agent with a
`Checkpointer` therefore resumes its own run, and finished tool calls are
not repeated.

```go
wf := g.MustCompile(
	workflow.WithCheckpointer(cp),
	workflow.Reducer("summaries", workflow.Append()),
)
_, err := wf.Run(ctx, st, workflow.WithRunID("job-42"))
if s, ok := errors.AsType[*workflow.Suspension](err); ok {
	approvals := map[string]loop.Approval{}
	for _, req := range s.Pending[0].Interrupt.Requests {
		approvals[req.Call.ID] = loop.Approve
	}
	st, err = wf.Resume(ctx, "job-42", map[string]any{s.Pending[0].Key: approvals})
}
```

**Fan-out, loops and retries.**

- `Map("docs", body, MapConcurrency(4))` runs `body` once per list
  element. Each element is visible as `item`. Writes are merged in element
  order, and finished elements are saved one by one.
- A branch may route back to an earlier node. The loop then restarts from
  that node, and `MaxSteps` bounds the run with `ErrStepLimit`.
- `WithRetry(RetryPolicy{RetryIf: workflow.Transient})` adds exponential
  backoff with jitter. `OnError("fallback")` routes a failed node to a
  fallback node.

**Events.** `Stream` yields node, state-delta, suspension, agent and
`Emit` events. `Mermaid()` renders the graph.

### MCP

The `mcp` package is a client and a server for the
[Model Context Protocol](https://modelcontextprotocol.io) (revision
2026-07-28, falling back to 2025-11-25, 2025-06-18, 2025-03-26 and
2024-11-05). The client:

- **Version negotiation:** `Connect` probes with `server/discover`. A modern
  server is used statelessly, with the protocol version in each request's
  `_meta`. Any other error, an HTTP 4xx without a modern error, or on stdio
  no answer within 10 seconds (`mcp.WithProbeTimeout`) means an older
  server: `Connect` then sends `initialize` offering 2025-11-25 and accepts
  any of the versions above. On stdio a late answer to the probe still
  counts, so a slow-starting modern server stays modern.
  `mcp.WithProtocolVersion` pins one version.
- **Older servers:** over Streamable HTTP the client keeps the session ID,
  sends it with `MCP-Protocol-Version` on every request, starts a new
  session when the server answers 404 (requests issued meanwhile wait for
  it), and sends `DELETE` on `Close`. Broken SSE streams are resumed with
  `GET` and `Last-Event-ID`, honoring the server's `retry`, including
  servers that poll by closing streams. Missing capabilities give empty
  lists, or an error wrapping `errors.ErrUnsupported`.
- **Client features:** `mcp.WithSampling(handler)` (`mcp.ModelSampler(m)`
  samples with an agentx model; `mcp.ApproveSampling` and
  `mcp.ReviewSampling` let a human check requests and replies),
  `mcp.WithElicitation(handler, modes...)` for form and URL elicitation, and
  `mcp.WithRoots(fn)` with `Client.RootsChanged`. Older servers send these
  as requests; modern ones return `input_required`, and `Call` runs the
  same handlers and retries. Without a handler, older servers get method
  not found and modern ones an `*mcp.InputRequiredError`.
- **Logging and ping:** `mcp.WithLogHandler`, `mcp.WithLogLevel` and
  `Client.SetLogLevel` (`logging/setLevel`, or the per-request `_meta`
  level with modern servers); `Client.Ping` (`server/discover` with modern
  servers, which have no `ping`).
- **Transports:** stdio and Streamable HTTP.
- **Authorization:** `mcp.WithOAuth(cfg)` handles a 401 by reading the
  `WWW-Authenticate` challenge, fetching the Protected Resource Metadata
  (RFC 9728) and the authorization server metadata (RFC 8414 or OpenID
  Connect, issuer checked), obtaining a client ID (pre-registered, Client
  ID Metadata Document, or Dynamic Client Registration) and running the
  authorization code flow with PKCE S256, `resource` (RFC 8707) and `iss`
  validation (RFC 9207). `cfg.Authorize` shows the URL to the user;
  `mcp.NewLoopbackRedirect` receives the redirect on 127.0.0.1. Tokens are
  refreshed, kept in a `TokenStore` (`NewMemoryTokenStore`, or
  `filestore.MCPTokens` of agentx-contrib with mode 0600), and a 403
  `insufficient_scope` steps the scopes up.
  `Grant: mcp.GrantClientCredentials` authorizes machines with a secret or
  `mcp.PrivateKeyJWT`.
- **Notifications:** `Listen` opens a `subscriptions/listen` stream for list
  and resource changes. With older servers it uses the HTTP GET stream
  (stdio needs none) and `resources/subscribe`, and acknowledges the
  subscription itself; their notifications are not filtered.
- **Tool names:** server tools become agentx tools named
  `mcp__<server>__<tool>`.
- **Media:** images, audio and embedded resource blobs in tool results
  become `tool.Result.Parts` (`mcp.ToolResultFrom`), so models that take
  media see them; text and resource links stay text. Sampling handlers
  receive and return image and audio content.
- **Approval and trust:** tools require approval unless the server hints
  that they are not destructive (`destructiveHint: false`); a missing hint
  counts as destructive, as in the MCP spec. That hint is trusted as given,
  but `readOnlyHint` is ignored unless you pass `mcp.TrustReadOnlyHint()`.
  To decide approval without relying on annotations, pass
  `mcp.RequireApprovalFor(fn)`; `RequireApprovalFor(nil)` turns approval
  off for every tool.
- **Name collisions:** long or colliding tool names get a short hash suffix.
  Whether a name collides depends on the other tools the server lists, so
  such a name can change when the tool list changes.

```go
c, err := mcp.Connect(ctx, mcp.Stdio("npx", "-y", "@modelcontextprotocol/server-filesystem", "."))
if err != nil {
	log.Fatal(err)
}
defer c.Close()
fsTools, err := mcp.Tools(ctx, c, "fs")
if err != nil {
	log.Fatal(err)
}
a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(fsTools...))
```

For a remote server, use `mcp.HTTP("https://example.com/mcp")`:

```go
lr, err := mcp.NewLoopbackRedirect("")
if err != nil {
	log.Fatal(err)
}
defer lr.Close()
cfg := &mcp.OAuthConfig{Store: filestore.MCPTokens(tokenPath)}
lr.Configure(cfg)
c, err := mcp.Connect(ctx, mcp.HTTP("https://example.com/mcp"), mcp.WithOAuth(cfg))
```

`mcp.NewServer(info, opts)` serves agentx tools, resources and prompts to
clients of every version above:

- **Tools:** the tools of a `tool.Registry` (`ServerOptions.Registry` or
  `Tools`), with their schemas, run through `tool.Executor`; `ReadOnly`
  becomes `readOnlyHint` and `RequiresApproval` `destructiveHint`.
  `tool.Result.Parts` become image and audio content, embedded resources
  for other media (and for audio before 2025-03-26), and resource links
  for media by URL (text before 2025-06-18).
  `mcp.AgentTool(a, nil)` exposes an agent, asking the client's user to
  approve its tool calls.
- **Resources and prompts:** `AddResource`, `AddResourceTemplate`
  (`{var}` and `{+var}`), `AddPrompt`, or `ResourceSource` and
  `PromptSource` implementations. Changes notify `subscriptions/listen`
  streams and sessions; `ResourceUpdated(uri)` reaches subscribers.
- **Handlers:** `mcp.Sample`, `mcp.Elicit` and `mcp.ListRoots` ask the
  client for input: older clients get a request, newer ones an
  `input_required` result whose `requestState` is signed and bound to the
  request. The handler just blocks until the retry; on another instance
  sharing `StateKey` it runs again with the earlier answers. `mcp.Log`,
  `mcp.Progress` and `mcp.CurrentRequest` work in both eras.
- **Transports:** `srv.ServeStdio(ctx, os.Stdin, os.Stdout)` and
  `srv.HTTPHandler(opts)`: modern requests are stateless and checked
  against their `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` and
  `Mcp-Param-*` headers; `initialize` starts a session with
  `Mcp-Session-Id`, resumable SSE streams (`Last-Event-ID`, optional
  polling), a GET stream and `DELETE`. Origins other than loopback and
  `AllowedOrigins` get 403. `ModernOnly` refuses the older versions.
- **Authorization:** `HTTPOptions.Auth` validates bearer tokens with your
  `Verify` function, answers 401 and 403 `insufficient_scope` challenges
  naming the Protected Resource Metadata, which it serves, and binds
  sessions to the token's subject.

```go
srv := mcp.NewServer(mcp.Implementation{Name: "files", Version: "1.0"},
	&mcp.ServerOptions{Tools: []tool.Tool{readTool, mcp.AgentTool(a, nil)}})
http.Handle("/mcp", srv.HTTPHandler(nil))
```

### Middleware

The core loop only calls the model, runs tools, handles approvals and saves
checkpoints. Everything else plugs in as runtime middleware, including
hooks and context engineering, which `agent.New` installs for you. A
middleware implements `loop.Middleware`, which only has a `Name`, plus
any of these stages:

| Stage | When it runs |
| --- | --- |
| `WrapContext` | Once when `Run` or `Resume` starts; derives the run's context, such as a trace span, and releases it when the call returns |
| `BeforeRun` | A new run starts, before the input enters the conversation |
| `BeforeTurn` | Each turn, before the model request is built |
| `BeforeModel` / `AfterModel` | Before and after each model call |
| `WrapModel` | Around each model call; can skip the call or retry it |
| `BeforeTool` | Each tool call: allow it, ask for approval or deny it |
| `RevalidateTool` | Approved argument edits, before execution; enforce argument policies without repeating hooks |
| `WrapTool` | Around each tool execution |
| `AfterTool` | After each tool call; can rewrite the result |
| `BeforeFinish` | The model answered without tool calls; can keep the run going |
| `AfterRun` | The run ended, including when it was interrupted. If the caller stopped reading, the status is `running`; if it stopped at the first event, `AfterRun` does not run |

- **Order:** `Before` stages run in registration order and `After` stages
  in reverse. For `Wrap` stages, the first middleware is the outermost.
- **Errors:** returning `loop.Block` or `loop.Stop` ends the run as
  blocked or stopped. Any other error fails the run.
- **State:** values set with `run.SetValue` are saved with checkpoints, so
  they survive an interrupted run.
- **Tools:** a middleware that implements `loop.ToolProvider` adds its
  tools. Middleware enforcing argument policies in `BeforeTool` must also
  implement `loop.ToolRevalidator` to check approved edits. Permission and
  tool guardrails do so. Revalidation can deny or fail the call; it must not
  rewrite approved arguments. An ask decision is already satisfied by the
  explicit approval. Hooks and unchanged approvals are not repeated.

The `middleware` package provides tracing and metrics. Logging
(`logging.New`) and retries with exponential backoff (`retry.New`), which
retry only transient failures, are in the `middleware` module of
[agentx-contrib](https://github.com/nostosx/agentx-contrib#middleware).
`middleware.ErrorType(err)` returns the `error.type` value the tracing and
metrics record for an error, for middleware of your own.

`middleware.Tracing` and `middleware.Metrics` follow the
[OpenTelemetry GenAI semantic conventions](https://github.com/open-telemetry/semantic-conventions-genai).
Each run gets an `invoke_agent {agent}` span. Its children are a client span
per model call, named like `chat gpt-6-astra`, and an `execute_tool {tool}`
span per tool call; a sub-agent's run nests under the `execute_tool` span
that started it. Spans carry `gen_ai.*` attributes, including token usage
with cache and reasoning tokens, and the run ID, session ID, agent name
and turn (the `invoke_agent` span has no turn). Providers report their
`gen_ai.provider.name` through `model.Describer`.

- **Other spans.** Hooks get a `hook {event}` span and automatic compaction a
  `compact_context` span; later model spans then carry
  `gen_ai.conversation.compacted`. Each retry of agentx-contrib's `retry`
  middleware is an `agentx.retry` event on the model call's span.
- **Handoffs and guardrails.** Each agent of a handed-off run gets its own
  `invoke_agent` span; the spans after a handoff carry
  `agentx.handoff.from`. A run a guardrail blocked carries
  `agentx.guardrail.name` and `agentx.guardrail.kind` and an
  `agentx.guardrail` event, with the guardrail's message when content is
  recorded.
- **Metrics.** Model call duration and streaming chunk timing, token usage
  counters and per-call histograms (including cache and reasoning tokens),
  tool call duration, and run duration with the run status and the run's own
  model and tool call counts. `middleware.LegacyMetrics()` also records the
  replaced `gen_ai.client.token.usage` and `gen_ai.client.operation.duration`.
- **Content.** The system prompt, messages, tool names, and tool arguments
  and results are recorded only with `middleware.RecordContent()`, because
  they may contain sensitive data.
- **OpenTelemetry.** The core stays dependency-free; the `otel` module of
  [agentx-contrib](https://github.com/nostosx/agentx-contrib#opentelemetry)
  adapts OpenTelemetry providers to `middleware.Tracer` and
  `middleware.Meter`. For other libraries, implement the two interfaces.

Register tracing and metrics before retries, so a model call's span and
duration cover its retries:

```go
a := agent.MustNew("assistant",
	agent.WithModel(m),
	agent.WithMiddleware(otel.Middleware(tracerProvider, meterProvider)...),
	agent.WithMiddleware(
		logging.New(slog.Default()),
		retry.New(retry.Policy{MaxAttempts: 5}),
	),
)
```

To write your own, use `loop.Funcs` or a type that implements the
stages:

```go
redact := &loop.Funcs{
	MiddlewareName: "redact",
	AfterToolFunc: func(_ context.Context, _ *loop.Run, _ *loop.ToolPlan, res *model.ToolResult) error {
		res.Content = apiKey.ReplaceAllString(res.Content, "[redacted]")
		return nil
	},
}
```

Some model calls don't go through the agent loop, such as the model of a
`actx.Summarize` strategy. For those, wrap the model directly:
`middleware.Wrap(m, middleware.TraceModel(tracer), retry.Model(policy))`.

### Serving

The `server` package serves a `Runner` over HTTP, using only the standard
library:

```go
srv := server.New(runner,
	server.WithAuth(checkToken),       // func(*http.Request) error
	server.WithCheckpointer(cp),       // report interrupts after a restart
	server.WithCORS("https://app.example"))
defer srv.Close()
http.ListenAndServe(":8080", srv)
```

| Endpoint | Purpose |
| --- | --- |
| `POST /sessions/{id}/runs` | Run `{"input": "..."}` or `{"message": {...}}` |
| `POST /sessions/{id}/resume` | Resume with `{"approvals": {"<call_id>": {"approved": true}}}` |
| `POST /sessions/{id}/discard` | Discard the interrupted or failed run |
| `GET /sessions/{id}` | Messages and the pending run with its approval requests |
| `DELETE /sessions/{id}` | `Runner.EndSession`; `?purge=true` also deletes it |
| `POST /sessions`, `GET /sessions`, `POST /sessions/{id}/clear`, `/fork` | Create, list, clear and fork |
| `GET /sessions/{id}/checkpoints`, `POST /sessions/{id}/rewind` | List checkpoints; rewind with `{"checkpoint_id": "...", "mode": "both"}` (`code`, `conversation`) |
| `POST /chat` | Vercel AI SDK `useChat` |
| `POST /agui` | AG-UI |
| `GET /.well-known/agent-card.json`, `POST /a2a` | A2A 1.0 (JSON-RPC) |

- **Wire format.** Runs stream Server-Sent Events: the event name is the
  event type, the ID its sequence number, and the data the `loop.Event`
  as JSON with snake_case fields. `Accept: application/json` returns the
  final `Result` instead, and `application/x-ndjson` streams JSON Lines.
  `server.WriteJSONL(w, events)` writes the same lines, for example for a
  command's output. The interrupt's run state and the full conversation are
  left out.
- **Protocols.** `?protocol=vercel-ui` or `ag-ui`, or `WithProtocol`, streams
  the [AI SDK UI message stream](https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol)
  or [AG-UI events](https://docs.ag-ui.com/concepts/events) from the session
  endpoints too. Tool approvals map to the AI SDK's approval parts and to
  AG-UI interrupts; handoffs and output retries to `data-handoff` and
  `data-output-retry` parts and AG-UI `CUSTOM` events; a guardrail block to
  the finish reason `content-filter` and `RUN_ERROR` code `blocked`.
- **Files.** User messages of every protocol take images, audio, video,
  text, PDF and Office files, inline or by URL, as model parts; other types
  get 415. The UI message stream sends media of answers and tool results as
  file parts. An answer an output guardrail blocks is never saved; with
  `guard.HoldOutput()` it is never streamed either.
- **A2A.** `SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`,
  `CancelTask` and `SubscribeToTask` of the
  [A2A specification](https://a2a-protocol.org/latest/specification/). The
  `contextId` is the session ID; a run waiting for approvals puts its task in
  `TASK_STATE_INPUT_REQUIRED`, and a guardrail block in
  `TASK_STATE_REJECTED`.
- **Concurrency.** A session runs one request at a time: another one gets
  409. `session.ErrConflict` is 409 as well.
- **Disconnects.** If the client goes away, the run stops and the session is
  saved. With a `Checkpointer`, `POST /sessions/{id}/resume` continues it.
- **Limits.** Bodies are capped by `WithMaxBodyBytes`. Clients may set
  `max_turns` and `limits` only with `AllowRunOptions`.
  `WithSessionAccess` authorizes each session ID.

See the `server` package documentation for the full list of endpoints and
error codes.

### Evaluation

The `eval` package runs a task over a dataset and scores each output, in the
style of [pydantic-evals](https://pydantic.dev/docs/ai/evals/) and
[Google ADK evaluation](https://adk.dev/evaluate/):

```go
ds := &eval.Dataset[string, string]{
	Name: "weather",
	Cases: []eval.Case[string, string]{{
		Name: "kyoto", Input: "Weather in Kyoto?", Expected: new("Sunny"),
		Evaluators: []eval.Evaluator{eval.ToolTrajectory(eval.MatchExact,
			[]eval.ToolCall{{Name: "weather", Args: map[string]any{"city": "Kyoto"}}})},
	}},
	Evaluators: []eval.Evaluator{
		eval.Contains("sunny", eval.IgnoreCase()),
		eval.MaxTurns(3),
		eval.LLMJudge(judge, "The answer states the forecast without hedging.", eval.JudgeInput()),
	},
}

func TestWeather(t *testing.T) {
	eval.Run(t, ds, eval.AgentTask(a), eval.Repeat(3), eval.Threshold(0.9))
}
```

- **Tasks.** Any `func(ctx, In) (Out, *loop.Result, error)`.
  `eval.AgentTask(a)` runs an agent on the input text, `TypedAgentTask[T]`
  decodes structured output and `AgentTaskFunc` maps other inputs.
  `SimulateUser(a, userModel)` holds a multi-turn conversation with a user
  played by a model following a `Scenario`, like ADK's user simulation.
- **Evaluators.** They return assertions, scores and labels with reasons:
  `Equals`, `EqualsExpected`, `Contains`, `Regex`, `JSONSchema`, `IsType`,
  `DecodesAs`, `Completed`, `MaxTurns`, `MaxTokens`, `MaxToolCalls`,
  `MaxDuration`, `ResponseMatch` (ROUGE-1) and `ToolTrajectory`
  (`MatchExact`, `MatchInOrder`, `MatchAnyOrder` or `MatchSubset`, with
  `PartialArgs` or `IgnoreArgs`). Write others with `Check`, `Func` or
  `TypedCheck`.
- **LLM judge.** `LLMJudge` asks a model for a verdict (reason, pass and,
  with `JudgeScore`, a score from 0 to 1), requested and validated as
  structured output, at temperature 0 by default. `JudgeInput` and
  `JudgeExpected` show the judge the input and expected output;
  `JudgeSeed` and `JudgeReasoning` are optional.
- **Spans.** Trace the agent with `middleware.Tracing(eval.Tracer())`: the
  spans of each run are in `EvaluatorContext.Spans`, and `HasSpan` and
  `NoSpan` match them with a `SpanQuery`.
- **Running.** `Dataset.Evaluate` takes `Concurrency` (8 by default),
  `Repeat`, `Timeout`, `EvaluatorTimeout`, `Experiment` and `OnResult`. A
  task or evaluator that fails, panics or hangs fails only its case.
- **Reports.** Pass rate, assertion rates, mean scores, labels, tokens and
  duration percentiles, with `ByCase` for repeated cases. `WriteText` prints
  a table, `Save` and `LoadReport` use JSON, and `eval.Compare(baseline,
  candidate)` lists regressions. `eval.Run` logs the report and fails the
  test below `Threshold` or `MinScore`, or when a case regressed from
  `Baseline`.
- **Datasets.** `eval.LoadDataset[In, Out](path)` and `Dataset.Save` read
  and write JSON or JSON Lines (`.jsonl`). Evaluators are code; expected tool
  calls can be stored in case metadata under `tool_calls`.

**Record and replay.** `eval/replay` records HTTP interactions to a cassette
file and replays them, so provider and agent tests run without network
access. Pass its client to a provider, such as one of agentx-contrib:

```go
rec := replay.NewTest(t, "testdata/weather.json")
m, _ := openai.New(openai.WithHTTPClient(rec.Client()))
```

`AGENTX_REPLAY=record go test ./...` records with real credentials; the
default `replay` mode answers from the cassette, and `auto` records only
missing cassettes. Requests match by method, URL and body (as JSON).
Credential headers such as `Authorization`, `x-api-key` and `x-goog-api-key`,
and query keys such as `key`, are saved as `REDACTED`; `WithFilter` scrubs
bodies. Server-Sent Events are saved and replayed event by event.

## Providers

`model.Model` is a small interface; the implementations live in the
`provider` module of
[agentx-contrib](https://github.com/nostosx/agentx-contrib#providers), which
calls the APIs directly over `net/http` without provider SDKs:

| Package | API |
| --- | --- |
| `github.com/nostosx/agentx-contrib/provider/anthropic` | Anthropic Messages API, also on Vertex AI |
| `github.com/nostosx/agentx-contrib/provider/openai` | OpenAI Responses and Chat Completions, Azure OpenAI and compatible services such as Ollama, DeepSeek and OpenRouter |
| `github.com/nostosx/agentx-contrib/provider/gemini` | Gemini `generateContent`, also on Vertex AI |
| `github.com/nostosx/agentx-contrib/provider/bedrock` | Amazon Bedrock Converse and ConverseStream |

Each reads its key from the environment, such as `OPENAI_API_KEY`:

```go
m, err := openai.New(openai.DeepSeek()) // reads DEEPSEEK_API_KEY
```

The agentx-contrib README lists the environment variables, the presets of
OpenAI-compatible services, cloud platform options and the media each
provider accepts. For request fields specific to one provider, use
`agent.WithExtra`. Any other model works once it implements `model.Model`;
`model/modeltest` provides a scripted model for tests.

## Packages

| Package | Responsibility |
| --- | --- |
| `agent` | Agent definition, options, `Runner` (sessions, resume and rewind) |
| `loop` | Agent loop, tool execution, approvals, events, checkpoints and the middleware interface |
| `model` | Neutral messages, requests, responses, streaming and errors; `modeltest` provides a scripted model for tests |
| `tool` | Tool interface, JSON Schema, registry, executor and middleware |
| `tool/builtin` | Built-in file, search, shell, web and todo tools; file history |
| `actx` | Context assembly, token estimates, compaction strategies; runs as middleware |
| `session` | Sessions and their storage |
| `memory` | Long-term memory with keyword, vector and hybrid retrieval |
| `rag` | Retrieval-augmented generation: loaders, splitters, indexing, retrievers, search tool and context provider |
| `rag/ragtest` | Conformance tests for `rag.VectorStore` implementations and a deterministic test embedder |
| `skill` | `SKILL.md` loading, discovery, registry and invocation |
| `hook` | Lifecycle hooks and command hooks; runs as middleware |
| `permission` | Claude Code permission rules, modes and settings; runs as middleware |
| `guard` | Input, output and tool guardrails; runs as middleware |
| `workflow` | Graph orchestration |
| `middleware` | Runtime middleware for tracing and metrics, plus plain model decorators |
| `mcp` | MCP client and server, transports, OAuth and tool adapter |
| `server` | HTTP serving: session API, Vercel AI SDK, AG-UI and A2A |
| `eval` | Datasets, evaluators (including LLM judges and tool trajectories), reports and comparisons |
| `eval/replay` | HTTP record and replay for provider tests |

[agentx-contrib](https://github.com/nostosx/agentx-contrib) holds the
implementations with dependencies or provider-specific code, each in its
own module: `provider` (OpenAI, Anthropic, Gemini and Bedrock), `otel`,
`sqlstore`, `filestore` and `middleware` (logging and retries). Runnable
applications live in the separate agentx-examples project.

## Examples

Runnable examples, from single-turn Q&A to serving an agent over HTTP with
tool approval, live in the separate
[agentx-examples](https://github.com/nostosx/agentx-examples) project. Check out
agentx and agentx-contrib alongside it; see its README for the layout:

```sh
cd agentx-examples
export ANTHROPIC_API_KEY=...
go run ./cmd/basic "What is an agent loop?"
```

## Development

Use Go 1.27.3, GNU Make and Bash. `make help` lists all targets.

```sh
make check      # format, modernizer and tidy checks, vet, build, dependency boundary, race tests
make lint       # golangci-lint
make vulncheck  # reachable vulnerability scan
make test       # unit tests without the race detector
make race       # race tests with shuffled execution order
```

Checks do not apply formatting, modernizer or tidy changes. Run `make fmt`,
`make fix` or `make tidy` explicitly to apply them, then review the diff.
All targets set `GOWORK=off`. The `deps-check` gate included in `check`
allows only the standard library, agentx and kitx, including in tests.
Implementations that need other dependencies belong in agentx-contrib.

CI runs `make check` on Linux, macOS and Windows, with independent lint and
vulnerability jobs. See [CONTRIBUTING.md](CONTRIBUTING.md) for tool setup and
contribution requirements, [SECURITY.md](SECURITY.md) for vulnerability reports
and the known sandbox and permission limitations, and
[RELEASING.md](RELEASING.md) for version ordering and release gates.

## License

[MIT](LICENSE)

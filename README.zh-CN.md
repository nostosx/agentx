# agentx

[English](README.md) | 简体中文

agentx 是一个 Go Agent 框架，用于调用工具、维护对话和恢复被中断的任务。
它通过可组合的包提供流式 Agent Loop、类型化工具、权限、检查点和上下文管理。

- **轻量依赖**：只依赖 Go 标准库和 [kitx](https://github.com/nostosx/kitx)。
  模块包含各项契约，以及每项契约的一个内存
  参考实现；模型 Provider、文件与 SQL 存储、OpenTelemetry、日志和重试位于
  [agentx-contrib](https://github.com/nostosx/agentx-contrib)。
- **小接口**：`model.Model`、`tool.Tool`、`hook.Hook`、`session.Store` 等核心
  类型都是小接口，每一个都可以单独替换。
- **迭代器流式**：运行过程是 `iter.Seq2[*loop.Event, error]`，模型流是
  `iter.Seq2[*model.StreamEvent, error]`，用普通的 `for range` 就能读取。

> 需要 Go 1.27.3 或更高版本，当前版本为 v1.0.1。
> 已发布的 v1 版本保持公开 API 兼容，具体变更见 [CHANGELOG.md](CHANGELOG.md)。

## 安装

Go 1.27.3 是工具链要求，库版本独立遵循语义版本。核心通过 Go 模块代理使用
kitx v1.0.0，无需并排检出仓库或添加本地替换。下面的命令同时安装快速开始所用的
provider 模块；该模块独立维护版本。

在应用的 Go 模块内执行安装命令。新建应用可以先运行：

```sh
mkdir agentx-demo
cd agentx-demo
go mod init example.com/agentx-demo
```

```sh
go get github.com/nostosx/agentx@v1.0.1 github.com/nostosx/agentx-contrib/provider@v1.0.0
```

核心模块定义 `model.Model`；OpenAI、Anthropic、Gemini 和 Bedrock 的实现位于
agentx-contrib 的 `provider` 模块，它通过 `net/http` 直接调用 API，无需厂商 SDK。

## 快速开始

把下面的程序保存为该目录下的 `main.go`：

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
	m, err := anthropic.New() // 读取 ANTHROPIC_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New("assistant",
		agent.WithModel(m),
		agent.WithInstructions("你是一个简洁的助手。"),
	)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	answer, err := a.Ask(ctx, "什么是 Agent Loop？")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(answer)
}
```

保存文件后，解析代码所导入的依赖：

```sh
go mod tidy
```

在 shell 中设置 `ANTHROPIC_API_KEY`，然后运行 `go run .`。程序会请求真实的
Anthropic API，可能产生费用。需要其他端点时可以设置 `ANTHROPIC_BASE_URL`；
使用 Anthropic 官方服务时保持未设置。账户需要有 Provider 默认模型的访问权限；
也可以在构造时通过 `anthropic.WithModel("your-model-id")` 选择其他模型。

下面的功能示例是代码片段，复用应用中的 `ctx`、`m` 等值并省略 import。
使用 contrib 包时还需要安装对应模块，见
[agentx-contrib](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#安装)。

## 功能

[工具](#类型化工具) · [运行](#agent-loop-与事件) · [权限](#权限) ·
[会话](#session) · [RAG](#检索增强生成) · [Workflow](#workflow) ·
[MCP](#mcp) · [服务化](#服务化)

### 类型化工具

`tool.Func` 根据参数结构体生成 JSON Schema，并在你的函数运行前用它校验模型
给出的参数。`jsonschema` 结构体标签用于细化 Schema，支持 Go 生态中常见的两种
写法。普通标签整体作为字段描述，与官方 MCP Go SDK 和 ADK Go 所用的
[google/jsonschema-go](https://github.com/google/jsonschema-go) 一致。以关键字
开头的标签使用 [invopop/jsonschema](https://github.com/invopop/jsonschema) 的
`key=value` 写法：`title`、`description`、`enum`（可重复）、`default`、
`format`、`pattern`、`minimum`、`maximum`、`exclusiveMinimum`、
`exclusiveMaximum`、`minLength`、`maxLength`、`minItems`、`maxItems`，以及
`required` 和 `nullable` 两个标志。值中的逗号写作 `\\,`；`jsonschema:"-"`
会把字段排除在 Schema 之外。

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"城市名，例如 Tokyo"`
	Unit string `json:"unit,omitempty" jsonschema:"enum=celsius,enum=fahrenheit,default=celsius"`
	Days int    `json:"days,omitempty" jsonschema:"minimum=1,maximum=7,description=预报天数\\, 1 表示今天"`
}

weather := tool.Func("get_weather", "查询城市当前天气。",
	func(ctx context.Context, in weatherArgs) (string, error) {
		return in.City + "：22°C，多云", nil
	}, tool.ReadOnly())

a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(weather))
```

- 同一次模型回复里相邻的 `ReadOnly` 或 `Concurrent` 工具调用会并发执行。
  `Concurrent` 工具可以有副作用，所以运行恢复时不会重复被中断的调用。
- 工具出错、参数不合法或 panic 都会作为错误结果返回给模型，不会中止运行。
- 参数必须是 I-JSON（RFC 7493）：重复成员名或含有无效 UTF-8 的 JSON 不合法，
  否则 Hook、权限规则、工具和 MCP 服务端可能读出不同的内容。同理，与已声明
  属性只差大小写的成员（如与 `command` 并存的 `Command`）也不合法：
  `encoding/json` 会把它解码进该字段，而 Hook 和规则按精确键名读取。
- `tool.Media(text, parts...)` 在文本之外返回图片或文档，见
  [多模态内容](#多模态内容)。`Result.Metadata` 会传到
  `model.ToolResult.Metadata` 和事件中，不发给模型。

### 内置工具

`tool/builtin` 提供一组参照 Claude Code 和 Codex 设计的通用工具：

| 工具 | 用途 | 是否需要审批 |
| --- | --- | --- |
| `read_file` | 带行号分页读取文本文件；图片（PNG、JPEG、GIF、WebP，最大 5 MB）和 PDF（最大 20 MB）作为 part 返回给模型查看 | 否（只读） |
| `write_file` | 创建或覆盖文件；覆盖时返回 diff | 是 |
| `edit_file` | 精确替换文本；未设置 `replace_all` 时匹配必须唯一。`edits` 按顺序做多处替换，要么全部成功，要么都不生效。返回 diff | 是 |
| `apply_patch` | 应用 Codex 格式的补丁，新增、更新、重命名和删除文件，要么全部成功，要么都不生效。返回摘要和 diff | 是 |
| `glob` | 按模式查找文件，支持 `**` 和 `{a,b}`；最近修改的排在前面 | 否（只读） |
| `grep` | 用正则表达式搜索文件内容，可显示上下文行，跳过二进制文件 | 否（只读） |
| `bash` | 带超时执行 shell 命令，或用 `run_in_background` 在后台执行；输出过长时保留开头和结尾 | 是，沙箱内执行时否 |
| `bash_output` | 读取后台命令的新输出（可用正则表达式过滤）及其状态 | 否 |
| `kill_shell` | 结束后台命令及其启动的所有进程 | 否 |
| `web_fetch` | 抓取 URL，并把 HTML 转成纯文本 | 否（只读） |
| `todo_write` | 维护任务清单，应用可以读取并展示 | 否 |

```go
todos := &builtin.TodoList{}
a := agent.MustNew("coder", agent.WithModel(m),
	agent.WithTools(builtin.Tools(builtin.Config{Dir: ".", Todos: todos})...))
```

- **文件限制**：文件类工具通过 `os.Root` 限制在 `Config.Dir` 内，`..` 和符号
  链接都无法越界。
- **先读后写**：`builtin.Tools` 返回的工具共享一个 `FileTracker`。
  `write_file`、`edit_file` 和 `apply_patch` 只有在 `read_file` 读过某个
  已有文件、且该文件之后没有被改动时，才会修改或删除它。
- **原子编辑**：文件通过同一目录下的临时文件替换，并保留原有权限；符号链接
  会原地写入目标文件。`apply_patch` 在写入任何内容之前检查所有操作，后续写入
  失败时会恢复已经改动的文件。
- **编辑格式**：`edit_file` 遵循 Claude Code 的 Edit 工具，`edits` 参数取代了
  以前的 MultiEdit 工具。`apply_patch` 使用 GPT 模型训练时采用的补丁格式，并像
  Codex 一样定位改动：先精确匹配，再依次忽略行尾空白、首尾空白和排版标点差异。
  OpenAI Responses API 会以受 Codex Lark 语法约束的自由格式工具接收它，其他
  Provider 则把补丁放在 JSON 参数 `input` 中发送。`Config.Edit` 可选
  `builtin.EditAll`（默认）、`EditReplace`（只有 `edit_file`）或 `EditPatch`
  （只有 `apply_patch`）。自定义的自由格式工具可以使用 `tool.Freeform`。
- **忽略文件**：`glob` 和 `grep` 总是跳过 `.git` 和 `node_modules`；
  `.gitignore` 排除的路径也会跳过，除非模型设置了 `include_ignored`。
  `.git/info/exclude` 以及上级目录直到仓库根目录的 `.gitignore` 也会生效。
  `.git` 文件或 `commondir` 只有指向包含 `HEAD` 的本地 git 目录时才会被
  采用，网络路径（例如 UNC 共享）从不访问。
- **Shell 访问**：`PATH` 中有 bash 时用 bash 执行，否则用 `sh`（Windows 上
  为 `cmd.exe`）。`bash` 在 `Config.Dir` 下启动，没有沙箱时不受该目录限制；
  除非设置 `Config.NoApproval`，否则需要审批。命令结束或超时时会结束整个进程树，
  包括后台进程。与 Claude Code 一样，`cd` 只要仍在 `Config.Dir` 内（解析符号
  链接后），就会延续到后续调用；否则下一次调用从 `Config.Dir` 开始，结果中注明
  `Shell cwd was reset to <dir>`。Shell 通过 `EXIT` trap 把目录写到描述符 3；
  `Config.ResetDir` 关闭此功能，Windows 上从不跟踪。
- **后台命令**：设置 `run_in_background` 时，`bash` 立即返回 `bash_1` 这样的
  ID。`bash_output` 返回命令自上次读取以来的输出，以及它是否仍在运行或如何
  结束；`kill_shell` 结束它的进程树。后台命令由 `builtin.Shells` 管理（Tools
  会自动创建）：最多同时运行 8 个，每个保留最后 1 MiB 输出，30 分钟后被结束，
  `timeout_ms` 最多可延长到 2 小时。Agent 结束时调用 `builtin.Close(tools)` 或
  `Shells.Close`。在 Hook 和权限规则中，Claude Code 的 `BashOutput`/`TaskOutput`
  和 `KillShell`/`TaskStop` 分别匹配这两个工具。
- **沙箱**：`Config.Sandbox` 让每条 `bash` 命令（包括后台命令）在操作系统沙箱
  中运行。`builtin.NewSandbox(policy)` 在 macOS 上使用 Seatbelt
  （`/usr/bin/sandbox-exec`），配置参照 Codex，默认拒绝一切；在 Linux 上使用
  bubblewrap（`bwrap`），挂载方式参照 Codex（`--ro-bind / /`、重新以读写方式
  绑定可写目录、新的 user、PID、IPC 和网络命名空间、`--die-with-parent`）。
  命令可以读取所有内容（或仅 `ReadableRoots`），`DenyRead` 除外；只能写入
  `Config.Dir`、`WritableRoots` 和临时目录，且不能写这些目录顶层的 `.git`、
  `.agents`、`.codex`、`.aws` 和 `.claude`；默认没有网络，除非设置
  `AllowNetwork`，或在 `AllowedDomains` 中列出主机，由本地 HTTP/CONNECT 代理
  通过 `HTTP_PROXY` 和 `HTTPS_PROXY` 按域名放行（Linux 上用 socat 桥接）。
  与 Claude Code 的 auto-allow 模式一样，沙箱内的命令无需审批（`AskSandboxed`
  恢复审批）；带 `dangerously_disable_sandbox` 重试的命令在沙箱外运行，总是
  需要审批，`NoSandboxEscape` 禁止这种重试。权限规则仍然生效，例如
  `Bash(dangerously_disable_sandbox:true)`。没有可用的后端时，
  `SandboxRequired`（默认）让命令失败，`SandboxBestEffort` 在审批后不经沙箱
  运行。限制：代理不检查 TLS；忽略代理变量的程序无法联网；macOS 上无法运行
  `ps` 等 setuid 程序；Linux 不使用 seccomp 或 Landlock，因此只对命令隐藏容器
  守护进程的 socket，且只保护已存在的路径。Linux 会先完成所有可写目录挂载，
  再施加路径保护，避免重叠的父目录覆盖保护；Windows 没有后端。

  ```go
  sb := builtin.NewSandbox(builtin.SandboxPolicy{
      DenyRead:       []string{"~/.ssh", "~/.aws"},
      AllowedDomains: []string{"proxy.golang.org", "*.githubusercontent.com"},
  })
  defer sb.Close()
  tools := builtin.Tools(builtin.Config{Dir: ".", Sandbox: sb})
  defer builtin.Close(tools)
  ```
- **网络访问**：`web_fetch` 默认拒绝回环、私有、链路本地和共享（CGNAT）
  地址，重定向后也会检查；直连时 DNS 变化后同样会检查。环境变量里配置的代理
  仍然可用，但此时只在每次请求前按域名检查目标，因为代理会重新解析域名。
  请求 60 秒后超时。需要访问这些地址时设置 `Config.AllowPrivateNetwork`，或者
  传入自己的 `HTTPClient`，它会替代这些检查。
- **只读子集**：`builtin.ReadOnlyTools` 只返回无副作用的工具，适合给做调研的
  子 Agent 使用。
- **文件历史**：设置 `Config.History` 后，`write_file`、`edit_file` 和
  `apply_patch` 修改文件前会保存它的状态，会话因此可以回退；见
  [检查点与回退](#检查点与回退)。`bash` 做的改动（无论是否在沙箱中）不会被
  记录。

### Agent Loop 与事件

`Run` 返回一个事件流，包括：

- `run_start`、`turn_start`
- `compacted`（调用模型前压缩了对话时出现）
- `delta`（仅在开启流式时出现）
- `message`
- `tool_call`、`tool_result`
- `output_retry`（最终回答不符合输出 Schema 并被发回时出现，`Event.OutputRetry`
  含问题列表和第几次尝试）
- `handoff`（运行转到另一个 Agent 时出现，见 Handoff）
- `interrupted`
- `run_end`

```go
for ev, err := range a.Run(ctx, agent.Text("规划一次京都旅行")) {
	if err != nil {
		log.Fatal(err)
	}
	switch ev.Type {
	case loop.EventDelta:
		fmt.Print(ev.Text())
	case loop.EventToolCall:
		fmt.Println("->", ev.ToolCall.Name)
	case loop.EventRunEnd:
		fmt.Println("\n状态：", ev.Result.Status)
	}
}
```

`loop.Collect` 读完所有事件后返回 `*loop.Result`，其中包含输出文本、
完整对话、Token 用量和轮数。

### 结构化输出

`agent.WithOutput[T]` 要求模型按结构体 `T` 的 Schema 输出 JSON，并用它检查
最终回答。`agent.AskAs[T]` 运行一条提示并解码回答；`loop.Result` 的方法
`Result.DecodeAs[T]` 和 `Result.Decode` 可以解码任意运行的结果。

```go
type Forecast struct {
	City  string `json:"city"`
	TempC int    `json:"temp_c" jsonschema:"Temperature in Celsius"`
}

a := agent.MustNew("weather", agent.WithModel(m), agent.WithTools(weather),
	agent.WithOutput[Forecast]())
f, err := agent.AskAs[Forecast](ctx, a, "京都天气如何？")
```

- **请求**：Schema 以严格模式作为响应格式发送，有工具时也一样；
  `WithResponseFormat` 设置了响应格式时以它为准。
- **服务商子集**：服务商拒绝或忽略的约束（例如 Anthropic 的 `minimum`，
  Gemini 的 `minLength`）会移入字段描述；严格工具的 Schema 同样
  处理。4.5 之前的 Claude 模型，以及有工具时的 Gemini 2.x，改为通过系统指令
  收到 Schema。回答仍按完整 Schema 检查。
- **重试**：不符合的回答会连同发现的问题发回给模型，默认两次
  （`agent.OutputRetries(n)`），每次占一轮并发出 `output_retry` 事件。之后运行
  以 `*loop.OutputError` 失败，恢复运行会重新给予重试次数。
- **代码块**：回答是单个 Markdown 代码块（例如 ```` ```json ````）时，去掉
  围栏后检查和解码。JSON 前后有其他文字则检查不通过。
- **不检查**：被输出 token 上限截断、被内容过滤拦下或被拒答的回答照常结束
  运行，解码它们会返回错误。

### 多模态内容

消息和工具结果可以携带图片、文档、音频和视频。Part 保存数据、URL 或 Provider
文件 ID；checkpoint 和 session 用 base64 保存数据。

```go
msg := model.NewMessage(model.RoleUser,
	model.TextPart("对比报告和图表。"),
	model.DocumentPart("application/pdf", pdf, "report.pdf"),
	model.ImagePart("image/png", chart))

shot := tool.Func("screenshot", "截取屏幕。",
	func(ctx context.Context, _ struct{}) (*tool.Result, error) {
		return tool.Media("主显示器", model.ImagePart("image/png", capture())), nil
	})
```

- **Provider。** 各 Provider 接受和返回哪些内容，见
  [agentx-contrib README](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#多模态内容)。
- **错误。** Provider 或模型不支持的内容会在请求前以永久错误失败，例如
  Anthropic 上的音频，或 Chat Completions 上的文档 URL。
- **选项。** `FileID` 指向 Files API 上传的文件（OpenAI、Anthropic）；
  Gemini 文件用其 URI 放在 `URL` 中。`Detail` 对应 OpenAI 的 `detail` 和
  Gemini 3 的单个 part `mediaResolution`。`Citations` 为 Anthropic 和
  Bedrock 文档开启引用；Anthropic 文本把引用保存在 `Part.Raw` 中并原样发回。
- **上下文。** Token 估算会计入媒体；`ClearToolResults` 清理结果时连同媒体
  一起清除。

### 用量限制

`agent.WithLimits` 限制每次运行；`Input.Limits` 和 `Input.MaxTurns` 为单次
运行覆盖用量限制和轮数上限，传给 `Runner.Run` 或 `Runner.RunText` 的
`agent.RunLimits` 和 `agent.RunMaxTurns` 也一样。值为零的字段不限制。

```go
agent.WithLimits(loop.Limits{Requests: 20, ToolCalls: 50, TotalTokens: 200_000}),
agent.WithBudget(func(u model.Usage) error {
	if cost(u) > 0.50 {
		return errors.New("over $0.50")
	}
	return nil
}),
```

- **检查时机**：`Requests` 在每次模型调用前检查，`ToolCalls` 在每批工具调用
  前检查，token 限制和预算两处都检查。会超出 `ToolCalls` 的一批调用不会开始。
- **计数**：`Usage.Requests` 和 `Usage.ToolCalls` 统计模型调用和已执行的工具
  调用。子 Agent 返回后，它的用量计入父 Agent 的限制。
- **结束**：运行以 `limit_exceeded` 状态结束，`Result.Reason` 指明是哪项
  限制。没执行的调用会补上说明，越过 token 限制的最终回答会保留。和
  `max_turns` 一样，这个状态是最终状态：用新的运行继续对话。

### 停止条件

`agent.WithStopAtTools("submit")` 在 `submit` 工具返回非错误结果后，以完成
状态结束运行。该结果就是运行的 `Output`，不会再调用模型。`agent.WithStopWhen`
接受条件函数，在每个 `loop.Step` 的工具调用执行后检查，例如
`func(s *loop.Step) bool { return s.Turn >= 5 }`；这时输出是模型最后一条
消息的文本。如果调用方在工具结果之后停止读取，恢复运行时会以同样方式结束，
不会调用模型。

### Hooks

Hooks 沿用 Claude Code 的生命周期事件：

| 事件 | 用途 |
| --- | --- |
| `SessionStart` | 会话开始时补充上下文：`startup`、`resume`、`clear`、`fork`（通过 `Runner`），或 `compact` 压缩之后 |
| `SessionEnd` | `Runner.EndSession` 结束会话时做清理；不能拦截 |
| `UserPromptSubmit` | 在用户输入交给模型前检查它，或给它附加上下文 |
| `PreModel` / `PostModel` | 修改模型请求，或查看模型响应 |
| `PreToolUse` | 在工具调用前允许、拒绝、请求审批或推迟，也可以改写参数 |
| `PostToolUse` | 替换成功的工具调用的结果，或在结果后追加内容 |
| `PostToolUseFailure` | 在失败的工具调用的结果后追加内容 |
| `Stop` / `SubagentStop` | 拦截停止，让 Agent 或子 Agent 继续工作（见[多 Agent](#多-agent)） |
| `SubagentStart` | 子 Agent 开始任务前为任务补充上下文 |
| `PreCompact` / `PostCompact` | 跳过自动压缩，或查看压缩摘要 |
| `Notification` | 运行因等待工具审批而中断时收到通知 |

matcher 遵循 Claude Code 的规则：

- matcher 为空或为 `*` 时匹配所有情况。
- 只含字母、数字、`_`、`-`、空格、`,` 和 `|` 的 matcher 是精确名称列表，
  例如 `Edit|Write` 或 `Edit, Write`。
- 其他 matcher 是 Go 正则表达式，可以匹配任意位置：`^Edit$` 只匹配
  `Edit`，`mcp__github__.*` 匹配 `github` MCP 服务器的所有工具。

工具事件匹配工具名。Claude Code 的内置工具名也会匹配作用相同的 agentx 工具：
`Bash`、`Read`、`Write`、`Edit` 和 `MultiEdit`、`Glob`、`Grep`、`WebFetch`
以及 `TodoWrite` 分别匹配 `bash`、`read_file`、`write_file`、`edit_file`、
`glob`、`grep`、`web_fetch` 和 `todo_write`，因此 `Edit|Write` 会匹配
`edit_file` 和 `write_file`。`Edit` 和 `Write` 也会匹配 `apply_patch`；
`BashOutput` 和 `TaskOutput` 匹配 `bash_output`，`KillShell` 和 `TaskStop`
匹配 `kill_shell`。
其他事件匹配 `Event.Reason`：`SessionStart` 为来源，
`SessionEnd` 为原因（`clear`、`logout`、`prompt_input_exit`、`other` 等），
`SubagentStart` 和 `SubagentStop` 为子 Agent 的名称，`PreCompact` 和
`PostCompact` 为 `auto`，`Notification` 为 `approval` 或 `permission_prompt`。

与 Claude Code 一样，匹配的 Hook 并行运行（`Prompt` 和 `Agent` Hook 同时最多
12 个；`hook.Parallelism(n)` 可限制全部 Hook），相同的处理器只运行一次。结果
按注册顺序合并：以最严格的决定为准（依次为拒绝、推迟、请求审批、允许），上下文
累加，以最后一个 `UpdatedInput` 为准。`PreModel` Hook 会修改请求，因此逐个运行。
`Stop` 和 `SubagentStop` Hook 最多连续 8 次让 Agent 继续工作
（`hook.StopBlockCap(n)`，0 表示不限制）；出现工具调用的轮次会重置计数。被
Hook 或审批拒绝的调用既不触发 `PostToolUse`，也不触发 `PostToolUseFailure`。

`Runner` 对每个会话触发一次 `SessionStart`：新会话为 `startup`，首次运行或恢复
已有会话为 `resume`，`Runner.Clear` 和 `Runner.Fork` 创建的会话为 `clear` 或
`fork`。`Runner.EndSession(ctx, id, reason)` 在 1.5 秒预算内触发 `SessionEnd`
（`hook.SessionEndTimeout`）。自动压缩后会以 `compact` 触发 `SessionStart`，其
上下文跟在摘要之后。
`WithHooks` 会复制传入的 manager，之后再往该 manager 添加的 Hook 不会影响
Agent。

```go
agent.WithHook(hook.PreToolUse, "bash", hook.Func(
	func(ctx context.Context, ev *hook.Event) (*hook.Result, error) {
		if strings.Contains(string(ev.ToolCall.Arguments), "rm -rf") {
			return &hook.Result{Decision: hook.Deny, Reason: "危险命令"}, nil
		}
		return nil, nil
	}))
```

`hook.Command` 把命令作为 Hook 运行，用法与 Claude Code 的命令 Hook 相同：

- 事件以 JSON 形式写入命令的 stdin，包含 Claude Code 的字段，例如 `cwd`、
  `tool_input`、`source`、`trigger` 和 `last_assistant_message`。
  `tool_name` 和 `tool_input` 使用 agentx 的值，`tool_response` 为
  `{"content": ..., "is_error": ...}`。
- `CLAUDE_PROJECT_DIR` 设为 Hook 的 `Dir`，未设置时为当前工作目录。设置了
  `Args` 时，命令不经 Shell 直接运行，命令和 `Args` 中的
  `${CLAUDE_PROJECT_DIR}` 会被替换。
- 退出码为 2 时拒绝该操作，stderr 作为理由。
- stdout 输出 JSON 时，可以返回决定、补充上下文、改写后的参数或
  `updatedToolOutput`。`systemMessage` 会作为警告写入日志，`defer` 与 `ask`
  一样请求审批。
- 超时默认为 10 分钟，`UserPromptSubmit` 为 30 秒，`SessionEnd` 为 1.5 秒。
- 命令留在后台的进程，在 Unix 上会在命令退出后被结束；在 Windows 上会继续
  运行，除非 Hook 超时或被取消。

Claude Code 的其他处理器类型也都是 Hook：

- `hook.HTTP` 把同样的 JSON POST 到 URL。`Headers` 中的 `$VAR` 只展开
  `AllowedEnvVars` 中的变量。2xx 的 JSON 响应体按命令输出解析；其他响应体、
  非 2xx 状态和请求失败都是非阻塞错误。
- `hook.Prompt` 把 `$ARGUMENTS` 替换为事件 JSON 后询问 `model.Model`；
  `{"ok": false, "reason": ...}` 按 Claude Code 的各事件规则拦截
  （`ContinueOnBlock`、`impossible`）。默认超时 30 秒。
- `hook.Agent` 把提示交给 `hook.Verifier`，例如
  `agent.HookVerifier(m, agent.WithTools(builtin.ReadOnlyTools(cfg)...))`，
  它可使用工具，最多 50 轮。默认超时 60 秒。
- `hook.MCPTool` 通过 `hook.MCPCaller`（例如 `mcp.HookCaller(clients)`）调用
  MCP 工具，输入支持 `${tool_input.file_path}` 这类替换。
- `hook.Async` 在后台运行 Hook。其 `additionalContext` 和 `systemMessage` 随
  下一个工具结果或下一次运行的输入交给模型；设置 `Rewake` 时，退出码 2 还会
  调用 `hook.OnAsyncResult`，以便唤醒会话。`Manager.Close` 会取消它们。

`hook.LoadSettings` 读取 Claude Code 设置文件中的 `hooks` 配置，现有的 Hook
配置无需修改即可使用：

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
	log.Print(w) // agentx 跳过的 Hook
}
a := agent.MustNew("assistant", agent.WithModel(m), agent.WithHooks(hooks))
```

不存在的文件会被跳过，多个文件中相同的 Hook 只运行一次，`disableAllHooks`
会生效，`onFailure: "block"` 会让失败的 Hook 拒绝该操作，`async`/`asyncRewake`
命令在后台运行。`command` 和 `http` 处理器总会运行；`prompt`、`agent` 和
`mcp_tool` 处理器需要 `hook.SettingsOptions{Model, Verifier, MCP}.Load(...)`。
`CLAUDE_CODE_STOP_HOOK_BLOCK_CAP` 和 `CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS`
会生效。缺少依赖的处理器、Claude Code 中该事件不支持的类型、带 `if` 或使用
PowerShell 的 Hook，以及 agentx 不会触发的事件都会被跳过并给出警告。

### 工具审批与可恢复运行

工具可以用 `tool.RequireApproval()` 标记，Hook 也可以返回 `hook.Ask`。实现
`tool.CallApprover` 的工具按每次调用的参数决定，沙箱中的 `bash` 就是如此。
两种情况下，工具调用都要先经过审批才会执行：

- **配置了 Approver**：如果 Agent 配置了 `Approver`，由它当场决定，适合
  交互式终端。
- **没有配置**：运行以 `interrupted` 状态停下，并通过 `Checkpointer` 保存
  状态。拿到审批结果后可以恢复运行。示例用 agentx-contrib 的 `filestore`
  模块把检查点和会话保存在文件中；`loop.NewMemoryCheckpointer` 和
  `session.NewMemoryStore` 则保存在内存里：

```go
cp, _ := filestore.Checkpoints(".agentx/checkpoints")
store, _ := filestore.Sessions(".agentx/sessions")
a := agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy),
	agent.WithCheckpointer(cp))
runner := agent.NewRunner(a, store)

res, _ := runner.RunText(ctx, "s1", "部署最新构建")
if res.Status == loop.StatusInterrupted {
	approvals := map[string]loop.Approval{}
	for _, req := range res.Interrupt.Requests {
		approvals[req.Call.ID] = loop.Approve // 或 loop.Deny
	}
	for ev, err := range runner.Resume(ctx, "s1", approvals) {
		// ...
	}
}
```

失败的运行（例如 Provider 故障时）和进程中途退出的运行也可以恢复：
`Status.Resumable` 为真的状态，`Runtime.Resume` 和 `LLMAgent.Resume` 都会
接受。`runner.Resume` 恢复会话中被中断的运行，没有的话就恢复最近一次失败的
运行（见 `agent.FailedRunKey`）。进程崩溃的运行要用 Agent 的 `Resume` 按
run ID 恢复，这需要 `Checkpointer`。没有配置 `Checkpointer` 时，Runner 用
内存中保存的状态恢复被中断的运行，进程重启后这些状态会丢失，失败的运行也无法
恢复。

- **提前停止**：停止读取运行的事件后，运行会在下一步之前停下。配置了
  `Checkpointer` 时，可以从停下的地方恢复；如果停在模型给出最终回答之后，
  恢复时直接处理这个回答，不会再次调用模型。`AfterRun` 中间件看到的
  `Result.Reason` 说明这次运行能否恢复。在第一个事件处就停止时什么都不会
  执行，被恢复的运行的检查点也保持原样。
- **恢复时的审批**：传给 `Resume` 的审批结果只回答正在恢复的这次中断里的
  请求。恢复未被中断的运行时会忽略它们，之后产生的请求交给 `Approver`，或者
  再次中断运行。
- **工具调用不会重复执行**：每批工具调用执行前，状态和已经拿到的结果会一起
  写入检查点。已经完成的调用保留结果；被打断的只读调用会重新执行；被打断的
  有副作用的调用不会再执行一次，而是告诉模型它可能已经生效。
- **放弃运行**：`runner.Discard(ctx, "s1")` 放弃会话中被中断或失败的运行，
  例如检查点已经丢失时。待处理的工具调用会被标记为未执行，会话可以继续。
- **状态**：最终回答因为输出 token 上限被截断，或者被内容过滤拦下时，运行以
  `incomplete` 状态结束。`Result.FinishReason` 区分这两种情况，也能看出模型
  是否拒答。
- **参数被修改**：模型发出的工具调用保持原样。Hook 或审批修改了参数时，工具
  结果里会告诉模型实际用的是哪些参数。

### 权限

`permission` 包按照 Claude Code 的
[权限规则](https://code.claude.com/docs/en/permissions)和
[权限模式](https://code.claude.com/docs/en/permission-modes)决定工具调用。
可以加载 Claude Code 设置文件中的 `permissions` 块，也可以自己写规则：

```go
s, warnings, _ := permission.LoadSettings(
	filepath.Join(home, ".claude/settings.json"), ".claude/settings.json")
cfg := s.Config(dir) // allow、ask、deny、defaultMode、additionalDirectories
cfg.Rules = append(cfg.Rules, permission.Rules{
	Allow: []string{"Bash(npm run *)", "Edit(src/**)", "WebFetch(domain:go.dev)"},
	Deny:  []string{"Read(./.env)", "Bash(git push *)", "mcp__github__*"},
})
p, _ := permission.New(cfg)
a := agent.MustNew("dev", agent.WithModel(m),
	agent.WithTools(builtin.Tools(builtin.Config{Dir: dir, CheckRedirect: p.CheckRedirect})...),
	agent.WithPermissions(p))
```

- **规则**：deny 优先于 ask，ask 优先于 allow。`Bash`、`Read`、`Edit` 和
  `WebFetch` 选中承担相应职责的内置工具；Edit 规则会检查 `apply_patch`
  调用涉及的每个文件。Bash 规则逐条匹配复合命令中的
  每个命令，包括子 Shell 和 `$(...)` 里的命令。Read 和 Edit 规则是
  gitignore 模式，以 `//`、`~/`、`/` 或 `./` 锚定。`Tool(param:value)`
  形式的 deny 和 ask 规则匹配其他参数，例如
  `Bash(dangerously_disable_sandbox:true)` 会在每条离开沙箱的命令前询问。
- **递归搜索**：使用 `WithPermissions` 时，glob 和 grep 会跳过路径规则
  禁止或需要审批的后代文件，即使设置了 `include_ignored`。显式指定文件
  路径可以请求该文件的审批。受限目录内的路径排除例外仍然生效。
- **模式**：`default` 把未决定的调用交给工具自身，但直接运行路径都在工作
  目录内的 `ls`、`git status` 等只读命令，`cat ~/.ssh/id_rsa` 则需要询问。`acceptEdits` 还允许工作目录内的编辑，`plan`
  拒绝一切非只读调用，`dontAsk` 拒绝所有需要询问的调用，
  `bypassPermissions` 除 deny 和 ask 规则外全部允许。
  `agent.RunValue(permission.ModeKey, "plan")` 设置单次运行的模式；
  中间件可以调用 `p.SetMode`。同一 Agent 的 Hook 会以 `permission_mode`
  收到当前模式。
- **plan 模式与 Agent**：Claude Code 运行 `BashOutput`、`KillShell` 和
  `TodoWrite` 无需权限；与之一致，`plan` 允许只改变 Agent 自身状态的
  `bash_output`、`kill_shell` 和 `todo_write`。handoff 工具是只读的，因此
  规划中的 Agent 可以 handoff；模式保存在运行值 `permission.mode` 中，
  handoff 会带上它，所以接手的 Agent 只要有权限策略，也会处于规划模式。
  子 Agent 工具不是只读的，仍然被拒绝。
- **始终允许**：用 `loop.Approval{Approved: true, Remember:
  loop.RememberSession}` 审批后，同类调用在本会话内不再需要审批
  （`RememberRun`：在本次运行内）。Runner 把这些授权随会话保存。
- **局限**：Shell 词法分析器不是完整的 Shell 解析器：无法拆分的命令
  永远不会被自动批准；规则匹配的是命令文本，而不是通过路径或
  `sh -c` 运行的程序。需要安全边界时请使用沙箱。

### Session

`agent.Runner` 把对话保存在 `session.Store` 中，在同一个 Runner 内，同一会话
的多次运行会依次执行。可用的存储有三种：

- `session.NewMemoryStore()`：保存在内存里。
- agentx-contrib 的 `filestore.Sessions(dir)`：每个会话原子写入一个 JSON 文件。
- agentx-contrib 的 `sqlstore.Sessions(db)`：保存在 SQL 数据库中，见
  [文件与 SQL 存储](#文件与-sql-存储)。

三种存储都能发现并发修改：如果其他 Runner 或进程改动了该会话，`Save` 会返回
`session.ErrConflict`。请先重新加载，再决定如何恢复。保存失败不会撤销已经完成的
模型请求或工具副作用，因此不要自动重跑整个运行。

不同用户或会话应使用独立的内置工具实例：工作目录、文件读取记录和待办列表都是
可变状态。工具的所有者退出时调用 `builtin.Close`。跨运行共享的自定义模型、
工具、Hook 和中间件必须支持并发调用，并响应 context 取消。

生成的标识符使用标准库 `uuid` 包。`session.NewID` 生成的会话 ID 是随机的
第 4 版 UUID，Runner 和服务端在会话没有 ID 时使用它。运行、工作流运行、A2A
任务和消息、记忆条目以及回退的 ID 是带短前缀（例如 `run_`）的第 7 版 UUID，
因此按创建时间排序。

运行结束时会保存会话，即使调用方的 context 已经取消也会保存。调用方提前停止
读取事件时，运行会被取消，结束后同样会保存会话；没执行完的工具调用会补上结果，
对话保持完整。

在 Runner 的运行中，工具、Hook、中间件和指令函数可以通过
`agent.SessionState(ctx)` 读写会话状态。运行结束时状态随会话一起保存，运行
失败或被中断时也一样；子 Agent 共享同一个状态。请存放 JSON 值，再用
`session.State.Value[T]` 按类型读回；以 `agentx.` 开头的键归 Runner 使用。不使用
Runner 时，可以用 `agent.ContextWithSessionState` 提供状态。中间件可以用
`loop.Run.EncodeValue` 和 `loop.Run.DecodeValue[T]` 保存带类型的运行状态，它们以 JSON
形式存进运行值，因此能跨检查点保留。

### 指令与依赖

- **动态指令。** `WithInstructionsFunc(f)` 把 `f` 返回的文本追加到静态指令
  后面。它每次运行只执行一次，系统提示词因此可以被缓存，恢复的运行也沿用它；
  `agent.EveryTurn()` 改为在每次模型调用前执行。返回错误会让运行失败。
- **模板。** `WithInstructionTemplate()` 在运行开始时用会话状态填充静态指令
  中的 `{key}`，与 Google ADK 相同：字符串原样插入，其他值以 JSON 插入。缺少
  `{key}` 会让运行失败，缺少 `{key?}` 时替换为空。只有名称（字母或 `_` 开头，
  后接字母、数字和 `_ . : -`）才是占位符，所以 JSON 保持原样；`{{` 和 `}}`
  表示字面的大括号。函数输出不做模板替换，需要时在函数中调用
  `agent.RenderTemplate`。
- **依赖。** 数据库连接、当前用户等带类型的值通过 context 传递：
  `loop.WithDeps(ctx, v)`，`agent.WithDeps(v)` 作为默认值，
  `agent.RunDeps(v)` 用于 Runner 的单次运行。工具、Hook、中间件和指令函数
  用 `loop.Deps[T](ctx)` 读取。它们不会发送给模型，也不会保存在检查点中：
  恢复运行时需要再次传入，例如
  `runner.Resume(ctx, id, approvals, agent.RunDeps(db))`。

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

### 检查点与回退

与 Claude Code 的[检查点](https://code.claude.com/docs/en/checkpointing)一样，
`Runner` 在每次运行的用户消息之前记录一个检查点。`Runner.Checkpoints` 列出
检查点，`Runner.Rewind` 把会话回退到其中一个：`agent.RewindCode` 恢复文件，
`RewindConversation` 删除该消息及其后的全部内容，并返回该消息以便重新发送，
`RewindBoth` 两者都做。返回的是调用方的原始输入，不含上下文提供者或中间件
追加的提示。检查点的 ID 就是对应运行的 ID，运行的事件里都带有它。

```go
hist, err := builtin.NewFileHistory(builtin.FileHistoryConfig{Dir: ".agentx/history"})
tools := builtin.Tools(builtin.Config{Dir: ".", History: hist})
a := agent.MustNew("coder", agent.WithModel(m), agent.WithTools(tools...),
	agent.WithFileHistory(hist))
runner := agent.NewRunner(a, store)

cps, err := runner.Checkpoints(ctx, id)
res, err := runner.Rewind(ctx, id, cps[len(cps)-1].ID, agent.RewindBoth)
// res.Message 是被删除的提示词；res.Restored 列出恢复的文件。
```

- **记录范围**：`write_file`、`edit_file` 和 `apply_patch` 修改文件之前会
  保存它的状态：按 SHA-256 存储的内容、权限和符号链接目标，新建、删除和移动
  同样记录。保存失败时工具不做任何修改。子 Agent 的编辑计入调用它的会话。与
  Claude Code 一样，`bash`、其他工具或 Agent 之外的改动不会被记录。
- **恢复**：回退代码时，自检查点以来改动过的每个文件都恢复到检查点之后第一次
  改动前的状态，像 `apply_patch` 一样经过 `os.Root`，要么全部成功，要么都不
  生效。被删除的符号链接不会重建，大于 `MaxFileSize` 的文件列在 `Skipped` 中。
  模型必须重新读取恢复后的文件才能修改它们。回退前的文件状态保存为检查点
  `RewindResult.Undo`，用 `History.Restore` 恢复它即可撤销回退。
- **规则**：会话运行期间 `Rewind` 返回 `ErrSessionBusy`；会话有中断的运行时
  拒绝回退，直到该运行被恢复或丢弃；失败的运行会被放弃。压缩删掉检查点的消息
  之后，只能回退到该检查点的代码。新检查点通过独立于内容的标识定位消息，
  重复提问不会误匹配已被压缩的旧检查点。没有稳定消息标识的旧记录不能回退
  对话，代码回退不受影响。
- **会话状态**：回退对话会让检查点时的当前 Agent（`Checkpoint.Agent`）重新
  成为当前 Agent，撤销之后的 Handoff。其他会话状态，例如 `agent.SessionState`
  的值和记住的权限授权，不会回退；Claude Code 回退时同样保留权限。
- **存储与保留**：`FileHistory` 把每份内容只存一次，放在 `objects/` 下，每个
  会话的检查点放在 `sessions/<id>.json`，权限为 0600。与 Claude Code 的默认
  行为一样，它保留每个会话最近 100 个检查点，并在最后一次改动 30 天后删除该
  会话的历史；此外每个会话最多保留 1 GiB，只保存不超过 16 MiB 的文件。
  `FileHistoryConfig` 可以调整这些限制。
- **分叉**：`Runner.Fork` 会复制检查点和历史，两个会话都能回退到分叉前的
  检查点，之后的改动只记录在做出改动的会话中。两个会话仍然操作同一份文件。

### 上下文工程

`actx` 包用 Provider 拼装系统提示，并为每条新输入拼装提醒内容；Provider
在每次运行开始前执行一次。它还会跟踪上下文窗口，超过阈值后自动压缩对话。

```go
import "github.com/nostosx/agentx/actx"

agent.WithContext(actx.Config{
	Window: actx.Window{Limit: 200_000, ReserveOutput: 16_000},
	Strategy: actx.Chain(
		actx.ClearToolResults{KeepRecent: 3},   // 先清理较早的工具结果
		actx.ClearToolArguments{KeepRecent: 3}, // 再清理较早的大段工具参数
		actx.Summarize{Model: m},               // 其次总结较早的轮次
		actx.Truncate{},                        // 最后丢弃最早的轮次
	),
}),
agent.WithContextProvider(actx.Static("rules", "始终用中文回答。", actx.PlaceSystem)),
```

Section 有两种放置位置：

- **`PlaceSystem`**：把稳定的内容追加到系统提示，系统提示的前缀保持不变，
  可以被缓存。
- **`PlaceReminder`**：把每次请求都可能变化的内容放进当前用户输入的
  `<system-reminder>` 块中。

`actx.InstructionFiles` 按 Codex 和 Claude Code 的方式加载项目指令文件
（`agent.WithInstructionFiles(dir)` 使用它的默认配置）：

```go
agent.WithContextProvider(actx.InstructionFiles(actx.InstructionConfig{
	Dir:       ".",                             // 默认值
	UserFiles: []string{"~/.codex/AGENTS.md"}, // 可选的个人文件，最先读取
}))
```

- 项目根目录是 `Dir` 最近的、包含 `RootMarkers` 中某一项（默认 `.git`）的
  祖先目录。从根目录到 `Dir` 的每一层目录，加载 `Names` 中第一个存在的文件
  （默认依次为 `AGENTS.override.md`、`AGENTS.md`、`CLAUDE.md`），再加载
  `LocalNames` 中存在的文件（默认没有，例如 `CLAUDE.local.md`）。找不到根目录
  时只搜索 `Dir`。
- 每个文件成为一个 `PlaceSystem` Section，并标注它的路径；每次运行都会重新
  读取文件。
- 代码片段和围栏代码块之外的 `@path` 导入会展开在导入它的文件之后，路径相对
  于该文件，最多 `ImportDepth` 跳（默认 4），每个文件只加载一次。
- 这些文件不可信：它们及其导入只能读取项目根目录内的文件（用户文件的导入只能
  读取它所在目录内的文件，二者都还可以读取 `ImportDirs` 内的文件），通过 `..`
  或符号链接离开这些目录的路径会被跳过。
- `MaxBytes` 限制总大小（默认 32 KiB），超出部分会被丢弃并附上一条说明。

没有设置 `Strategy` 时，压缩依次使用 `ClearToolResults`、
`ClearToolArguments` 和 `Truncate`。压缩会保留运行正在处理的任务：

- 运行中途追加的提醒（例如 Stop Hook 让 Agent 继续时附加的内容）留在它所
  延续的那一轮里。
- 进行中的这一轮保留带签名的推理内容，Anthropic 的扩展思考因此仍然有效。
- 模型还没看到的工具结果永远不会被清理，所以单独一批过大的结果可能超出窗口，
  此时中间件会记录一条警告。可以限制工具输出的大小，例如使用
  `tool.TruncateOutput`。

在启用思考前缀检查的模型上（Claude Fable 5.1、Opus 5.5、Sonnet 5.5 和
Haiku 5.5），编辑较早的历史会让其后回放的 Anthropic 思考失效。
agentx-contrib 的 Anthropic Provider 会让 API 丢弃这些块，请求因此成功，模型
会重新思考。

### Skills

Skills 使用与 Claude Code 相同的 `SKILL.md` 格式：开头是 YAML front matter，
包含 `description` 和可选的 `name`（默认为目录名），后面是 Markdown 正文。
模型一开始只看到名称和描述，需要时再通过工具加载全文。这就是渐进式披露：全文
只在模型用到时才进入上下文。

- **Front matter**：Claude Code
  [frontmatter 参考](https://code.claude.com/docs/en/skills#frontmatter-reference)
  中的所有字段都在 `skill.Metadata` 中有类型化字段，包括 Agent Skills 的
  `license`、`compatibility` 和 `metadata`；未知键保留在 `Extra` 中。
  YAML 子集支持嵌套的块式与流式映射和列表、跨行的引号标量、块标量和注释。
- **名称**：`Validate` 遵循 Claude Code，也接受 `Release Notes` 这类名称；
  加载选项 `skill.Strict()` 执行
  [Agent Skills 规范](https://agentskills.io/specification)。
- **发现**：`skill.Discover` 从工作目录到项目根目录加载 `.claude/skills` 和
  `.agents/skills`。托管目录、主目录（需显式开启）、附加目录和嵌套目录均为
  可选。优先级高者胜出：托管、用户、项目，然后是附加目录；冲突会被报告。
- **调用**：`skill` 工具接受 `arguments`，并展开 `$ARGUMENTS`、`$N`、
  `$name`（来自 `arguments`）、`${CLAUDE_SKILL_DIR}`、`${CLAUDE_SESSION_ID}`
  等替换。
  `` !`command` `` 注入只在 `skill.Config{Shell: true}` 时运行，因为它会执行
  Skill 文件中写的 shell 命令；否则文本保持原样。
- **行为**：`disable-model-invocation` 的 Skill 对模型隐藏；用户通过
  `agent.RunSkill` 或 `Registry.Invoke` 调用它们。在本次运行结束前，
  `allowed-tools` 免审批，`disallowed-tools` 被移除，`model` 和 `effort`
  生效。`context: fork` 在子 Agent 中运行 Skill：默认为 `general-purpose`，
  或传给 `WithSkillAgents` 的 Agent。默认子 Agent 使用父 Agent 的工具，并受父 Agent 的
  权限策略和工具 Guardrail 约束。Skill 的 `hooks` 在整个会话中有效，
  带 `paths` 的 Skill 在读取或编辑匹配文件后出现。
- **基础目录**：从磁盘加载的 Skill 会把所在目录告诉模型，方便它找到附带的
  文件；嵌入的 Skill 没有基础目录。

```go
//go:embed skills
var skillFS embed.FS

skills, err := skill.LoadFS(skillFS, "skills", "")
if err != nil {
	log.Fatal(err)
}
found, err := skill.Discover(skill.DiscoverConfig{}) // 项目 Skills
if err != nil {
	log.Print(err) // 加载失败的 Skill；其余的在 found 中
}
a := agent.MustNew("dev", agent.WithModel(m),
	agent.WithSkills(append(skills, found.Skills...)...))
res, err := agent.NewRunner(a, nil).RunText(ctx, "", "ship it", agent.RunSkill("deploy", "staging"))
```

### 长期记忆

`memory` 包跨会话保存信息。默认按关键词检索；配置 `Embedder` 后，
`memory.Hybrid` 会把关键词检索和向量检索的结果合并。某个检索器失败时，它使用
其余检索器的结果，并用 `slog.Default` 记录错误，除非设置了 `OnError`。
`memory.NewMemoryStore` 把条目保存在内存里；agentx-contrib 的
`filestore.Memory` 和 `sqlstore.Memory` 将其持久化。

```go
store, _ := filestore.Memory(".agentx/memory.json")
mem := memory.New(store)
// 每次输入召回 3 条记忆，作用域为 user-42。
a := agent.MustNew("assistant", agent.WithModel(m), agent.WithMemory(mem, "user-42", 3))
```

默认还会给模型提供记住、召回和遗忘信息的工具（`memory_remember`、
`memory_recall` 和 `memory_forget`），可以用 `agent.WithoutMemoryTools()`
隐藏它们。

`WithMemory` 让 agent 的每次运行都使用同一个 scope。服务多个用户的 agent
（例如放在 `server.New` 后面）必须按运行选择 scope：
`agent.WithMemoryScopeFunc(mem, scope, 3)` 会在每次召回和每次记忆工具调用时，
用启动运行时的 context 调用 `scope(ctx)`，比如 HTTP 中间件放入认证用户的请求
context。它返回错误时该调用失败，因此不知道用户是谁的运行不会读写任何记忆。

agentx-contrib 的 Provider 自带 Embedder：`openai.NewEmbedder`（`/embeddings`，
也适用于 `openai.Ollama()` 等兼容服务）和 `gemini.NewEmbedder`
（`batchEmbedContents`）。agentx-contrib 的 `retry.Embedder` 像重试模型调用一样
重试它们。Gemini 可以为查询和存储的条目使用不同的任务类型：

```go
docs, _ := gemini.NewEmbedder(gemini.WithTaskType(gemini.TaskRetrievalDocument))
queries, _ := gemini.NewEmbedder(gemini.WithTaskType(gemini.TaskRetrievalQuery))
policy := retry.Policy{}
mem := memory.New(store,
	memory.WithEmbedder(retry.Embedder(policy, docs)),
	memory.WithQueryEmbedder(retry.Embedder(policy, queries)))
```

### 检索增强生成

`rag` 包加载文档、切分成块、为块生成向量并建立索引，再把索引作为搜索工具
和每次运行的自动检索接入 Agent。

```go
docs, _ := (&rag.FSLoader{FS: os.DirFS("docs"), Include: []string{"**/*.md"}, GitIgnore: true}).Load(ctx)
emb, _ := openai.NewEmbedder()
store, _ := filestore.Vectors(".agentx/rag")
ix := rag.NewIndex(rag.Config{Store: store, Embedder: emb})
// 只为新增和变化的来源生成向量，并删除已移除的来源。
if _, err := ix.Sync(ctx, docs); err != nil {
	log.Fatal(err)
}
a := agent.MustNew("assistant", agent.WithModel(m),
	agent.WithTools(rag.Tool(ix, rag.ToolOptions{Description: "Search the product docs."})),
	agent.WithContextProvider(rag.Provider(ix, 3)))
```

- **加载器**：`FSLoader` 读取任意 `fs.FS`，支持 glob 过滤和可选的
  `.gitignore` 规则；`URLLoader` 抓取 URL。解析器支持文本、Markdown、HTML
  （使用 `web_fetch` 的转换器）、JSON、JSON Lines 和 CSV。PDF 需要标准库以外
  的库：实现 `rag.Parser` 并注册到 `FSLoader.Parsers`。
- **切分器**：大小按 Token 计算，默认使用 `actx` 包的估算，也可以传入任意
  `LengthFunc`。`RecursiveSplitter` 按分隔符切分并支持重叠，
  `MarkdownSplitter` 把标题路径保存在 `MetaHeaders`，`CodeSplitter` 在 Go、
  Python 和 JavaScript 的声明处切分，默认的 `AutoSplitter` 为每个文档选择其中
  之一。块 ID 是来源、文档和偏移量的哈希，因此保持稳定。
- **索引**：`Index` 批量生成块的向量并存入 `VectorStore`：
  `rag.NewMemoryStore()`，或 agentx-contrib 中每个来源一个 JSON 文件的
  `filestore.Vectors`。每个来源都有内容哈希，`Sync` 据此跳过
  未变化的来源；更换切分器或 Embedder 后要修改 `Config.Version`。
- **检索**：默认用 `Hybrid` 以倒数排名融合合并 BM25（`Keyword`）和余弦相似度
  （`Vector`）的结果。`Config.Reranker` 增加第二阶段，例如 `LLMReranker`。
  `ContextualEnricher` 实现
  [Contextual Retrieval](https://www.anthropic.com/news/contextual-retrieval)：
  由模型为每个块写一段简短的上下文并缓存，与块一起生成向量和参与检索。
- **Agent**：`rag.Tool` 是只读工具，返回带编号的块及其来源、ID 和分数，输出
  有大小上限；结果元数据中带有引用信息。

### 文件与 SQL 存储

核心模块只在内存中保存状态：`session.NewMemoryStore`、
`loop.NewMemoryCheckpointer`、`workflow.NewMemoryCheckpointer`、
`memory.NewMemoryStore`、`rag.NewMemoryStore` 和 `mcp.NewMemoryTokenStore`。
持久化存储位于 agentx-contrib：

- `filestore` 写入 JSON 文件：`Sessions`、`Checkpoints`、
  `WorkflowCheckpoints`、`Memory`、`Vectors` 和 `MCPTokens`。
- `sqlstore` 通过 `database/sql` 把会话、运行检查点、Workflow 检查点和记忆
  保存在 SQLite、PostgreSQL 或 MySQL 中，驱动由你选择。

```go
sessions, _ := filestore.Sessions(".agentx/sessions")
runner := agent.NewRunner(a, sessions)
```

它们的选项、表结构和测试见
[agentx-contrib README](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#存储)。

一致性测试套件按内置存储遵循的契约检查任意存储：`session/sessiontest`、
`loop/checkpointtest`、`workflow/checkpointtest`、`memory/memorytest` 和
`rag/ragtest`；其中 `ragtest.Embedder` 是供测试使用的确定性 Embedder。

```go
func TestStore(t *testing.T) {
	sessiontest.Run(t, func(t *testing.T) session.Store { return newStore(t) })
}
```

### 多 Agent

把子 Agent 传给 `WithSubAgents`，父 Agent 就能像调用工具一样调用它们。

- **Hook**：子 Agent 开始任务前会触发父 Agent 的 `SubagentStart` Hook，
  Hook 返回的 `AdditionalContext` 会放在任务前面。子 Agent 完成时会触发
  `SubagentStop` Hook，Hook 带理由拒绝或补充上下文时，子 Agent 会继续工作，
  上限与 `Stop` 相同。两者都按子 Agent 的名称匹配，`Event.RunID` 是子 Agent 的运行。
- **用量**：子 Agent 的 token 用量、模型调用和工具调用次数会计入父 Agent
  运行的 `Usage`，因此也受父 Agent 的限制约束。
- **审批**：没有自己 `Approver` 的子 Agent 需要审批时，由父 Agent 的
  `Approver` 决定；父 Agent 也没有配置 Approver 时，父运行会带着子 Agent 的审批请求中断，
  `ApprovalRequest.Agent` 标明是哪个 Agent 发起的。恢复父运行时，子 Agent 会
  带着审批结果一起恢复，子 Agent 不需要自己的检查点存储。
- **事件**：`WithSubAgentEvents()` 会把各层子 Agent 的事件转发到父 Agent 的
  事件流中。转发的事件保留子 Agent 的 `RunID` 和 `Agent`，子 Agent 的事件
  `Depth` 为 1，它的子 Agent 为 2，`ParentCallID` 是启动该子 Agent 的工具调用。
  其中的 `run_end` 表示子 Agent 结束而不是父 Agent 结束，所以按 `Type` 分支时要
  检查 `Depth == 0`。默认关闭。
- **并发**：子 Agent 可能有副作用，默认逐个运行。使用
  `WithConcurrentSubAgents()` 后，同一次模型回复里的子 Agent 调用会并行运行，
  并发数受 `WithParallelism` 限制。同时需要审批的多个子 Agent 会让父运行一起
  中断，并一起恢复。
- **深度**：子 Agent 最多嵌套 `agent.DefaultMaxDepth`（5）层，可以用
  `WithMaxDepth` 修改，子 Agent 之上最小的限制生效。超出限制的调用会向模型
  返回错误。

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

`agent.AsTool` 可以为单个 Agent 构建工具，并指定自己的名称、描述和上述
选项，再通过 `WithTools` 使用。

### Handoff

`WithHandoffs` 让模型把对话交给另一个 Agent，与 OpenAI Agents SDK 和 Google
ADK 相同。每个 Agent 成为一个 `transfer_to_<name>` 工具。模型调用它时，这一轮
的工具调用照常执行，然后运行在另一个 Agent 上继续，使用它自己的指令、工具、
Guardrail 和中间件。

- **同一个运行。** 运行保留 ID、用量和轮数，因此限制覆盖所有 Agent。下一个
  Agent 的事件跟在 `loop.EventHandoff` 之后（`Event.Handoff` 给出 `From`、
  `To` 和对应的调用），`Result.Agent` 是最后回答的 Agent。
- **会话。** Runner 把当前 Agent 保存在会话状态中（`agent.ActiveAgentKey`），
  下一条消息发给它。回退对话会恢复检查点时的当前 Agent。
- **权限。** Handoff 工具是只读的，因此 `plan` 模式允许它们；见
  [权限](#权限)。
- **历史。** 下一个 Agent 看到完整对话和一条关于 Handoff 的说明。过滤器
  （`HandoffInputFilter`）改写的是它的模型看到的内容，不影响会话保存的内容；
  内置 `agent.RemoveToolCalls` 和 `agent.NestHandoffHistory`。
- **选项。** `WithHandoff(a, ...)` 接受 `HandoffName`、`HandoffDescription`、
  `HandoffInputFilter` 和 `HandoffInput[T](fn)`，后者是模型发送的带类型参数，
  由 `fn` 检查。`agent.Ref(name, desc)` 引用之后才构建的 Agent，例如发起
  Handoff 的那个，这样 Agent 之间可以互相交回。`WithMaxHandoffs` 限制每次运行
  的 Handoff 次数（默认 10），每一轮只有第一个 Handoff 调用生效。
- **审批。** 接手运行的 Agent 可以像其他 Agent 一样被中断。`Resume` 在该
  Agent 上继续，通过整个 Agent 图的检查点存储找到它。相互 Handoff 的 Agent
  应共用一个检查点存储。
- **Hook。** 没有专门针对 Handoff 的 Hook 事件。匹配器为 `transfer_to_.*` 的
  `PreToolUse` 和 `PostToolUse` 可以观察并拒绝 Handoff。

```go
billing := agent.MustNew("billing", agent.WithModel(m),
	agent.WithDescription("Invoices and refunds."),
	agent.WithHandoffs(agent.Ref("triage", "Anything else.")))
triage := agent.MustNew("triage", agent.WithModel(m),
	agent.WithHandoff(billing, agent.HandoffInputFilter(agent.RemoveToolCalls)))
res, err := agent.NewRunner(triage, nil).RunText(ctx, id, "My invoice is wrong")
fmt.Println(res.Agent, res.Output) // billing ...
```

### Guardrail

`guard` 包检查输入、最终回答和工具调用。检查返回
`guard.Result{Tripwire, Info, Message}`。Guardrail 触发时，运行以
`StatusBlocked` 结束并设置 `Result.Guardrail`，结果的 `Output` 为空。返回错误
会让运行失败。

- **输入。** `WithInputGuardrail(g)` 在运行开始前、Hook 之前执行。使用
  `guard.Parallel()` 时改为与第一次模型调用同时执行：触发时取消该调用；第一次
  响应会等待所有并行 Guardrail 完成，因此不会先执行任何工具；已完成调用的用量
  仍然计入。尚未通过它们的运行在恢复时会重新执行。接手 Handoff 的 Agent 不执行
  输入 Guardrail。
- **输出。** `WithOutputGuardrail(g)` 在 `Stop` Hook 允许运行结束后检查回答。
  被拦截的回答会被扣留：结果、检查点和会话中以
  `[response withheld by guardrail <name>]` 代替它，下一次运行的对话仍然有效。
  回答会在检查之前随模型生成流式发出，除非使用
  `WithOutputGuardrail(g, guard.HoldOutput())`：它会暂存每个响应的增量和消息，
  直到该响应调用工具（此时照常流式发送）或 Guardrail 通过；暂存的回答在响应
  结束时一次性发出。没有结束运行的暂存回答不会作为输出发出：被 Stop Hook
  要求继续的、被 provider 暂停的、不符合输出 schema 的，以及运行先被停止或
  失败时的回答。`Result.Output`、`OutputError.Output` 和 `output_retry` 都不带
  这些原文；schema 错误只提供通用描述，避免详情引用未经检查的文本。
  草稿仍保留在对话中，包括 `Result.Messages`、`Result.NewMessages` 和检查点，
  用于继续和恢复运行。向用户展示输出时，应把这些对话字段视为内部状态。
- **工具。** `WithToolGuardrail(g)` 在审批前检查调用（`Before`），在执行后、
  追踪、Hook 和事件看到之前检查结果（`After`）。`After` 触发时会替换结果，
  包括其中的媒体和元数据，被拦下的内容既不会发出也不会留在对话中，该调用也
  不会触发 `PostToolUse` Hook。

```go
agent.WithInputGuardrail(guard.InputGuardrail{
	Name: "topic",
	Check: func(ctx context.Context, run *loop.Run, in *model.Message) (guard.Result, error) {
		return guard.Result{Tripwire: isHomework(in.Text()), Message: "Not supported."}, nil
	},
}, guard.Parallel())
```

### Workflow

`workflow` 包把 Agent 和函数组织成图来执行，支持：

- 并行分支
- 汇合节点：等所有前驱分支都完成后才执行
- 条件路由，并可经由条件路由回到前面的节点形成循环
- 用 `Map` 对列表动态扇出
- 按键配置的 reducer，用于合并状态
- 指数退避重试，以及兜底节点
- 持久化、崩溃恢复，以及等待人工处理的挂起
- 流式事件和 Mermaid 渲染
- 用共享状态填充的 `{{key}}` 模板

```go
g := workflow.NewGraph().
	AddNode("draft", workflow.AgentNode(writer, workflow.Template("写一篇关于 {{topic}} 的短文"), "draft")).
	AddNode("review", workflow.AgentNode(reviewer, workflow.Template("{{draft}}"), "final"),
		workflow.Retry(2, time.Second)).
	AddChain(workflow.Start, "draft", "review", workflow.End)

wf, err := g.Compile()
if err != nil {
	log.Fatal(err)
}
st, err := wf.Run(ctx, workflow.NewState(map[string]any{"topic": "Go 迭代器"}))
if err != nil {
	log.Fatal(err)
}
fmt.Println(st.String("final"))
```

**状态合并。** 每个节点在自己的状态视图上工作。节点成功时，它的写入一次性提交；
节点失败或挂起时，写入全部丢弃。图中没有先后关系的两个节点不能设置同一个键，
除非这个键配置了 reducer，否则运行会以 `ConflictError` 失败。`State.Update`
的修改会被重放，不会冲突。

**持久化与恢复。** 配置 `WithCheckpointer` 后，每次节点尝试开始时和每个节点
结束后，运行都会保存为一个 `Checkpoint`。可以使用 `NewMemoryCheckpointer`，或
agentx-contrib 的 `filestore.WorkflowCheckpoints` 或
`sqlstore.WorkflowCheckpoints`。此时状态值必须能编码为 JSON；恢复后的值是普通 JSON 值，
可以用 `State.Value[T]` 按类型读回。`Resume` 可以继续崩溃、失败或挂起的运行：

- 已完成的节点不会再次运行。
- 中断时正在运行的节点会以相同的尝试编号重新运行。因此节点代码至少执行一次，
  但状态写入只生效一次。

检查点保存或清理失败会使运行失败，由 `Run`、`Resume` 和事件流返回，包括挂起和
完成阶段的失败。原有执行错误会与存储错误一起保留；挂起状态未保存成功时，不会
报告为可恢复的挂起。

**人工介入。** 节点调用 `Suspend` 并返回它的错误。运行随后以 `*Suspension`
结束，其中列出 `Pending` 键。为每个键提供一个值来恢复运行。节点会从头再次运行，
这一次 `Suspend` 返回该值。

`AgentNode` 的 Agent 需要工具审批时，也会以同样方式挂起。它的
`Pending.Interrupt` 包含审批请求，用 `map[string]loop.Approval` 恢复。
每次 Agent 运行都有可预测的 ID，即 `SubRunID(run, node, attempt, item)`。
因此崩溃后，配置了 `Checkpointer` 的 Agent 会恢复自己的运行，已完成的工具调用
不会重复执行。

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

**扇出、循环与重试。**

- `Map("docs", body, MapConcurrency(4))` 对列表中的每个元素运行一次 `body`。
  每个元素以 `item` 的形式可见。写入按元素顺序合并，已完成的元素逐个保存。
- 分支可以路由回前面的节点。循环随后从该节点重新开始，`MaxSteps` 限制运行的
  步数，超出时返回 `ErrStepLimit`。
- `WithRetry(RetryPolicy{RetryIf: workflow.Transient})` 提供带抖动的指数退避。
  `OnError("fallback")` 把失败的节点路由到兜底节点。

**事件。** `Stream` 产出节点、状态增量、挂起、Agent 以及 `Emit` 事件。
`Mermaid()` 渲染这张图。

### MCP

`mcp` 包实现了 [Model Context Protocol](https://modelcontextprotocol.io)
客户端和服务端（协议版本 2026-07-28，可回退到 2025-11-25、2025-06-18、
2025-03-26 和 2024-11-05）。客户端：

- **版本协商**：`Connect` 先用 `server/discover` 探测。新版服务端以无状态方式
  使用，每个请求都在 `_meta` 里声明协议版本。若返回其他错误、不含新版错误的
  HTTP 4xx，或在 stdio 上 10 秒内没有应答（`mcp.WithProbeTimeout`），即视为
  旧版服务端：`Connect` 改发 `initialize`，提议 2025-11-25，并接受上述任一
  版本。在 stdio 上，探测的迟到应答仍然有效，因此启动较慢的新版服务端仍按新版
  使用。`mcp.WithProtocolVersion` 可固定使用某个版本。
- **旧版服务端**：在 Streamable HTTP 上，客户端保存会话 ID，在每个请求中连同
  `MCP-Protocol-Version` 一起发送；服务端返回 404 时新建会话（期间发出的请求会
  等待新会话）；`Close` 时发送 `DELETE`。中断的 SSE 流会用 `GET` 加
  `Last-Event-ID` 续传，并遵守服务端的 `retry`，也支持通过关闭流来轮询的服务端。
  缺少相应能力时返回空列表，或返回包装了 `errors.ErrUnsupported` 的错误。
- **客户端功能**：`mcp.WithSampling(handler)`（`mcp.ModelSampler(m)` 用 agentx
  模型采样；`mcp.ApproveSampling` 和 `mcp.ReviewSampling` 让人工检查请求和
  回复）、`mcp.WithElicitation(handler, modes...)`（表单和 URL 两种
  elicitation），以及 `mcp.WithRoots(fn)` 配合 `Client.RootsChanged`。旧版服务端
  以请求的形式发来这些操作；新版服务端返回 `input_required`，由 `Call` 调用
  同样的 handler 后重试。没有 handler 时，旧版服务端收到 method not found，
  新版服务端则得到 `*mcp.InputRequiredError`。
- **日志与 ping**：`mcp.WithLogHandler`、`mcp.WithLogLevel` 和
  `Client.SetLogLevel`（发送 `logging/setLevel`；对新版服务端则在每个请求的
  `_meta` 中携带级别）；`Client.Ping`（新版服务端没有 `ping`，改用
  `server/discover`）。
- **传输方式**：支持 stdio 和 Streamable HTTP。
- **授权**：`mcp.WithOAuth(cfg)` 处理 401：读取 `WWW-Authenticate` 质询，获取
  受保护资源元数据（RFC 9728）和授权服务器元数据（RFC 8414 或 OpenID
  Connect，并校验 issuer），获取客户端 ID（预注册、Client ID Metadata
  Document 或动态客户端注册），再执行带 PKCE S256、`resource`（RFC 8707）和
  `iss` 校验（RFC 9207）的授权码流程。`cfg.Authorize` 把 URL 展示给用户；
  `mcp.NewLoopbackRedirect` 在 127.0.0.1 上接收重定向。令牌会自动刷新，保存在
  `TokenStore` 中（`NewMemoryTokenStore`，或 agentx-contrib 中权限为 0600 的
  `filestore.MCPTokens`）；遇到 403 `insufficient_scope` 时提升 scope。
  `Grant: mcp.GrantClientCredentials` 用密钥或 `mcp.PrivateKeyJWT` 为机器授权。
- **变更通知**：`Listen` 打开 `subscriptions/listen` 流，接收列表和资源的变更。
  对旧版服务端，它改用 HTTP GET 流（stdio 无需额外的流）和
  `resources/subscribe`，并自行确认订阅；这类服务端的通知不做过滤。
- **工具命名**：服务端的工具会转换成名为 `mcp__<server>__<tool>` 的 agentx
  工具。
- **媒体**：工具结果中的图片、音频和内嵌资源 blob 会变成 `tool.Result.Parts`
  （`mcp.ToolResultFrom`），支持媒体的模型可以直接看到；文本和资源链接仍为
  文本。采样 handler 可以接收和返回图片与音频内容。
- **审批与信任**：除非服务端明确提示工具不具破坏性（`destructiveHint: false`），
  否则工具都需要审批；与 MCP 规范一致，没有提示时视为具破坏性。这个提示会被
  直接采信，但 `readOnlyHint` 默认被忽略，传入 `mcp.TrustReadOnlyHint()` 后
  才会采用。不想依赖注解决定审批时，传入 `mcp.RequireApprovalFor(fn)`；
  `RequireApprovalFor(nil)` 会关闭所有工具的审批。
- **工具名冲突**：过长或互相冲突的工具名会加上一个简短的哈希后缀。是否冲突
  取决于服务端列出的其他工具，所以工具列表变化时，这类名称可能随之改变。

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

连接远程服务时使用 `mcp.HTTP("https://example.com/mcp")`：

```go
lr, err := mcp.NewLoopbackRedirect("")
if err != nil {
	log.Fatal(err)
}
defer lr.Close()
cfg := mcp.OAuthConfig{Store: filestore.MCPTokens(tokenPath)}
lr.Configure(&cfg)
c, err := mcp.Connect(ctx, mcp.HTTP("https://example.com/mcp"), mcp.WithOAuth(cfg))
```

`mcp.NewServer(info, opts)` 把 agentx 的工具、资源和提示词提供给上述所有版本的
客户端：

- **工具**：`tool.Registry` 中的工具（`ServerOptions.Registry` 或 `Tools`），
  连同其 schema，通过 `tool.Executor` 执行；`ReadOnly` 变为 `readOnlyHint`，
  `RequiresApproval` 变为 `destructiveHint`。`tool.Result.Parts` 变为图片和
  音频内容；其他媒体（以及 2025-03-26 之前版本中的音频）变为内嵌资源，按 URL
  引用的媒体变为资源链接（2025-06-18 之前版本中为文本）。
  `mcp.AgentTool(a, mcp.AgentToolOptions{})` 把 agent 暴露为工具，并请客户端的用户审批它的工具
  调用。
- **资源与提示词**：`AddResource`、`AddResourceTemplate`（`{var}` 和
  `{+var}`）、`AddPrompt`，或实现 `ResourceSource` 和 `PromptSource`。变更会
  通知 `subscriptions/listen` 流和会话；`ResourceUpdated(uri)` 通知订阅者。
- **Handler**：`mcp.Sample`、`mcp.Elicit` 和 `mcp.ListRoots` 向客户端请求输入：
  旧版客户端收到请求，新版客户端收到 `input_required` 结果，其 `requestState`
  经过签名并绑定到该请求。handler 只需阻塞等待重试；若重试到达共享 `StateKey`
  的另一个实例，handler 会带着之前的回答重新运行。`mcp.Log`、`mcp.Progress` 和
  `mcp.CurrentRequest` 在两代协议中都可用。
- **传输方式**：`srv.ServeStdio(ctx, os.Stdin, os.Stdout)` 和
  `srv.HTTPHandler(opts)`：新版请求无状态，并校验 `MCP-Protocol-Version`、
  `Mcp-Method`、`Mcp-Name` 和 `Mcp-Param-*` 请求头；`initialize` 创建带
  `Mcp-Session-Id` 的会话，提供可续传的 SSE 流（`Last-Event-ID`，可选轮询）、
  GET 流和 `DELETE`。来自回环地址和 `AllowedOrigins` 之外的 Origin 返回 403。
  `ModernOnly` 拒绝旧版本。
- **授权**：`HTTPOptions.Auth` 用你提供的 `Verify` 函数校验 bearer 令牌，返回
  指向受保护资源元数据（由它提供）的 401 和 403 `insufficient_scope` 质询，并把
  会话绑定到令牌的主体。

```go
srv := mcp.NewServer(mcp.Implementation{Name: "files", Version: "1.0"},
	mcp.ServerOptions{Tools: []tool.Tool{readTool, mcp.AgentTool(a, mcp.AgentToolOptions{})}})
http.Handle("/mcp", srv.HTTPHandler(mcp.HTTPOptions{}))
```

### 中间件

核心循环只负责调用模型、执行工具、处理审批和保存检查点，其余功能都以
runtime 中间件的形式接入。Hook 和上下文工程也是中间件，`agent.New` 会自动
装上。中间件需要实现 `loop.Middleware`（只有一个 `Name` 方法），再按需
实现下列阶段：

| 阶段 | 触发时机 |
| --- | --- |
| `WrapContext` | `Run` 或 `Resume` 开始时执行一次；派生出 run 的 context（例如一个 trace Span），调用返回时释放 |
| `BeforeRun` | 新的 run 开始，输入进入对话之前 |
| `BeforeTurn` | 每轮开始，构建模型请求之前 |
| `BeforeModel` / `AfterModel` | 每次调用模型之前和之后 |
| `WrapModel` | 包住每次模型调用，可以跳过这次调用，也可以重试 |
| `BeforeTool` | 每个工具调用：放行、要求审批或拒绝 |
| `RevalidateTool` | 审批修改参数后、执行前重新检查参数策略，不重复执行 Hook |
| `WrapTool` | 包住每次工具执行 |
| `AfterTool` | 每次工具调用之后，可以改写结果 |
| `BeforeFinish` | 模型没有调用工具、准备结束时，可以让 run 继续 |
| `AfterRun` | run 结束时，包括被中断的情况。调用方停止读取事件时，状态为 `running`；如果在第一个事件处就停止，`AfterRun` 不会执行 |

- **执行顺序：** `Before` 阶段按注册顺序执行，`After` 阶段倒序执行；`Wrap`
  阶段先注册的在最外层。
- **错误：** 返回 `loop.Block` 或 `loop.Stop` 时，run 分别以
  blocked 或 stopped 状态结束；返回其他错误时，run 失败。
- **状态：** 用 `run.SetValue` 保存的值会写入检查点，run 被中断后仍然保留。
- **工具：** 实现了 `loop.ToolProvider` 的中间件可以添加自己的工具。在
  `BeforeTool` 中执行参数策略的中间件，还必须实现 `loop.ToolRevalidator`，检查
  审批后修改的参数。权限与工具 Guardrail 已接入。重新校验可以拒绝调用或返回
  错误，但不能改写已批准参数；要求审批的决定已由这次明确审批满足。Hook 和
  未修改参数的审批不会重复执行。

#### 工具调用流水线

`agent.New` 按以下顺序注册中间件：输入 Guardrail、Hook、Skill、按传入顺序排列的
`WithMiddleware` 中间件、`WithPermissions` 权限策略、Handoff、上下文工程，最后是工具与
输出 Guardrail。每次工具调用依次经过：

1. **BeforeTool：** `PreToolUse` Hook、当前 Skill 的允许与禁用工具、`WithMiddleware`
   中间件、权限策略、Handoff 检查和工具输入 Guardrail。无论顺序如何，最严格的决定
   生效：Hook 的允许不能解除权限规则的询问或拒绝。首个拒绝会跳过后续阶段；工具输入
   Guardrail 触发时结束 run。权限策略还会读取此前的决定，这在 `dontAsk` 模式下会影响结果。
2. **审批：** 有阶段要求审批，或没有阶段作出决定且工具需要审批时进行；已允许的调用跳过审批。
3. **RevalidateTool：** 审批修改了参数时执行，Hook 不会重复执行。
4. **执行：** 先按 Schema 校验参数，再经过 `WrapTool` 中间件（先注册的在最外层，
   工具输出 Guardrail 在最内层）、`WithToolMiddleware`（先注册的在最外层），最后调用工具。
5. **AfterTool：** 倒序执行：Guardrail、权限策略、`WithMiddleware` 中间件、Skill，
   最后是 `PostToolUse` Hook。被拒绝的调用不会触发 Hook。

| 用途 | 机制 |
| --- | --- |
| 工具、路径和命令的规则与模式 | `permission.Policy` 配合 `WithPermissions` |
| 由程序外部维护的策略 | 通过 `WithHooks` 配置命令或 HTTP Hook |
| 失败时必须结束 run 的检查 | `WithToolGuardrail` |
| 针对参数的 Go 策略 | 实现 `BeforeTool` 与 `loop.ToolRevalidator` 的 `loop.Middleware` |
| 超时、截断等执行层关注点 | `WithToolMiddleware`；需要访问 run 时用 `loop.ToolWrapper` |
| 哪些调用需要审批以及由谁审批 | `WithApprover`、`tool.CallApprover`、`tool.Info.RequiresApproval` |

`middleware` 包提供链路追踪和指标。日志（`logging.New`）和只重试临时性故障的
指数退避重试（`retry.New`）位于
[agentx-contrib](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#中间件)
的 `middleware` 模块。`middleware` 包通过 `telemetry` 包的 `Tracer` 和 `Meter` 接口记录
链路追踪和指标。`telemetry.ErrorType(err)` 返回链路追踪和指标为某个错误记录的
`error.type` 值，`telemetry.StartSpan` 为当前 run 添加 span，供你自己的中间件使用。

`middleware.Tracing` 和 `middleware.Metrics` 遵循
[OpenTelemetry GenAI 语义约定](https://github.com/open-telemetry/semantic-conventions-genai)。
每次运行有一个 `invoke_agent {agent}` Span，下面挂着每次模型调用的 client
Span（名称形如 `chat gpt-6-astra`）和每次工具调用的 `execute_tool {tool}`
Span；子 Agent 的运行挂在启动它的 `execute_tool` Span 下面。Span 带有
`gen_ai.*` 属性，token 用量里包含缓存和推理 token，还带有 run ID、会话 ID、
Agent 名称和轮次（`invoke_agent` Span 没有轮次）。Provider 通过
`model.Describer` 提供 `gen_ai.provider.name`。

- **其他 Span：** Hook 有 `hook {event}` Span，自动压缩有 `compact_context`
  Span；压缩之后的模型 Span 带有 `gen_ai.conversation.compacted`。
  agentx-contrib 的 `retry` 中间件每重试一次，就在模型调用 Span 上记录一个
  `agentx.retry` 事件。
- **Handoff 与 Guardrail：** 经过 Handoff 的运行中，每个 Agent 有自己的
  `invoke_agent` Span，Handoff 之后的 Span 带有 `agentx.handoff.from`。被
  Guardrail 拦截的运行带有 `agentx.guardrail.name`、`agentx.guardrail.kind`
  属性和一个 `agentx.guardrail` 事件；记录内容时事件还包含 Guardrail 的
  消息。
- **指标：** 模型调用耗时和流式分块耗时、token 用量计数器和单次调用直方图
  （包含缓存和推理 token）、工具调用耗时，以及运行耗时，附带运行状态和本次
  运行自己的模型调用、工具调用次数。`middleware.LegacyMetrics()` 还会记录
  已被取代的 `gen_ai.client.token.usage` 和 `gen_ai.client.operation.duration`。
- **内容：** 系统提示词、消息、工具名称以及工具的参数和结果可能包含敏感数据，
  只有加上 `middleware.RecordContent()` 才会记录。
- **OpenTelemetry：** 核心模块仅依赖标准库和 kitx；
  [agentx-contrib](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#opentelemetry)
  的 `otel` 模块把 OpenTelemetry Provider 适配为 `telemetry.Tracer` 和
  `telemetry.Meter`。接入其他库时，实现这两个接口即可。

把链路追踪和指标注册在重试之前，这样模型调用的 Span 和耗时会覆盖它的
重试：

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

自己写中间件时，可以用 `loop.Funcs`，也可以定义一个实现这些阶段的类型：

```go
redact := &loop.Funcs{
	MiddlewareName: "redact",
	AfterToolFunc: func(_ context.Context, _ *loop.Run, _ *loop.ToolPlan, res *model.ToolResult) error {
		res.Content = apiKey.ReplaceAllString(res.Content, "[redacted]")
		return nil
	},
}
```

有些模型调用不经过 Agent Loop，例如 `actx.Summarize` 策略使用的模型。
这时直接包装模型：`middleware.Wrap(m, middleware.TraceModel(tracer), retry.Model(policy))`。

### 服务化

`server` 包通过 HTTP 提供 `Runner`，只使用标准库：

```go
srv := server.New(runner,
	server.WithAuth(checkToken),       // func(*http.Request) error
	server.WithCheckpointer(cp),       // 重启后仍能报告中断
	server.WithCORS("https://app.example"))
defer srv.Close()
http.ListenAndServe(":8080", srv)
```

`srv.Shutdown(ctx)` 取消后台 A2A 任务，并在 `ctx` 结束前等待它们保存会话；`Close`
不设期限地等待。应先关闭 HTTP 服务器。

| 端点 | 用途 |
| --- | --- |
| `POST /sessions/{id}/runs` | 运行 `{"input": "..."}` 或 `{"message": {...}}` |
| `POST /sessions/{id}/resume` | 用 `{"approvals": {"<call_id>": {"approved": true}}}` 恢复运行 |
| `POST /sessions/{id}/discard` | 放弃被中断或失败的运行 |
| `GET /sessions/{id}` | 消息，以及待恢复的运行和它的审批请求 |
| `DELETE /sessions/{id}` | `Runner.EndSession`；`?purge=true` 同时删除会话 |
| `POST /sessions`、`GET /sessions`、`POST /sessions/{id}/clear`、`/fork` | 创建、列出、清空和分叉 |
| `GET /sessions/{id}/checkpoints`、`POST /sessions/{id}/rewind` | 列出检查点；用 `{"checkpoint_id": "...", "mode": "both"}`（`code`、`conversation`）回退 |
| `POST /chat` | Vercel AI SDK `useChat` |
| `POST /agui` | AG-UI |
| `GET /.well-known/agent-card.json`、`POST /a2a` | A2A 1.0（JSON-RPC） |

- **传输格式**：运行以 Server-Sent Events 流式返回：事件名是事件类型，ID 是
  序号，数据是字段为 snake_case 的 `loop.Event` JSON。
  `Accept: application/json` 改为返回最终的 `Result`，`application/x-ndjson`
  则以 JSON Lines 流式返回。`server.WriteJSONL(w, events)` 写出同样的行，
  例如用作命令的输出。中断的运行状态和完整对话不会发送。
- **协议**：通过 `?protocol=vercel-ui` 或 `ag-ui`，或 `WithProtocol`，会话端点
  也可以输出 [AI SDK UI 消息流](https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol)
  或 [AG-UI 事件](https://docs.ag-ui.com/concepts/events)。工具审批对应 AI SDK
  的审批 part 和 AG-UI 的中断；Handoff 和输出重试对应 `data-handoff`、
  `data-output-retry` part 和 AG-UI 的 `CUSTOM` 事件；Guardrail 拦截对应结束
  原因 `content-filter` 和代码为 `blocked` 的 `RUN_ERROR`。
- **文件**：各协议的用户消息都接受图片、音频、视频、文本、PDF 和 Office 文件，
  内联或按 URL 引用，转换为模型 part；其他类型返回 415。UI 消息流把回答和工具
  结果中的媒体作为 file part 发送。被输出 Guardrail 拦截的回答永远不会被保存；
  使用 `guard.HoldOutput()` 时也不会被流式发送。
- **A2A**：实现了 [A2A 规范](https://a2a-protocol.org/latest/specification/)
  的 `SendMessage`、`SendStreamingMessage`、`GetTask`、`ListTasks`、
  `CancelTask` 和 `SubscribeToTask`。`contextId` 就是会话 ID；等待审批的运行
  会让任务进入 `TASK_STATE_INPUT_REQUIRED`，被 Guardrail 拦截的运行进入
  `TASK_STATE_REJECTED`。
- **并发**：一个会话同时只处理一个请求，其他请求得到 409。
  `session.ErrConflict` 同样返回 409。
- **断开连接**：客户端断开时，运行停止，会话被保存。配置了 `Checkpointer`
  时，`POST /sessions/{id}/resume` 可以继续它。
- **限制**：请求体大小由 `WithMaxBodyBytes` 限制。只有开启
  `AllowRunOptions` 时，客户端才能设置 `max_turns` 和 `limits`。
  `WithSessionAccess` 对每个会话 ID 做授权。

完整的端点和错误码见 `server` 包文档。

### 评估

`eval` 包在数据集上运行任务并为每个输出打分，风格参照
[pydantic-evals](https://pydantic.dev/docs/ai/evals/) 和
[Google ADK 评估](https://adk.dev/evaluate/)：

```go
ds := &eval.Dataset[string, string]{
	Name: "weather",
	Cases: []eval.Case[string, string]{{
		Name: "kyoto", Input: "京都天气如何？", Expected: new("Sunny"),
		Evaluators: []eval.Evaluator{eval.ToolTrajectory(eval.MatchExact,
			[]eval.ToolCall{{Name: "weather", Args: map[string]any{"city": "Kyoto"}}})},
	}},
	Evaluators: []eval.Evaluator{
		eval.Contains("sunny", eval.IgnoreCase()),
		eval.MaxTurns(3),
		eval.LLMJudge(judge, "回答明确给出天气预报，不含糊其辞。", eval.JudgeInput()),
	},
}

func TestWeather(t *testing.T) {
	eval.Run(t, ds, eval.AgentTask(a), eval.Repeat(3), eval.Threshold(0.9))
}
```

- **任务**：任意 `func(ctx, In) (Out, *loop.Result, error)`。
  `eval.AgentTask(a)` 以输入文本运行 Agent，`TypedAgentTask[T]` 解码结构化
  输出，`AgentTaskFunc` 映射其他输入。`SimulateUser(a, userModel)` 由模型按
  `Scenario` 扮演用户，与 Agent 进行多轮对话，类似 ADK 的用户模拟。
- **评估器**：返回带理由的断言、分数和标签：`Equals`、`EqualsExpected`、
  `Contains`、`Regex`、`JSONSchema`、`IsType`、`DecodesAs`、`Completed`、
  `MaxTurns`、`MaxTokens`、`MaxToolCalls`、`MaxDuration`、`ResponseMatch`
  （ROUGE-1）和 `ToolTrajectory`（`MatchExact`、`MatchInOrder`、
  `MatchAnyOrder` 或 `MatchSubset`，可配合 `PartialArgs` 或 `IgnoreArgs`）。
  其他评估器可以用 `Check`、`Func` 或 `TypedCheck` 编写。
- **LLM 评审**：`LLMJudge` 让模型给出裁决（理由、是否通过，以及开启
  `JudgeScore` 时 0 到 1 的分数），以结构化输出请求并校验，默认温度为 0。
  `JudgeInput` 和 `JudgeExpected` 让评审看到输入和期望输出；`JudgeSeed` 和
  `JudgeReasoning` 可选。
- **Span**：用 `middleware.Tracing(eval.Tracer())` 追踪 Agent，每次运行的
  Span 位于 `EvaluatorContext.Spans`，`HasSpan` 和 `NoSpan` 用 `SpanQuery`
  匹配它们。
- **运行**：`Dataset.Evaluate` 接受 `Concurrency`（默认 8）、`Repeat`、
  `Timeout`、`EvaluatorTimeout`、`Experiment` 和 `OnResult`。失败、panic 或
  卡住的任务或评估器只会让所在用例失败。
- **报告**：通过率、各断言通过率、平均分、标签、Token 和耗时分位数，重复
  运行的用例用 `ByCase` 汇总。`WriteText` 打印表格，`Save` 和 `LoadReport`
  使用 JSON，`eval.Compare(baseline, candidate)` 列出退化的用例。`eval.Run`
  记录报告，并在低于 `Threshold` 或 `MinScore`，或有用例相对 `Baseline`
  退化时让测试失败。
- **数据集**：`eval.LoadDataset[In, Out](path)` 和 `Dataset.Save` 读写 JSON
  或 JSON Lines（`.jsonl`）。评估器是代码；期望的工具调用可以存放在用例
  元数据的 `tool_calls` 下。

**录制与回放**：`eval/replay` 把 HTTP 交互录制到 cassette 文件并回放，让
Provider 和 Agent 测试无需联网。把它的客户端传给 Provider，例如 agentx-contrib
中的 Provider：

```go
rec := replay.NewTest(t, "testdata/weather.json")
m, _ := openai.New(openai.WithHTTPClient(rec.Client()))
```

`AGENTX_REPLAY=record go test ./...` 使用真实凭据录制；默认的 `replay` 模式
从 cassette 应答，`auto` 只录制缺失的 cassette。请求按方法、URL 和请求体
（按 JSON 比较）匹配。`Authorization`、`x-api-key`、`x-goog-api-key` 等凭据
请求头以及 `key` 等查询参数保存为 `REDACTED`；`WithFilter` 可清理请求体。
Server-Sent Events 按事件逐个保存和回放。

## Provider

`model.Model` 是一个小接口，实现位于
[agentx-contrib](https://github.com/nostosx/agentx-contrib/blob/main/README.zh-CN.md#provider)
的 `provider` 模块。它直接通过 `net/http` 调用各家 API，无需厂商 SDK：

| 包 | API |
| --- | --- |
| `github.com/nostosx/agentx-contrib/provider/anthropic` | Anthropic Messages API，也支持 Vertex AI |
| `github.com/nostosx/agentx-contrib/provider/openai` | OpenAI Responses 和 Chat Completions、Azure OpenAI，以及 Ollama、DeepSeek、OpenRouter 等兼容服务 |
| `github.com/nostosx/agentx-contrib/provider/gemini` | Gemini `generateContent`，也支持 Vertex AI |
| `github.com/nostosx/agentx-contrib/provider/bedrock` | Amazon Bedrock Converse 和 ConverseStream |

每个 Provider 都从环境变量读取密钥，例如 `OPENAI_API_KEY`：

```go
m, err := openai.New(openai.DeepSeek()) // 读取 DEEPSEEK_API_KEY
```

环境变量、OpenAI 兼容服务的预设、云平台选项以及各 Provider 接受的媒体见
agentx-contrib 的 README。某个 Provider 特有的请求字段可以通过
`agent.WithProviderOptions("openai", map[string]any{...})` 传入：每个适配器只读取自己
名下的选项，因此同一个请求可以携带多个 Provider 的选项。推理强度和推理内容展示使用带类型的
`model.Effort` 与 `model.ReasoningDisplay` 常量。其他模型只要实现 `model.Model` 就能使用；
`model/modeltest` 提供用于测试的脚本化模型。

## 包结构

| 包 | 职责 |
| --- | --- |
| `agent` | Agent 定义、配置选项、`Runner`（会话、恢复运行与回退） |
| `loop` | Agent Loop、工具执行、审批、事件、检查点和中间件接口 |
| `model` | 与 Provider 无关的消息、请求、响应、流式和错误类型；`modeltest` 提供用于测试的脚本化模型 |
| `tool` | 工具接口、JSON Schema、注册表、执行器和中间件 |
| `tool/builtin` | 内置的文件、搜索、Shell、网页和待办工具；文件历史 |
| `actx` | 上下文拼装、Token 估算、压缩策略；以中间件形式运行 |
| `session` | 会话及其存储 |
| `memory` | 长期记忆，支持关键词、向量和混合检索 |
| `rag` | 检索增强生成：加载器、切分器、索引、检索器、搜索工具和上下文 Provider |
| `rag/ragtest` | `rag.VectorStore` 实现的一致性测试，以及用于测试的确定性 Embedder |
| `skill` | `SKILL.md` 加载、发现、注册表与调用 |
| `hook` | 生命周期 Hook 与命令 Hook；以中间件形式运行 |
| `permission` | Claude Code 权限规则、模式与设置；以中间件形式运行 |
| `guard` | 输入、输出和工具 Guardrail；以中间件形式运行 |
| `workflow` | 图编排 |
| `middleware` | 内置的 runtime 中间件：链路追踪、指标；另含纯模型装饰器 |
| `mcp` | MCP 客户端与服务端、传输层、OAuth 和工具适配 |
| `server` | HTTP 服务：会话 API、Vercel AI SDK、AG-UI 和 A2A |
| `eval` | 数据集、评估器（包括 LLM 评审和工具轨迹）、报告与对比 |
| `eval/replay` | 用于 Provider 测试的 HTTP 录制与回放 |

[agentx-contrib](https://github.com/nostosx/agentx-contrib) 存放带有依赖或
特定于 Provider 的实现，每项都是独立模块：`provider`（OpenAI、Anthropic、
Gemini 和 Bedrock）、`otel`、`sqlstore`、`filestore`、`middleware`（日志和
重试）。可运行的程序位于独立项目 agentx-examples。

## 示例

从单轮问答到通过 HTTP 提供带工具审批的 Agent，可运行的示例位于独立项目
[agentx-examples](https://github.com/nostosx/agentx-examples)。独立检出即可，
Go 会下载已发布的依赖：

```sh
git clone https://github.com/nostosx/agentx-examples.git
cd agentx-examples
go mod download
export ANTHROPIC_API_KEY=...
go run ./cmd/basic "什么是 Agent Loop？"
```

## 开发

使用 Go 1.27.3、GNU Make 和 Bash；`make help` 列出所有入口。

```sh
make check      # 格式、现代化、tidy 检查、vet、构建、依赖边界、随机顺序 race 测试
make lint       # golangci-lint
make vulncheck  # 可达漏洞扫描
make test       # 普通单元测试
make race       # 随机顺序 race 测试
```

检查入口不会应用格式、现代化或 tidy 修改；需要修改时显式运行 `make fmt`、
`make fix` 或 `make tidy`，然后审阅差异。所有入口设置 `GOWORK=off`。
`make deps-check` 是 `check` 的一部分，包括测试在内只允许标准库、agentx 和 kitx。
需要其他依赖的实现应放在 agentx-contrib。

CI 在 Linux、macOS 和 Windows 上运行 `make check`，lint 和漏洞扫描独立运行。
工具安装和贡献要求见 [CONTRIBUTING.md](CONTRIBUTING.md)；漏洞报告与沙箱及权限的
已知限制见 [SECURITY.md](SECURITY.md)；版本顺序和发布门禁见
[RELEASING.md](RELEASING.md)。

## 许可证

[MIT](LICENSE)

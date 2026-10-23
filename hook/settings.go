package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/nostosx/agentx/model"
)

// settingsEvents lists the events a settings file can configure, and
// whether each supports matchers. As in Claude Code, the matcher of an
// event without matcher support is ignored.
var settingsEvents = map[EventType]bool{
	SessionStart:       true,
	UserPromptSubmit:   false,
	PreModel:           false,
	PostModel:          false,
	PreToolUse:         true,
	PostToolUse:        true,
	PostToolUseFailure: true,
	Stop:               false,
	SessionEnd:         true,
	SubagentStart:      true,
	SubagentStop:       true,
	PreCompact:         true,
	PostCompact:        true,
	Notification:       true,
}

type settingsFile struct {
	DisableAllHooks *bool                      `json:"disableAllHooks"`
	Hooks           map[string][]settingsGroup `json:"hooks"`
}

type settingsGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []settingsHandler `json:"hooks"`
}

type settingsHandler struct {
	Type            string            `json:"type"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args"`
	Timeout         float64           `json:"timeout,omitempty"`
	If              string            `json:"if,omitempty"`
	Async           bool              `json:"async,omitempty"`
	AsyncRewake     bool              `json:"asyncRewake,omitempty"`
	Shell           string            `json:"shell,omitempty"`
	OnFailure       string            `json:"onFailure,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	AllowedEnvVars  []string          `json:"allowedEnvVars,omitempty"`
	Server          string            `json:"server,omitempty"`
	Tool            string            `json:"tool,omitempty"`
	Input           map[string]any    `json:"input,omitempty"`
	Prompt          string            `json:"prompt,omitempty"`
	Model           string            `json:"model,omitempty"`
	ContinueOnBlock bool              `json:"continueOnBlock,omitempty"`
}

// SettingsOptions supplies what the hooks of a settings file need beyond
// commands. Handlers whose dependency is missing are skipped with a
// warning.
type SettingsOptions struct {
	// Model evaluates "prompt" handlers. Their "model" field becomes
	// Prompt.ModelName.
	Model model.Model
	// Verifier runs "agent" handlers; see agent.HookVerifier.
	Verifier Verifier
	// MCP calls the tools of "mcp_tool" handlers; see mcp.HookCaller.
	MCP MCPCaller
	// HTTPClient sends "http" handlers; nil uses http.DefaultClient.
	HTTPClient *http.Client
}

// promptEvents lists the events that support prompt and agent handlers in
// Claude Code.
var promptEvents = map[EventType]bool{
	UserPromptSubmit:   true,
	PreToolUse:         true,
	PostToolUse:        true,
	PostToolUseFailure: true,
	Stop:               true,
	SubagentStop:       true,
}

// ParseSettings returns a manager with the hooks configured in the "hooks"
// block of a Claude Code settings file, such as
//
//	{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [
//		{"type": "command", "command": "./check.sh", "timeout": 30}]}]}}
//
// It is SettingsOptions.Parse without options, so only command and http
// handlers run.
func ParseSettings(data []byte) (*Manager, []string, error) {
	return SettingsOptions{}.Parse(data)
}

// LoadSettings reads Claude Code settings files with
// SettingsOptions.Load without options, so only command and http handlers
// run.
func LoadSettings(paths ...string) (*Manager, []string, error) {
	return SettingsOptions{}.Load(paths...)
}

// Parse returns a manager with the hooks configured in the "hooks" block
// of a Claude Code settings file.
//
// Handlers become Command, HTTP, MCPTool, Prompt and Agent hooks: timeout
// is in seconds, args selects a command's exec form, async and
// asyncRewake commands are wrapped in Async (async commands get no
// timeout), and onFailure "block" makes a failing hook deny the action as
// exit status 2 would, except on Stop and SubagentStop and for async
// hooks. statusMessage and once are ignored. "disableAllHooks": true
// yields an empty manager. As in Claude Code, the
// CLAUDE_CODE_STOP_HOOK_BLOCK_CAP environment variable sets the manager's
// StopBlockCap, and CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS its
// SessionEndTimeout in milliseconds.
//
// Handlers agentx cannot run as configured are skipped and described in the
// returned warnings: prompt, agent and mcp_tool handlers without the
// matching option, handler types an event does not support in Claude Code
// (SessionStart runs only command and mcp_tool handlers; prompt and agent
// handlers run only for UserPromptSubmit, PreToolUse, PostToolUse,
// PostToolUseFailure, Stop and SubagentStop), the if field, shell
// "powershell", events agentx does not dispatch, and invalid matchers.
// The error reports malformed JSON.
func (o SettingsOptions) Parse(data []byte) (*Manager, []string, error) {
	l := newSettingsLoader(o)
	if err := l.parse("", data); err != nil {
		return nil, nil, err
	}
	return l.manager(), l.warnings, nil
}

// Load reads Claude Code settings files with Parse and returns one
// manager with all their hooks. Pass the files in increasing precedence,
// for example ~/.claude/settings.json, .claude/settings.json and
// .claude/settings.local.json: the last disableAllHooks wins. Files that
// do not exist are skipped. A handler configured identically in more than
// one file runs once, as in Claude Code.
func (o SettingsOptions) Load(paths ...string) (*Manager, []string, error) {
	l := newSettingsLoader(o)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if err := l.parse(path, data); err != nil {
			return nil, nil, err
		}
	}
	return l.manager(), l.warnings, nil
}

type settingsLoader struct {
	opts     SettingsOptions
	m        *Manager
	disabled bool
	seen     map[string]bool
	warnings []string
}

func newSettingsLoader(opts SettingsOptions) *settingsLoader {
	return &settingsLoader{opts: opts, m: NewManager(envOptions()...), seen: map[string]bool{}}
}

func (l *settingsLoader) manager() *Manager {
	if l.disabled {
		return NewManager(envOptions()...)
	}
	return l.m
}

// envOptions returns the manager options Claude Code's environment
// variables set.
func envOptions() []Option {
	var opts []Option
	if n, err := strconv.Atoi(os.Getenv("CLAUDE_CODE_STOP_HOOK_BLOCK_CAP")); err == nil && n >= 0 {
		opts = append(opts, StopBlockCap(n))
	}
	if ms, err := strconv.Atoi(os.Getenv("CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS")); err == nil && ms > 0 {
		opts = append(opts, SessionEndTimeout(time.Duration(ms)*time.Millisecond))
	}
	return opts
}

func (l *settingsLoader) parse(path string, data []byte) error {
	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		if path != "" {
			return fmt.Errorf("hook settings %s: %w", path, err)
		}
		return fmt.Errorf("hook settings: %w", err)
	}
	if f.DisableAllHooks != nil {
		l.disabled = *f.DisableAllHooks
	}
	prefix := "hooks"
	if path != "" {
		prefix = path + ": hooks"
	}
	for _, name := range slices.Sorted(maps.Keys(f.Hooks)) {
		groups, event := f.Hooks[name], EventType(name)
		matchers, ok := settingsEvents[event]
		if !ok {
			l.warnings = append(l.warnings, fmt.Sprintf("%s.%s: event not dispatched by agentx; skipped", prefix, name))
			continue
		}
		for gi, g := range groups {
			matcher := g.Matcher
			if !matchers {
				matcher = ""
			}
			if _, err := compileMatcher(matcher); err != nil {
				l.warnings = append(l.warnings, fmt.Sprintf("%s.%s[%d]: invalid matcher %q: %v; skipped", prefix, name, gi, matcher, err))
				continue
			}
			for hi, sh := range g.Hooks {
				at := fmt.Sprintf("%s.%s[%d].hooks[%d]", prefix, name, gi, hi)
				h, problem := l.hook(event, sh)
				if problem != "" {
					l.warnings = append(l.warnings, at+": "+problem+"; skipped")
					continue
				}
				key, _ := json.Marshal(sh)
				id := name + "\x00" + matcher + "\x00" + string(key)
				if l.seen[id] {
					continue
				}
				l.seen[id] = true
				if err := l.m.On(event, matcher, h); err != nil {
					l.warnings = append(l.warnings, at+": "+err.Error()+"; skipped")
				}
			}
		}
	}
	return nil
}

// hook returns the hook for a handler, or a description of why agentx
// cannot run it.
func (l *settingsLoader) hook(event EventType, sh settingsHandler) (Hook, string) {
	timeout := time.Duration(sh.Timeout * float64(time.Second))
	switch {
	case sh.If != "":
		return nil, `the "if" field is not supported`
	case (sh.Async || sh.AsyncRewake) && sh.Type != "command":
		return nil, "async is only supported for command hooks"
	case sh.OnFailure != "" && sh.OnFailure != "continue" && sh.OnFailure != "block":
		return nil, fmt.Sprintf("unknown onFailure %q", sh.OnFailure)
	case event == SessionStart && sh.Type != "command" && sh.Type != "mcp_tool":
		return nil, fmt.Sprintf("SessionStart does not support %s hooks", sh.Type)
	case (sh.Type == "prompt" || sh.Type == "agent") && !promptEvents[event]:
		return nil, fmt.Sprintf("%s does not support %s hooks", event, sh.Type)
	}
	var h Hook
	switch sh.Type {
	case "command":
		switch {
		case sh.Command == "":
			return nil, "command is empty"
		case sh.Shell != "" && sh.Shell != "bash":
			return nil, fmt.Sprintf("shell %q is not supported", sh.Shell)
		}
		c := &Command{Command: sh.Command, Args: sh.Args, Timeout: timeout}
		switch {
		case sh.AsyncRewake:
			return &Async{Hook: c, Rewake: true}, ""
		case sh.Async:
			// Claude Code does not enforce the timeout of async hooks.
			c.Timeout = -1
			return &Async{Hook: c}, ""
		}
		h = c
	case "http":
		if sh.URL == "" {
			return nil, "url is empty"
		}
		h = &HTTP{URL: sh.URL, Headers: sh.Headers, AllowedEnvVars: sh.AllowedEnvVars, Timeout: timeout, Client: l.opts.HTTPClient}
	case "mcp_tool":
		switch {
		case sh.Server == "" || sh.Tool == "":
			return nil, "server and tool are required"
		case l.opts.MCP == nil:
			return nil, "mcp_tool hooks need SettingsOptions.MCP"
		}
		h = &MCPTool{Caller: l.opts.MCP, Server: sh.Server, Tool: sh.Tool, Input: sh.Input, Timeout: timeout}
	case "prompt":
		switch {
		case sh.Prompt == "":
			return nil, "prompt is empty"
		case l.opts.Model == nil:
			return nil, "prompt hooks need SettingsOptions.Model"
		}
		h = &Prompt{Model: l.opts.Model, Prompt: sh.Prompt, ModelName: sh.Model, Timeout: timeout, ContinueOnBlock: sh.ContinueOnBlock}
	case "agent":
		switch {
		case sh.Prompt == "":
			return nil, "prompt is empty"
		case l.opts.Verifier == nil:
			return nil, "agent hooks need SettingsOptions.Verifier"
		}
		h = &Agent{Verifier: l.opts.Verifier, Prompt: sh.Prompt, Model: sh.Model, Timeout: timeout}
	default:
		return nil, fmt.Sprintf("unsupported hook type %q", sh.Type)
	}
	if sh.OnFailure == "block" && event != Stop && event != SubagentStop {
		h = blockOnFailure{h}
	}
	return h, ""
}

// blockOnFailure denies the action when its hook fails, like Claude Code's
// onFailure "block".
type blockOnFailure struct{ Hook }

func (b blockOnFailure) hookKey() string {
	if k := hookKey(b.Hook); k != "" {
		return "block\x00" + k
	}
	return ""
}

func (b blockOnFailure) usesModel() bool { return usesModel(b.Hook) }

func (b blockOnFailure) hookTimeout() time.Duration {
	if t, ok := b.Hook.(timeouter); ok {
		return t.hookTimeout()
	}
	return 0
}

func (b blockOnFailure) Handle(ctx context.Context, ev *Event) (*Result, error) {
	res, err := b.Hook.Handle(ctx, ev)
	if err != nil {
		// The failure becomes the hook's verdict rather than a hook error.
		return &Result{Decision: Deny, Reason: `hook failed; blocking because onFailure is "block": ` + err.Error()}, nil //nolint:nilerr // reported in Reason
	}
	return res, nil
}

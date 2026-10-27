package skill

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Forker runs the content of a context: fork skill, task, as the prompt
// of an isolated sub-agent and returns the sub-agent's answer as the
// skill tool's result. It is called again with an empty task when the
// calling run resumes after the sub-agent interrupted for approvals; see
// loop.ResumedCall. The agent package provides one.
type Forker func(ctx context.Context, s Skill, task string) (*tool.Result, error)

// Config configures a Manager. The zero value renders skills with their
// arguments and substitutions, without command injection, and runs
// context: fork skills inline.
type Config struct {
	// ProjectDir is the project root: the base of paths patterns, of
	// relative file paths and of file rules, the working directory of
	// injected commands, and ${CLAUDE_PROJECT_DIR}. Empty means the
	// process's working directory.
	ProjectDir string

	// Shell enables Claude Code's dynamic context injection: !`command`
	// and ```! blocks in a skill run when the skill is invoked and their
	// output replaces them. It runs shell commands written in skill files
	// with the process's privileges, so enable it only for skills you
	// trust. Without it the placeholders are left as written.
	//
	// Commands run with bash, or sh if bash is missing (cmd.exe on
	// Windows), or with PowerShell for skills with "shell: powershell". A
	// command that exits with a non-zero status, except 1 from search and
	// comparison commands such as grep and diff, fails the invocation.
	// Arguments reach commands through the environment variables
	// ARGUMENTS, ARGUMENTS_0, ARGUMENTS_1, ... and those of declared
	// argument names, and with sh or bash also as $0, $1, ..., so they are
	// never parsed as shell code.
	Shell bool
	// ShellTimeout bounds each injected command; zero means
	// DefaultShellTimeout.
	ShellTimeout time.Duration
	// ShellOutputLimit bounds the output of each injected command in
	// bytes; zero means 30000.
	ShellOutputLimit int

	// Effort is the effort level ${CLAUDE_EFFORT} expands to when no
	// active skill sets one.
	Effort string
	// Fork runs context: fork skills in a sub-agent. Without it they run
	// inline, like other skills.
	Fork Forker
	// Models resolves the model field of an inline skill. A nil result, or
	// a nil Models, sends the name as model.Request.Model to the agent's
	// model instead. "inherit" keeps the current model.
	Models func(name string) model.Model
	// HookOptions builds the hooks of skills; see hook.SettingsOptions.
	HookOptions hook.SettingsOptions
	// Files returns the files a tool call reads or edits, which make
	// skills with paths available; nil means FilesOf.
	Files func(call *model.ToolCall) []string
	// MatchTool reports whether a rule of allowed-tools or disallowed-tools
	// matches a tool call; nil means MatchTool with ProjectDir.
	MatchTool func(rule string, call *model.ToolCall) bool
}

// Manager invokes the skills of a registry and gives their frontmatter
// effect in agent runs: its Tool lets the model invoke skills, its
// Middleware applies what active skills configure, and AttachHooks
// dispatches skill hooks. The agent package sets it up for agents with
// skills.
//
// While a skill invoked by the skill tool, Invoke or WithCall is active,
// until the run that invoked it ends, as Claude Code clears them when the
// user sends the next message:
//
//   - calls matching its allowed-tools need no approval, unless a hook or
//     another middleware asks or denies; other tools are unaffected;
//   - tools matching its disallowed-tools are removed from the model's
//     requests and their calls denied;
//   - model and effort override the run's model and reasoning effort.
//
// For a context: fork skill these apply to the sub-agent instead. A
// skill's hooks are registered for the rest of the session, or of the run
// for runs without a session. Skills with paths, and Scoped skills, are
// listed for the model only once the conversation has read or edited a
// matching file; one that becomes relevant during a run is announced in a
// reminder after the tool result.
type Manager struct {
	reg *Registry
	cfg Config

	mu       sync.Mutex
	sessions map[string]*sessionHooks
	outer    *hook.Manager
}

// NewManager returns a manager of the skills of r.
func NewManager(r *Registry, cfg Config) *Manager {
	if r == nil {
		r = NewRegistry()
	}
	if cfg.ProjectDir == "" {
		cfg.ProjectDir, _ = os.Getwd()
	}
	if cfg.Files == nil {
		cfg.Files = FilesOf
	}
	if cfg.MatchTool == nil {
		root := cfg.ProjectDir
		cfg.MatchTool = func(rule string, call *model.ToolCall) bool { return MatchTool(root, rule, call) }
	}
	return &Manager{reg: r, cfg: cfg, sessions: map[string]*sessionHooks{}}
}

// Registry returns the manager's registry.
func (m *Manager) Registry() *Registry { return m.reg }

// Prompt describes the skills offered to the model in a conversation:
// those it may invoke, without paths, or whose paths or scope match a
// file the conversation read or edited. It is empty when there are none.
func (m *Manager) Prompt(msgs []*model.Message) string {
	return m.reg.prompt(m.offered(touchedFiles(msgs, m.cfg.Files)))
}

// offered returns the skills offered to the model given the files read or
// edited.
func (m *Manager) offered(files []string) []Skill {
	var out []Skill
	for _, s := range m.reg.List() {
		if !s.Metadata().DisableModelInvocation && Applies(s, m.cfg.ProjectDir, files) {
			out = append(out, s)
		}
	}
	return out
}

// Call is an invocation of a skill by name, with the arguments a user
// would type after the name.
type Call struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// Invoke renders a skill as a user invokes it, for example with /name in
// a chat interface: unlike the skill tool it accepts skills with
// disable-model-invocation and refuses those with user-invocable: false.
// The result, wrapped in a <skill> element, is meant to be sent to the
// model as or with a user message. sessionID replaces
// ${CLAUDE_SESSION_ID}. Invoke does not activate the skill; to give
// allowed-tools and the other fields effect, invoke it in a run with
// WithCall or agent.RunSkill instead.
func (m *Manager) Invoke(ctx context.Context, sessionID string, c Call) (string, error) {
	s, err := m.userSkill(c.Name)
	if err != nil {
		return "", err
	}
	text, err := m.render(ctx, s, Vars{Arguments: c.Arguments, SessionID: sessionID})
	if err != nil {
		return "", err
	}
	return wrap(s, text), nil
}

func (m *Manager) userSkill(name string) (Skill, error) {
	s, ok := m.reg.Get(name)
	switch {
	case !ok:
		return nil, fmt.Errorf("unknown skill %q", name)
	case !s.Metadata().IsUserInvocable():
		return nil, fmt.Errorf("skill %q cannot be invoked by users (user-invocable: false)", name)
	}
	return s, nil
}

// render returns the content of s with its placeholders expanded and, if
// enabled, its commands replaced by their output.
func (m *Manager) render(ctx context.Context, s Skill, v Vars) (string, error) {
	content, err := s.Content(ctx)
	if err != nil {
		return "", fmt.Errorf("load skill %q: %w", s.Metadata().Name, err)
	}
	meta, dir := s.Metadata(), s.Dir()
	v.ProjectDir = m.cfg.ProjectDir
	if v.Effort == "" {
		v.Effort = m.cfg.Effort
	}
	e := newExpander(meta, v)
	if !m.cfg.Shell {
		return e.finish(e.claude(e.expand(content), dir)), nil
	}
	segs := segments(content)
	for _, seg := range segs {
		if seg.isCmd && controlChar(seg.command) {
			return "", fmt.Errorf("no shell command in skill %q was run: the command of %q contains a control character", meta.Name, seg.text)
		}
	}
	r := &shellRunner{dir: m.cfg.ProjectDir, timeout: m.cfg.ShellTimeout, limit: m.cfg.ShellOutputLimit}
	if r.timeout <= 0 {
		r.timeout = DefaultShellTimeout
	}
	if r.limit <= 0 {
		r.limit = defaultShellOutput
	}
	var b strings.Builder
	for _, seg := range segs {
		if !seg.isCmd {
			b.WriteString(e.claude(e.expand(seg.text), dir))
			continue
		}
		e.scan(seg.command)
		out, err := r.run(ctx, meta, dir, seg.command, e)
		if err != nil {
			return "", fmt.Errorf("skill %q: shell command failed for pattern %q: %w", meta.Name, seg.text, err)
		}
		b.WriteString(out)
	}
	return e.finish(b.String()), nil
}

// wrap returns the text given to the model for a skill's rendered content.
func wrap(s Skill, text string) string {
	meta := s.Metadata()
	var b strings.Builder
	fmt.Fprintf(&b, "<skill name=%q>\n", meta.Name)
	if dir := s.Dir(); dir != "" {
		fmt.Fprintf(&b, "Base directory for this skill: %s\n\n", dir)
	}
	if tools := meta.AllowedTools; len(tools) > 0 {
		fmt.Fprintf(&b, "Tools this skill uses: %s\n\n", strings.Join(tools, ", "))
	}
	b.WriteString(text)
	b.WriteString("\n</skill>")
	return b.String()
}

type skillArgs struct {
	Name      string `json:"name" jsonschema:"The skill name, exactly as listed in available_skills."`
	Arguments string `json:"arguments,omitempty" jsonschema:"Arguments for the skill, as a user would type them after the skill name; quote an argument that contains spaces."`
}

// Tool returns the tool through which the model invokes skills. It refuses
// skills with disable-model-invocation, unless the user invoked the skill
// in the same run, and runs context: fork skills with Config.Fork. The
// tool is read-only, so it may run concurrently with other read-only
// calls, unless the registry has context: fork skills or command
// injection is enabled.
func (m *Manager) Tool() tool.Tool {
	var opts []tool.Option
	readOnly := !m.cfg.Shell
	if m.cfg.Fork != nil && slices.ContainsFunc(m.reg.List(), func(s Skill) bool { return s.Metadata().Fork() }) {
		readOnly = false
	}
	if readOnly {
		opts = append(opts, tool.ReadOnly())
	}
	return tool.Func(ToolName,
		"Invoke a skill listed in available_skills: load its instructions, or run it if it runs on its own. "+
			"Call it when the task matches the skill's description, before doing the work, then follow the instructions it returns.",
		m.call, opts...)
}

func (m *Manager) call(ctx context.Context, in skillArgs) (*tool.Result, error) {
	name := strings.TrimSpace(in.Name)
	s, ok := m.reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown skill %q; available skills: %s", name, strings.Join(m.reg.Names(), ", "))
	}
	meta := s.Metadata()
	run := loop.CallingRun(ctx)
	var act activation
	if run != nil {
		act = loadActivation(run)
	}
	if meta.DisableModelInvocation && !slices.Contains(act.Permitted, name) {
		return tool.Errorf("The skill %q can only be invoked by the user. Do not carry out its steps another way; suggest that the user invoke it.", name), nil
	}
	if meta.Fork() && m.cfg.Fork != nil {
		if _, _, resumed := loop.ResumedCall(ctx); resumed {
			return m.cfg.Fork(ctx, s, "")
		}
	}
	// An invoked skill is active, so its effort is the current one.
	v := Vars{Arguments: in.Arguments, Effort: cmp.Or(meta.Effort, act.Effort)}
	if run != nil {
		v.SessionID = run.SessionID()
	}
	text, err := m.render(ctx, s, v)
	if err != nil {
		return nil, err
	}
	if meta.Fork() && m.cfg.Fork != nil {
		if dir := s.Dir(); dir != "" {
			text = "Base directory for this skill: " + dir + "\n\n" + text
		}
		return m.cfg.Fork(WithActivation(ctx, s), s, text)
	}
	out := wrap(s, text)
	if run != nil && loaded(run.Messages(), out) {
		return tool.Text(fmt.Sprintf("The skill %q is already loaded above with the same content; follow those instructions.", name)), nil
	}
	return tool.Text(out), nil
}

// loaded reports whether a tool result in msgs holds text.
func loaded(msgs []*model.Message, text string) bool {
	for _, msg := range msgs {
		for _, r := range msg.ToolResults() {
			if r.Name == ToolName && r.Content == text {
				return true
			}
		}
	}
	return false
}

// activationKey is the run state key of the active skills' effects.
const activationKey = "skill.active"

// activation is what the active skills of a run configure. It is kept in
// the run state, so a resumed run keeps it.
type activation struct {
	Allowed    []string `json:"allowed,omitempty"`
	Disallowed []string `json:"disallowed,omitempty"`
	Model      string   `json:"model,omitempty"`
	Effort     string   `json:"effort,omitempty"`
	// Permitted lists the skills the user invoked in the run, which the
	// model may then invoke despite disable-model-invocation.
	Permitted []string `json:"permitted,omitempty"`
	// Listed lists the conditional skills the model was told about.
	Listed []string `json:"listed,omitempty"`
}

func loadActivation(run *loop.Run) activation {
	a, _, _ := run.DecodeValue[activation](activationKey)
	return a
}

func (a activation) save(run *loop.Run) {
	b, _ := json.Marshal(a)
	if string(b) == "{}" {
		run.SetValue(activationKey, "")
		return
	}
	run.SetValue(activationKey, string(b))
}

// apply adds the effects of a skill's metadata. withModel also applies
// its model, which a forked skill's sub-agent gets from Config.Fork.
func (a *activation) apply(meta Metadata, dir, projectDir string, withModel bool) {
	expand := func(rules []string) []string {
		out := make([]string, 0, len(rules))
		for _, r := range rules {
			if dir != "" {
				r = strings.ReplaceAll(r, "${CLAUDE_SKILL_DIR}", dir)
			}
			if projectDir != "" {
				r = strings.ReplaceAll(r, "${CLAUDE_PROJECT_DIR}", projectDir)
			}
			out = append(out, r)
		}
		return out
	}
	a.Allowed = append(a.Allowed, expand(meta.AllowedTools)...)
	a.Disallowed = append(a.Disallowed, expand(meta.DisallowedTools)...)
	if withModel && meta.Model != "" {
		a.Model = meta.Model
	}
	if meta.Effort != "" {
		a.Effort = meta.Effort
	}
}

// activationToken carries the effects of a forked skill to its sub-agent.
type activationToken struct {
	// runID is the run that invoked the skill. Only sub-agent runs started
	// by its tool call apply the token, not their own sub-agents.
	runID string
	meta  Metadata
	dir   string
}

type activationKeyType struct{}

// WithActivation returns a context that makes the first runs started in
// it, such as the sub-agent of a context: fork skill invoked by the
// current tool call, apply the allowed-tools, disallowed-tools and effort
// of s, if their agent has the Middleware of a Manager. Their own
// sub-agents do not. A Forker passes it to the sub-agent's run.
func WithActivation(ctx context.Context, s Skill) context.Context {
	tok := &activationToken{meta: s.Metadata(), dir: s.Dir()}
	if run := loop.CallingRun(ctx); run != nil {
		tok.runID = run.ID()
	}
	return context.WithValue(ctx, activationKeyType{}, tok)
}

// userCall is a skill invocation by the user, consumed by the first run
// that sees it.
type userCall struct {
	Call
	used atomic.Bool
}

type callKey struct{}

// WithCall returns a context in which the next run of an agent with the
// Middleware of a Manager starts with an invocation of a skill by the
// user, as if the message began with /name arguments in Claude Code: the
// skill's rendered content is added in front of the run's input and the
// skill is active for the run. A context: fork skill is instead permitted
// for the run, and the model is told to invoke it. The run is blocked if
// the skill is unknown or has user-invocable: false. agent.RunSkill uses
// it.
func WithCall(ctx context.Context, c Call) context.Context {
	return context.WithValue(ctx, callKey{}, &userCall{Call: c})
}

// Middleware returns the runtime middleware that gives active skills
// effect in an agent's runs; see Manager. It is named "skill".
func (m *Manager) Middleware() loop.Middleware { return &middleware{m: m} }

type middleware struct{ m *Manager }

func (mw *middleware) Name() string { return "skill" }

func (mw *middleware) BeforeRun(ctx context.Context, run *loop.Run, in *loop.RunInput) error {
	m := mw.m
	history := run.Messages()
	// Skill hooks last for the session: re-register those of the skills
	// invoked earlier, for example before the process restarted.
	for _, name := range invokedSkills(history) {
		if s, ok := m.reg.Get(name); ok {
			m.registerHooks(run, s)
		}
	}
	act := loadActivation(run)
	if tok, _ := ctx.Value(activationKeyType{}).(*activationToken); tok != nil {
		if caller := loop.CallingRun(ctx); caller != nil && caller.ID() == tok.runID {
			act.apply(tok.meta, tok.dir, m.cfg.ProjectDir, false)
		}
	}
	for _, s := range m.offered(touchedFiles(history, m.cfg.Files)) {
		if Conditional(s) {
			act.Listed = append(act.Listed, s.Metadata().Name)
		}
	}
	if uc, _ := ctx.Value(callKey{}).(*userCall); uc != nil && uc.used.CompareAndSwap(false, true) {
		if err := mw.userInvocation(ctx, run, in, uc.Call, &act); err != nil {
			return err
		}
	}
	act.save(run)
	return nil
}

// userInvocation starts a run with a skill the user invoked.
func (mw *middleware) userInvocation(ctx context.Context, run *loop.Run, in *loop.RunInput, c Call, act *activation) error {
	m := mw.m
	s, err := m.userSkill(strings.TrimSpace(c.Name))
	if err != nil {
		return loop.Block(err.Error())
	}
	meta := s.Metadata()
	act.Permitted = append(act.Permitted, meta.Name)
	var part string
	if meta.Fork() && m.cfg.Fork != nil {
		b, _ := json.Marshal(Call{Name: meta.Name, Arguments: c.Arguments})
		part = model.WrapReminder(fmt.Sprintf("The user invoked the %q skill. Call the `%s` tool with %s now, then report its result.", meta.Name, ToolName, b))
	} else {
		text, err := m.render(ctx, s, Vars{Arguments: c.Arguments, SessionID: run.SessionID(), Effort: cmp.Or(meta.Effort, act.Effort)})
		if err != nil {
			return loop.Block(err.Error())
		}
		part = wrap(s, text)
		act.apply(meta, s.Dir(), m.cfg.ProjectDir, true)
	}
	m.registerHooks(run, s)
	in.Message.Parts = slices.Insert(in.Message.Parts, 0, model.TextPart(part))
	return nil
}

func (mw *middleware) WrapModel(run *loop.Run, next model.Model) model.Model {
	act := loadActivation(run)
	if act.Model == "" || act.Model == "inherit" || mw.m.cfg.Models == nil {
		return next
	}
	if mdl := mw.m.cfg.Models(act.Model); mdl != nil {
		return mdl
	}
	return next
}

func (mw *middleware) BeforeModel(_ context.Context, run *loop.Run, req *model.Request) error {
	act := loadActivation(run)
	if act.Model != "" && act.Model != "inherit" && (mw.m.cfg.Models == nil || mw.m.cfg.Models(act.Model) == nil) {
		req.Model = act.Model
	}
	if act.Effort != "" {
		r := model.Reasoning{}
		if req.Reasoning != nil {
			r = *req.Reasoning
		}
		r.Effort = model.Effort(act.Effort)
		req.Reasoning = &r
	}
	if len(act.Disallowed) > 0 {
		req.Tools = slices.DeleteFunc(req.Tools, func(spec *model.ToolSpec) bool {
			// Only whole-tool rules remove a tool; rules with a specifier
			// deny the calls they match.
			call := &model.ToolCall{Name: spec.Name, Arguments: json.RawMessage("{}")}
			return slices.ContainsFunc(act.Disallowed, func(rule string) bool {
				return !strings.Contains(rule, "(") && mw.m.cfg.MatchTool(rule, call)
			})
		})
	}
	return nil
}

func (mw *middleware) BeforeTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	act := loadActivation(run)
	if len(act.Allowed) == 0 && len(act.Disallowed) == 0 {
		return nil
	}
	match := mw.m.cfg.MatchTool
	for _, rule := range act.Disallowed {
		if matchesAnyFile(match, rule, plan.Call) {
			plan.Deny(fmt.Sprintf("The tool %s is not available while the active skill disallows it (%s).", plan.Call.Name, rule))
			return nil
		}
	}
	for _, rule := range act.Allowed {
		if match(rule, plan.Call) {
			plan.Allow()
			return nil
		}
	}
	return nil
}

func (mw *middleware) AfterTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan, res *model.ToolResult) error {
	m := mw.m
	if d, _ := plan.Decision(); d == loop.Denied {
		return nil
	}
	if plan.Call.Name == ToolName && !res.IsError {
		var in skillArgs
		if json.Unmarshal(plan.Call.Arguments, &in) == nil {
			if s, ok := m.reg.Get(strings.TrimSpace(in.Name)); ok {
				m.registerHooks(run, s)
				if !s.Metadata().Fork() || m.cfg.Fork == nil {
					act := loadActivation(run)
					act.apply(s.Metadata(), s.Dir(), m.cfg.ProjectDir, true)
					act.save(run)
				}
			}
		}
	}
	files := m.cfg.Files(plan.Call)
	if len(files) == 0 {
		return nil
	}
	act := loadActivation(run)
	var b strings.Builder
	for _, s := range m.reg.List() {
		meta := s.Metadata()
		if meta.DisableModelInvocation || !Conditional(s) || slices.Contains(act.Listed, meta.Name) || !Applies(s, m.cfg.ProjectDir, files) {
			continue
		}
		act.Listed = append(act.Listed, meta.Name)
		b.WriteString(listEntry(meta))
	}
	if b.Len() == 0 {
		return nil
	}
	act.save(run)
	res.Content += "\n\n" + model.WrapReminder("These skills apply to the files you are working with. Call the `"+ToolName+"` tool with a skill's name when its description matches the task.\n"+strings.TrimSuffix(b.String(), "\n"))
	return nil
}

func (mw *middleware) AfterRun(_ context.Context, run *loop.Run, res *loop.Result) {
	if run.SessionID() == "" && res.Status.Done() {
		// Without a session, skill hooks last for the run.
		mw.m.forget(sessionKey("", run.ID()))
	}
}

// invokedSkills returns the names of the skills invoked in msgs: by skill
// tool calls that succeeded and by user messages that start with a skill.
func invokedSkills(msgs []*model.Message) []string {
	var names []string
	ok := map[string]bool{}
	for _, msg := range msgs {
		for _, r := range msg.ToolResults() {
			ok[r.CallID] = r.Name == ToolName && !r.IsError
		}
	}
	for _, msg := range msgs {
		for _, c := range msg.ToolCalls() {
			var in skillArgs
			if c.Name == ToolName && ok[c.ID] && json.Unmarshal(c.Arguments, &in) == nil {
				names = append(names, strings.TrimSpace(in.Name))
			}
		}
		if msg.Role != model.RoleUser {
			continue
		}
		for _, p := range msg.Parts {
			if p.Type != model.PartText || !strings.HasPrefix(p.Text, "<skill name=") {
				continue
			}
			line, _, _ := strings.Cut(strings.TrimPrefix(p.Text, "<skill name="), "\n")
			if name, err := strconv.Unquote(strings.TrimSuffix(line, ">")); err == nil {
				names = append(names, name)
			}
		}
	}
	return names
}

// Package permission decides tool calls with permission rules and modes
// in Claude Code's syntax and semantics; see
// https://code.claude.com/docs/en/permissions and
// https://code.claude.com/docs/en/permission-modes.
//
// A Policy holds allow, ask and deny rules such as "Bash(npm run test *)",
// "Read(./.env)", "Edit(src/**)", "WebFetch(domain:example.com)" and
// "mcp__github__*", and a permission mode. It runs as runtime middleware:
// before each tool call it allows the call, requires approval for it or
// denies it, and the runtime merges that with the decisions of other
// middleware, such as hooks, and with the tool's own RequiresApproval,
// keeping the most restrictive one.
//
// Rules are evaluated in order deny, then ask, then allow; the first match
// in that order decides, whatever the rules' specificity. Claude Code tool
// names select the agentx built-in tools: Bash selects bash, Read selects
// read_file, glob and grep, Edit selects write_file and edit_file, WebFetch
// selects web_fetch, and Write, MultiEdit, Glob, Grep and TodoWrite select
// the tool of the same role, as do BashOutput and TaskOutput for
// bash_output and KillShell and TaskStop for kill_shell. Rules may also
// name agentx tools, such as "bash(git status)" or "transfer_to_billing".
// Config.Classify extends this to other tools.
//
// In deny and ask lists, Tool(param:value) is a parameter rule: it matches
// the calls whose top-level argument param, a string, number or boolean,
// matches value, in which "*" matches any text. It cannot name a tool's
// primary parameter, such as command, path or url. For example,
// "Bash(dangerously_disable_sandbox:true)" in the ask list asks before
// every command that leaves bash's sandbox, in every mode; see
// builtin.Config.Sandbox.
//
// # Shell commands
//
// Bash rules match each command of a compound command on its own: the
// command line is split on &&, ||, ;, |, |&, & and newlines, and the
// commands inside subshells, command substitutions ($(...) and
// backquotes), process substitutions and control-flow bodies are matched
// too. A deny or ask rule applies if any command matches it; an allow rule
// approves the line only if every command is approved, by a rule, by being
// one of the read-only commands Claude Code runs without approval (ls,
// cat, echo, pwd, head, tail, grep, find, wc, which, diff, stat, du, cd and
// read-only git commands), or in acceptEdits mode by being mkdir, touch,
// rm, rmdir, mv or cp on paths inside the working directories. Process
// wrappers (timeout, time, nice, nohup, stdbuf, command, builtin, noglob,
// and xargs without flags) and leading assignments of known-safe
// variables are stripped before allow rules match; deny and ask rules
// match past any assignment and wrapper. Files that redirections, tee and
// well-known file commands such as cat and rm read or write are checked
// against Read and Edit rules.
//
// The lexer behind this is not a shell parser. A command it cannot split
// reliably, such as one with an unterminated quote, a trailing && or a
// construct like a case statement or function definition, is never
// approved by allow rules, while deny and ask rules still see the commands
// it found and the whole text. Like Claude Code's, Bash rules match
// command text: they do not see a program run by its path (/bin/rm), in
// an inline script (sh -c, which deny rules here do check), through a
// variable or by another program, so they are not a security boundary.
// Sandbox the shell where that matters.
//
// # Files and URLs
//
// Read and Edit rules use gitignore patterns anchored as in Claude Code:
// "//path" at the filesystem root, "~/path" at the home directory, "/path"
// at the rule's source (Rules.Base, by default the working directory) and
// "path" or "./path" at the working directory, Config.Dir. A bare name
// such as ".env" matches at any depth, and in deny and ask rules so does
// a single directory such as "secrets/**". "!pattern" in a deny or ask
// list carves paths out of the rules before it in the same list. A path
// through a symbolic link is checked as requested and as resolved: allow
// rules must match both, deny and ask rules either. A Read deny rule also
// blocks edits of the path.
// Recursive glob and grep searches omit discovered files whose paths are
// denied or need approval, including when include_ignored is true. Name a
// file explicitly to request approval for it. Directory traversal is not
// pruned by these rules, so that path carve-outs still work.
//
// WebFetch(domain:...) rules match the host of the URL argument. Redirects
// are checked when the web_fetch tool uses Policy.CheckRedirect; see
// builtin.Config.CheckRedirect.
//
// # Modes
//
// ModeDefault leaves calls no rule decides to the tool's RequiresApproval,
// except read-only shell commands, which are allowed. ModeAcceptEdits also
// allows file edits and the filesystem commands above inside the working
// directories. ModePlan denies every call that is not read-only, except
// the built-in bash_output, kill_shell and todo_write, which only change
// the agent's own state. Handoff tools (agent.WithHandoffs) are read-only,
// so a plan can be handed off; plan mode is recorded in the run value
// ModeKey, which a handoff carries over, so the agent handed off to plans
// too if it has a policy.
// ModeDontAsk denies every call that would need approval unless an allow
// rule approves it. ModeBypassPermissions allows every call that no deny
// or ask rule matches. In every mode deny rules deny, ask rules ask (deny
// in dontAsk), and an rm or rmdir of a critical path, such as the home or
// working directory, asks; writes to protected paths such as .git and
// .claude ask in every mode but bypassPermissions. Claude Code's auto mode
// needs its classifier model and is not supported.
//
// The mode of a run is Config.Mode unless the run value ModeKey sets
// another, for example with agent.RunValue or Policy.SetMode from other
// middleware.
//
// # Remembered approvals
//
// An approval with loop.Approval.Remember set makes the policy allow
// later calls like the approved one: for the rest of the run, or, kept in
// a session-scoped run value that agent.Runner saves with the session, for
// the rest of the session. The policy remembers the rules in
// Approval.RememberRules or, without them, rules derived from the call:
// the exact commands that needed approval, the exact paths, the domain or
// the tool. Remembered rules are allow rules: they apply from the next
// call the policy decides, and deny rules, ask rules, critical-path
// removals and protected paths still take precedence over them.
package permission

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/fsmatch"

	"github.com/nostosx/agentx/internal/fileaccess"
	"github.com/nostosx/agentx/internal/toolnames"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Mode is a permission mode; see
// https://code.claude.com/docs/en/permission-modes.
type Mode string

// The permission modes. Their values are Claude Code's.
const (
	ModeDefault           Mode = "default"
	ModeAcceptEdits       Mode = "acceptEdits"
	ModePlan              Mode = "plan"
	ModeDontAsk           Mode = "dontAsk"
	ModeBypassPermissions Mode = "bypassPermissions"
)

// ParseMode parses a mode. As in Claude Code, "manual" is an alias of
// "default"; the empty string is the default mode too.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeDefault, "manual":
		return ModeDefault, nil
	case ModeAcceptEdits, ModePlan, ModeDontAsk, ModeBypassPermissions:
		return Mode(s), nil
	case "auto":
		return "", errors.New(`permission mode "auto" needs Claude Code's classifier and is not supported`)
	}
	return "", fmt.Errorf("unknown permission mode %q", s)
}

// Kind is the kind of a tool as rules see it.
type Kind int

const (
	// KindOther tools are matched by name and parameter rules only.
	KindOther Kind = iota
	// KindRead tools read files; Read rules apply to their paths.
	KindRead
	// KindEdit tools write files; Edit rules, and Read deny rules, apply
	// to their paths.
	KindEdit
	// KindBash tools run shell commands; Bash rules apply.
	KindBash
	// KindFetch tools fetch URLs; WebFetch rules apply to the host.
	KindFetch
)

// Access describes what a tool call touches, as rules see it.
type Access struct {
	Kind Kind
	// Paths are the files of a KindRead or KindEdit call, relative to
	// Config.Dir or absolute.
	Paths []string
	// Command is the command line of a KindBash call.
	Command string
	// URL is the URL of a KindFetch call.
	URL string
}

// builtinAccess returns the access of a call of an agentx built-in tool.
func builtinAccess(name string, args json.RawMessage) (Access, bool) {
	var in struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		URL     string `json:"url"`
	}
	var kind Kind
	switch name {
	case "bash":
		kind = KindBash
	case "read_file", "glob", "grep":
		kind = KindRead
	case "write_file", "edit_file":
		kind = KindEdit
	case "web_fetch":
		kind = KindFetch
	case "apply_patch":
		return Access{Kind: KindEdit, Paths: toolnames.PatchPaths(args)}, true
	default:
		return Access{}, false
	}
	acc := Access{Kind: kind}
	if json.Unmarshal(args, &in) != nil {
		return acc, true
	}
	switch kind {
	case KindBash:
		acc.Command = in.Command
	case KindFetch:
		acc.URL = in.URL
	default:
		p := in.Path
		if p == "" && name != "read_file" && name != "write_file" && name != "edit_file" {
			p = "."
		}
		if p != "" {
			acc.Paths = []string{p}
		}
	}
	return acc, true
}

// Rules lists permission rules in Claude Code syntax, for example from one
// settings file.
type Rules struct {
	Allow, Ask, Deny []string
	// Base is the directory "/path" patterns are relative to: for Claude
	// Code's user settings ~/.claude, and for a settings file passed with
	// --settings that file's directory. Empty means the working directory,
	// as for project settings. A relative Base is relative to Config.Dir.
	Base string
}

// Config configures a Policy.
type Config struct {
	// Mode is the default permission mode; empty means ModeDefault.
	Mode Mode
	// Dir is the working directory, against which relative paths in tool
	// calls and rules resolve. Use the Dir of the built-in tools. The
	// default is the current directory.
	Dir string
	// AdditionalDirectories extend the working directories, as Claude
	// Code's additionalDirectories setting does: acceptEdits mode
	// approves edits in them too. Relative entries are relative to Dir.
	AdditionalDirectories []string
	// Rules are the permission rules, for example one Rules per settings
	// file. A "!" carve-out applies only within its own Rules.
	Rules []Rules
	// Home is the home directory for "~/" patterns; the default is the
	// user's home directory.
	Home string
	// AllowBypassPermissions lets runs switch to ModeBypassPermissions
	// although Mode is another mode. Without it, as in Claude Code, a run
	// can use bypassPermissions only if the policy started in it.
	AllowBypassPermissions bool
	// DisableBypassPermissionsMode forbids ModeBypassPermissions, as
	// Claude Code's disableBypassPermissionsMode setting does.
	DisableBypassPermissionsMode bool
	// Classify, if set, describes calls of tools other than the built-in
	// ones, so path, command and domain rules apply to them; it reports
	// false for tools it does not know. It may also override a built-in
	// tool's access.
	Classify func(name string, args json.RawMessage) (Access, bool)
}

// Run value keys used by the policy.
const (
	// ModeKey holds the run's permission mode, if it is not Config.Mode.
	ModeKey = "permission.mode"
	// GrantsKey holds the rules remembered for the run, as a JSON array.
	GrantsKey = "permission.grants"
	// SessionGrantsKey holds the rules remembered for the session. Its
	// prefix makes agent.Runner keep it with the session.
	SessionGrantsKey = loop.SessionValuePrefix + "permission.grants"
)

// Policy decides tool calls with permission rules and a mode. It is
// runtime middleware; see the package documentation. It is safe for
// concurrent use.
type Policy struct {
	mode     Mode
	bypassOK bool
	classify func(string, json.RawMessage) (Access, bool)
	// dir and home are native absolute paths, dirSlash and homeSlash their
	// slash-separated forms.
	dir, home           string
	dirSlash, homeSlash string
	// dirAnchors and homeAnchors are their slash-separated forms as given
	// and with symbolic links resolved.
	dirAnchors, homeAnchors []string
	// workDirs are the slash-separated working directories, as given and
	// with symbolic links resolved; extraDirs the additional ones only.
	workDirs, extraDirs []string
	sources             []*source
}

// source holds the parsed rules of one Rules.
type source struct {
	allow, ask, deny []*rule
}

func (s *source) list(k listKind) []*rule {
	switch k {
	case listAsk:
		return s.ask
	case listDeny:
		return s.deny
	}
	return s.allow
}

// noHome anchors "~/" patterns when the home directory is unknown, so they
// match nothing.
const noHome = "/\x00home"

// New returns a policy. It fails for invalid rules and modes.
func New(cfg Config) (*Policy, error) {
	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}
	if mode == ModeBypassPermissions && cfg.DisableBypassPermissionsMode {
		return nil, errors.New("permission: bypassPermissions mode is disabled")
	}
	p := &Policy{
		mode:     mode,
		bypassOK: !cfg.DisableBypassPermissionsMode && (mode == ModeBypassPermissions || cfg.AllowBypassPermissions),
		classify: cfg.Classify,
	}
	dir := cfg.Dir
	if dir == "" {
		dir = "."
	}
	if p.dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("permission: %w", err)
	}
	p.dirSlash = toSlash(p.dir)
	p.home = cfg.Home
	if p.home == "" {
		p.home, _ = os.UserHomeDir()
	}
	p.homeSlash = noHome
	p.homeAnchors = []string{noHome}
	if p.home != "" {
		p.home = filepath.Clean(p.home)
		p.homeSlash = toSlash(p.home)
		p.homeAnchors = p.variants(p.home)
	}
	p.dirAnchors = p.variants(p.dir)
	p.workDirs = p.variants(p.dir)
	for _, d := range cfg.AdditionalDirectories {
		abs := p.resolve(d, p.dir)
		vs := p.variants(abs)
		p.workDirs = append(p.workDirs, vs...)
		p.extraDirs = append(p.extraDirs, vs...)
	}
	var errs []error
	for _, rs := range cfg.Rules {
		base := p.dirAnchors
		if rs.Base != "" {
			base = p.variants(p.resolve(rs.Base, p.dir))
		}
		src := &source{}
		for _, l := range []struct {
			kind  listKind
			rules []string
			dst   *[]*rule
		}{{listAllow, rs.Allow, &src.allow}, {listAsk, rs.Ask, &src.ask}, {listDeny, rs.Deny, &src.deny}} {
			for _, text := range l.rules {
				r, err := parseRule(text, l.kind)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				r.anchor(base, p.dirAnchors, p.homeAnchors)
				*l.dst = append(*l.dst, r)
			}
		}
		p.sources = append(p.sources, src)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("permission: %w", err)
	}
	return p, nil
}

// MustNew is like New but panics on error.
func MustNew(cfg Config) *Policy {
	p, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return p
}

// Middleware returns the policy of cfg as runtime middleware; it is New
// with the result typed as loop.Middleware.
func Middleware(cfg Config) (loop.Middleware, error) {
	p, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// resolve makes a path from configuration absolute, expanding a leading
// "~/".
func (p *Policy) resolve(path, dir string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if p.home != "" {
			path = filepath.Join(p.home, path[1:])
		}
	}
	return joinPath(dir, path)
}

// variants returns the slash-separated forms of an absolute path as given
// and with symbolic links resolved.
func (p *Policy) variants(abs string) []string {
	out := []string{toSlash(abs)}
	if r := toSlash(resolveLinks(abs)); r != out[0] {
		out = append(out, r)
	}
	return out
}

// target is a file a call reads or writes.
type target struct {
	// cands are the slash-separated path as requested and, if different,
	// with symbolic links resolved.
	cands []string
	isDir bool
	write bool
	// known is false for a path that could not be determined, such as one
	// with an expansion; such paths are never approved by rules.
	known bool
	// redirect is set for redirection and tee targets of commands.
	redirect bool
}

func (p *Policy) target(native string, write bool) target {
	return target{cands: p.variants(native), isDir: isDir(native), write: write, known: true}
}

// call is a tool call prepared for evaluation.
type call struct {
	name   string
	info   tool.Info
	known  bool
	args   json.RawMessage
	kind   Kind
	bash   *bashCall
	files  []target
	host   string
	params map[string]json.RawMessage
}

func (p *Policy) prepare(req *Request) *call {
	c := &call{name: req.Name, info: req.Info, known: req.Known, args: req.Arguments}
	acc, ok := Access{}, false
	if p.classify != nil {
		acc, ok = p.classify(req.Name, req.Arguments)
	}
	if !ok {
		acc, _ = builtinAccess(req.Name, req.Arguments)
	}
	c.kind = acc.Kind
	switch acc.Kind {
	case KindBash:
		c.bash = analyzeBash(acc.Command, p.dir, p.home)
		c.files = p.bashFiles(c.bash)
	case KindRead, KindEdit:
		for _, path := range acc.Paths {
			c.files = append(c.files, p.target(joinPath(p.dir, path), acc.Kind == KindEdit))
		}
	case KindFetch:
		c.host = urlHost(acc.URL)
	}
	return c
}

// bashFiles returns the files the commands of bc read and write.
func (p *Policy) bashFiles(bc *bashCall) []target {
	var out []target
	for _, s := range bc.subs {
		for _, fa := range s.files() {
			t := target{write: fa.write, redirect: fa.redirect || fa.tee}
			lit := fa.w.lit
			tilde := lit == "~" || strings.HasPrefix(lit, "~/")
			if (literal(fa.w) || tilde && !fa.w.glob && !strings.Contains(lit, "$")) && lit != "" {
				dir := s.cwd
				t.known = dir != "" || filepath.IsAbs(lit) || tilde
				if dir == "" {
					// Best effort for deny and ask rules.
					dir = p.dir
				}
				native := joinPath(dir, lit)
				if tilde {
					t.known = t.known && !fa.redirect
					native = p.resolve(lit, dir)
				}
				t.cands, t.isDir = p.variants(native), isDir(native)
			}
			out = append(out, t)
		}
	}
	return out
}

// param returns a top-level argument of the call.
func (c *call) param(name string) (json.RawMessage, bool) {
	if c.params == nil {
		c.params = map[string]json.RawMessage{}
		_ = json.Unmarshal(c.args, &c.params)
	}
	v, ok := c.params[name]
	return v, ok
}

// hasProperty reports whether the tool has a top-level parameter name.
func (c *call) hasProperty(name string) bool {
	if !c.known {
		return false
	}
	if c.info.Parameters != nil {
		_, ok := c.info.Parameters.Properties[name]
		return ok
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(c.info.RawParameters, &schema) != nil {
		return false
	}
	_, ok := schema.Properties[name]
	return ok
}

// paramRule reports whether r is a parameter rule for this call.
func (c *call) paramRule(r *rule) bool {
	return r.param != "" && !primaryParams[r.param] && c.hasProperty(r.param)
}

// paramMatches reports whether the call's argument matches a parameter
// rule. An omitted parameter never matches.
func (c *call) paramMatches(r *rule) bool {
	raw, ok := c.param(r.param)
	if !ok {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		switch v.(type) {
		case bool, float64:
			s = strings.TrimSpace(string(raw))
		default:
			return false
		}
	}
	return fsmatch.Wildcard(r.value, s)
}

// applies reports whether r selects the call. Read deny rules also apply
// to edits.
func (c *call) applies(r *rule) bool {
	return r.selects(c.name, c.kind) || r.list == listDeny && c.kind == KindEdit && r.kind == KindRead
}

// Request is a tool call to decide.
type Request struct {
	// Mode is the permission mode; empty means the policy's mode.
	Mode Mode
	Name string
	// Info describes the tool; Known is false for unknown tools.
	Info      tool.Info
	Known     bool
	Arguments json.RawMessage
	// Grants are remembered allow rules.
	Grants []string
	// Prior is the decision of the middleware that ran before the policy,
	// such as hooks. In dontAsk mode a prior Allowed lets a call that
	// needs approval run, and a prior Asked denies the call.
	Prior loop.Decision
}

// Verdict is the decision on a call.
type Verdict struct {
	// Decision is loop.NoDecision when the policy leaves the call to the
	// tool's RequiresApproval and to other middleware.
	Decision loop.Decision
	// Reason explains a denial to the model or a request to the approver.
	Reason string
}

// Decide decides a call.
func (p *Policy) Decide(req *Request) Verdict {
	mode := req.Mode
	if mode == "" {
		mode = p.mode
	}
	return p.decide(mode, p.grantRules(req.Grants), req.Prior, p.prepare(req))
}

func (p *Policy) decide(mode Mode, grants *source, prior loop.Decision, c *call) Verdict {
	deny := func(reason string) Verdict { return Verdict{Decision: loop.Denied, Reason: reason} }
	ask := func(reason string) Verdict {
		if mode == ModeDontAsk {
			return deny(reason + " Permission mode dontAsk denies calls that would ask for approval.")
		}
		return Verdict{Decision: loop.Asked, Reason: reason}
	}
	if r := p.match(c, listDeny); r != nil {
		return deny(fmt.Sprintf("Permission to use %s has been denied by the permission rule %q.", c.name, r.text))
	}
	if r := p.match(c, listAsk); r != nil {
		return ask(fmt.Sprintf("The permission rule %q requires approval for this call.", r.text))
	}
	if target := p.criticalRemoval(c); target != "" {
		return ask(fmt.Sprintf("This command removes %s, a critical path; removals of critical paths always need approval.", target))
	}
	if mode == ModeBypassPermissions {
		return Verdict{Decision: loop.Allowed}
	}
	if mode == ModePlan && !p.readOnly(c) {
		return deny(fmt.Sprintf("Plan mode is active: while planning, only read-only tools and commands run, so this %s call was not made. "+
			"Finish exploring and present your plan; the user leaves plan mode to carry it out.", c.name))
	}
	if path := p.protectedWrite(c); path != "" {
		return ask(fmt.Sprintf("Writing %s needs approval: it is a protected path.", path))
	}
	if p.allowed(c, mode, grants) {
		return Verdict{Decision: loop.Allowed}
	}
	if mode == ModeDontAsk {
		needs := c.known && c.info.RequiresApproval && prior != loop.Allowed || prior == loop.Asked
		if needs {
			return deny(fmt.Sprintf("Permission mode dontAsk denies calls that need approval, and no allow rule approves this %s call.", c.name))
		}
	}
	return Verdict{}
}

// match returns the first rule of list, across the sources, that matches
// the call.
func (p *Policy) match(c *call, list listKind) *rule {
	for _, src := range p.sources {
		if r := p.matchSource(c, src.list(list)); r != nil {
			return r
		}
	}
	return nil
}

func (p *Policy) matchSource(c *call, rules []*rule) *rule {
	var pathRules []*rule
	for _, r := range rules {
		if !c.applies(r) {
			continue
		}
		switch {
		case !r.hasSpec:
			return r
		case c.paramRule(r):
			if c.paramMatches(r) {
				return r
			}
		case c.kind == KindBash:
			if r.bash != nil && c.bash.matchAny(r.bash) {
				return r
			}
		case c.kind == KindFetch:
			if r.domain != nil && r.domain.match(c.host) {
				return r
			}
		case c.kind == KindRead || c.kind == KindEdit:
			if r.path != nil {
				pathRules = append(pathRules, r)
			}
		}
	}
	if c.kind == KindRead || c.kind == KindEdit {
		for _, t := range c.files {
			if r := matchTarget(pathRules, t, true); r != nil {
				return r
			}
		}
	}
	if c.kind == KindBash {
		// Files the commands read and write meet Read and Edit rules.
		for _, t := range c.files {
			if !t.known && len(t.cands) == 0 {
				continue
			}
			var kindRules []*rule
			for _, r := range rules {
				if r.kind == KindRead && (!t.write || r.list == listDeny) || r.kind == KindEdit && t.write {
					if !r.hasSpec {
						return r
					}
					if r.path != nil {
						kindRules = append(kindRules, r)
					}
				}
			}
			if r := matchTarget(kindRules, t, true); r != nil {
				return r
			}
		}
	}
	return nil
}

// matchTarget matches the path rules against a file, grouped by the tool
// they name so that carve-outs apply within their group. With any, one
// matching candidate suffices; otherwise every candidate must match.
func matchTarget(rules []*rule, t target, any bool) *rule {
	if len(rules) == 0 || len(t.cands) == 0 {
		return nil
	}
	var groups [][]*rule
	index := map[string]int{}
	for _, r := range rules {
		i, ok := index[r.tool]
		if !ok {
			i = len(groups)
			index[r.tool] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], r)
	}
	var found *rule
	for _, cand := range t.cands {
		var m *rule
		for _, g := range groups {
			if m = excludedBy(g, cand, t.isDir); m != nil {
				break
			}
		}
		switch {
		case m != nil && any:
			return m
		case m == nil && !any:
			return nil
		}
		found = m
	}
	return found
}

// matchAny reports whether any form of any command matches a deny or ask
// pattern. For a command that could not be split reliably, the whole text
// is matched too.
func (bc *bashCall) matchAny(bp *bashPattern) bool {
	for _, s := range bc.subs {
		if slices.ContainsFunc(s.forms, bp.match) {
			return true
		}
	}
	return bc.bad && bp.match(bc.raw)
}

// inWorkDirs reports whether every candidate of t is inside a working
// directory.
func (p *Policy) inWorkDirs(t target) bool {
	if !t.known || len(t.cands) == 0 {
		return false
	}
	for _, cand := range t.cands {
		if !slices.ContainsFunc(p.workDirs, func(d string) bool { return inside(cand, d) }) {
			return false
		}
	}
	return true
}

// allowed reports whether allow rules, grants or the mode approve the
// call.
func (p *Policy) allowed(c *call, mode Mode, grants *source) bool {
	srcs := p.sources
	if grants != nil {
		srcs = append(slices.Clone(srcs), grants)
	}
	if c.kind == KindBash {
		return p.bashAllowed(c, mode, srcs)
	}
	var pathRules []*rule
	for _, src := range srcs {
		for _, r := range src.allow {
			if !c.applies(r) {
				continue
			}
			switch {
			case !r.hasSpec:
				return true
			case c.kind == KindFetch:
				if r.domain != nil && r.domain.match(c.host) {
					return true
				}
			case c.kind == KindRead || c.kind == KindEdit:
				if r.path != nil {
					pathRules = append(pathRules, r)
				}
			}
		}
	}
	if c.kind != KindRead && c.kind != KindEdit || len(c.files) == 0 {
		return false
	}
	for _, t := range c.files {
		if matchTarget(pathRules, t, false) != nil {
			continue
		}
		if mode == ModeAcceptEdits && c.kind == KindEdit && p.inWorkDirs(t) {
			continue
		}
		return false
	}
	return true
}

// bashAllowed reports whether every command of a shell call is approved.
func (p *Policy) bashAllowed(c *call, mode Mode, srcs []*source) bool {
	bc := c.bash
	if bc.bad || len(bc.subs) == 0 {
		return false
	}
	gitAway := p.cdAwayWithGit(bc)
	for _, s := range bc.subs {
		if s.inline {
			continue
		}
		switch {
		case p.ruleAllows(c, s, srcs):
		case s.readOnly(p.inWorkDir) && (!gitAway || s.program() != "git"):
		case mode == ModeAcceptEdits && p.editInScope(s):
		default:
			return false
		}
	}
	// Redirection and tee targets need their own approval.
	var reads, writes []*rule
	for _, src := range srcs {
		for _, r := range src.allow {
			switch r.kind {
			case KindRead:
				reads = append(reads, r)
			case KindEdit:
				writes = append(writes, r)
			}
		}
	}
	for _, t := range c.files {
		if !t.redirect {
			continue
		}
		rules := reads
		if t.write {
			rules = writes
		}
		switch {
		case !t.known:
			return false
		case slices.ContainsFunc(rules, func(r *rule) bool { return !r.hasSpec }):
		case matchTarget(withPaths(rules), t, false) != nil:
		case !t.write && p.inWorkDirs(t):
		case t.write && mode == ModeAcceptEdits && p.inWorkDirs(t):
		default:
			return false
		}
	}
	return true
}

func withPaths(rules []*rule) []*rule {
	var out []*rule
	for _, r := range rules {
		if r.path != nil {
			out = append(out, r)
		}
	}
	return out
}

// ruleAllows reports whether an allow rule or grant approves a command. A
// rule with a wildcard does not approve exec wrappers such as watch, or
// find with an action such as -exec or -delete.
func (p *Policy) ruleAllows(c *call, s *subcommand, srcs []*source) bool {
	prog := s.program()
	risky := execWrappers[prog] || prog == "find" && slices.ContainsFunc(s.args, func(w word) bool { return findActions[w.lit] })
	for _, src := range srcs {
		for _, r := range src.allow {
			if !c.applies(r) {
				continue
			}
			if !r.hasSpec {
				return true
			}
			if r.bash != nil && r.bash.match(s.text) && (!r.bash.wild || !risky) {
				return true
			}
		}
	}
	return false
}

// inWorkDir reports whether a native path is inside the working
// directories.
func (p *Policy) inWorkDir(native string) bool {
	return p.inWorkDirs(target{cands: p.variants(native), known: true})
}

// cdAwayWithGit reports whether a command line changes directory and runs
// git, which can run the hooks of the repository it changed to.
func (p *Policy) cdAwayWithGit(bc *bashCall) bool {
	away, git := false, false
	for _, s := range bc.subs {
		switch s.program() {
		case "cd":
			if len(s.args) != 2 || !literal(s.args[1]) || s.cwd == "" || joinPath(s.cwd, s.args[1].lit) != p.dir {
				away = true
			}
		case "git":
			git = true
		}
	}
	return away && git
}

// editInScope reports whether a command is a filesystem command that
// acceptEdits mode approves: mkdir, touch, rm, rmdir, mv or cp with every
// path inside the working directories.
func (p *Policy) editInScope(s *subcommand) bool {
	if !s.safeEnv || !editPrograms[s.program()] {
		return false
	}
	for _, w := range operands(s.args[1:]) {
		if w.expand || w.subst || w.lit == "" || s.cwd == "" && !filepath.IsAbs(w.lit) {
			return false
		}
		if !p.inWorkDir(joinPath(cmp.Or(s.cwd, p.dir), w.lit)) {
			return false
		}
	}
	return true
}

// planTools are the built-in tools that plan mode allows although they
// are not read-only, since they only change the agent's own state, as
// Claude Code runs BashOutput, KillShell and TodoWrite without permission:
// bash_output consumes the output it reads, kill_shell stops a background
// command the agent started, and todo_write updates the agent's task list.
var planTools = map[string]bool{"bash_output": true, "kill_shell": true, "todo_write": true}

// readOnly reports whether a call only reads, as plan mode requires: a
// read-only tool, one of planTools, or a shell command line whose
// commands are all read-only and that writes no files.
func (p *Policy) readOnly(c *call) bool {
	if c.kind != KindBash {
		return !c.known || c.info.ReadOnly || planTools[c.name]
	}
	bc := c.bash
	if bc.bad || len(bc.subs) == 0 {
		return false
	}
	gitAway := p.cdAwayWithGit(bc)
	for _, s := range bc.subs {
		if s.inline {
			continue
		}
		if !s.readOnly(p.inWorkDir) || s.writesRedirect() || gitAway && s.program() == "git" {
			return false
		}
	}
	return true
}

// protectedWrite returns a protected path the call writes, or "".
func (p *Policy) protectedWrite(c *call) string {
	for _, t := range c.files {
		if !t.write {
			continue
		}
		for _, cand := range t.cands {
			rel := cand
			for _, d := range p.workDirs {
				if inside(cand, d) && cand != d {
					rel = strings.TrimPrefix(cand, d)
					break
				}
			}
			if protected(rel) {
				return cand
			}
		}
	}
	return ""
}

// criticalRemoval returns the critical path an rm or rmdir in a shell call
// targets, or "".
func (p *Policy) criticalRemoval(c *call) string {
	if c.kind != KindBash {
		return ""
	}
	for _, s := range c.bash.subs {
		prog := filepath.Base(s.program())
		if prog != "rm" && prog != "rmdir" {
			continue
		}
		args := s.args[1:]
		recursive := prog == "rm" && recursiveRemoval(args)
		for _, w := range operands(args) {
			if criticalTarget(w, recursive, s.cwd, p.home, p.critical, p.globCritical) {
				return w.raw
			}
		}
	}
	return ""
}

// critical reports whether a native path is critical: the filesystem
// root, a top-level directory, the home directory, or the working
// directory or one of its parents.
func (p *Policy) critical(native string) bool {
	s := toSlash(filepath.Clean(native))
	depth := strings.Count(strings.Trim(s, "/"), "/") + 1
	if s == "/" {
		depth = 0
	}
	limit := 1
	if runtime.GOOS == "windows" {
		// /c is a drive root and /c/Windows a top-level directory.
		limit = 2
	}
	if depth <= limit || s == p.homeSlash {
		return true
	}
	for _, d := range p.variants(p.dir) {
		if inside(d, s) {
			return true
		}
	}
	return false
}

// globCritical reports whether a glob directly under a native path is
// critical: under a critical path, or an additional working directory or
// one of its parents.
func (p *Policy) globCritical(native string) bool {
	if p.critical(native) {
		return true
	}
	s := toSlash(filepath.Clean(native))
	return slices.ContainsFunc(p.extraDirs, func(d string) bool { return inside(d, s) })
}

// grantRules parses remembered rules into a source of allow rules.
// Invalid rules are skipped.
func (p *Policy) grantRules(grants []string) *source {
	if len(grants) == 0 {
		return nil
	}
	src := &source{}
	for _, text := range grants {
		r, err := parseRule(text, listAllow)
		if err != nil {
			continue
		}
		r.anchor(p.dirAnchors, p.dirAnchors, p.homeAnchors)
		src.allow = append(src.allow, r)
	}
	return src
}

// Name implements loop.Middleware.
func (p *Policy) Name() string { return "permission" }

// PermissionMode returns the run's mode as a string. It lets agent.New
// report the mode to hooks; see hook.Manager.SetPermissionMode.
func (p *Policy) PermissionMode(run *loop.Run) string { return string(p.Mode(run)) }

// Mode returns the permission mode of a run: the run value ModeKey if it
// holds a mode the policy permits, and Config.Mode otherwise.
func (p *Policy) Mode(run *loop.Run) Mode {
	v, ok := run.Value(ModeKey)
	if !ok {
		return p.mode
	}
	m, err := ParseMode(v)
	if err == nil && m == ModeBypassPermissions && !p.bypassOK {
		err = errors.New("bypassPermissions mode is not enabled")
	}
	if err != nil {
		run.Logger().Warn("permission: ignoring run mode", "run_id", run.ID(), "mode", v, "error", err)
		return p.mode
	}
	return m
}

// SetMode switches the permission mode of a run, from its next tool call.
// Middleware can call it, for example to leave plan mode once a plan was
// approved. It fails for an invalid mode and for bypassPermissions unless
// the policy started in it or Config.AllowBypassPermissions is set.
func (p *Policy) SetMode(run *loop.Run, m Mode) error {
	m, err := ParseMode(string(m))
	if err != nil {
		return fmt.Errorf("permission: %w", err)
	}
	if m == ModeBypassPermissions && !p.bypassOK {
		return errors.New("permission: bypassPermissions mode is not enabled")
	}
	run.SetValue(ModeKey, string(m))
	return nil
}

// Grants returns the rules remembered for a run: those of the run, then
// those of the session.
func (p *Policy) Grants(run *loop.Run) []string {
	return append(decodeGrants(run, GrantsKey), decodeGrants(run, SessionGrantsKey)...)
}

// ClearGrants forgets the rules remembered for a run and, if session is
// set, for its session.
func (p *Policy) ClearGrants(run *loop.Run, session bool) {
	run.SetValue(GrantsKey, "")
	if session {
		run.SetValue(SessionGrantsKey, "")
	}
}

func decodeGrants(run *loop.Run, key string) []string {
	rules, _, err := run.DecodeValue[[]string](key)
	if err != nil {
		run.Logger().Warn("permission: ignoring malformed grants", "run_id", run.ID(), "key", key, "error", err)
		return nil
	}
	return rules
}

// BeforeTool implements loop.BeforeTooler.
func (p *Policy) BeforeTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	prior, _ := plan.Decision()
	mode := p.Mode(run)
	if _, ok := run.Value(ModeKey); !ok && mode == ModePlan {
		// Record plan mode so that it holds for the agents the run is
		// handed off to, whatever their policies' default modes.
		run.SetValue(ModeKey, string(ModePlan))
	}
	v := p.Decide(&Request{
		Mode:      mode,
		Name:      plan.Call.Name,
		Info:      plan.Info,
		Known:     plan.Known,
		Arguments: plan.Call.Arguments,
		Grants:    p.Grants(run),
		Prior:     prior,
	})
	switch v.Decision {
	case loop.Denied:
		plan.Deny(v.Reason)
	case loop.Asked:
		plan.Ask(v.Reason)
	case loop.Allowed:
		plan.Allow()
	}
	return nil
}

// RevalidateTool applies the policy to arguments edited in an approval.
// An explicit denial still blocks the call; requests for approval have
// already been satisfied by approval of the edited arguments.
func (p *Policy) RevalidateTool(ctx context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	return p.BeforeTool(ctx, run, plan)
}

// AfterTool implements loop.AfterTooler. It remembers approvals that
// ask for it; see loop.Approval.Remember.
func (p *Policy) AfterTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan, _ *model.ToolResult) error {
	a, ok := plan.Approval()
	if !ok || !a.Approved || a.Remember == "" {
		return nil
	}
	var key string
	switch a.Remember {
	case loop.RememberRun:
		key = GrantsKey
	case loop.RememberSession:
		key = SessionGrantsKey
	default:
		run.Logger().Warn("permission: unknown remember scope", "run_id", run.ID(), "scope", a.Remember)
		return nil
	}
	rules := a.RememberRules
	if len(rules) == 0 {
		rules = p.Suggest(&Request{Mode: p.Mode(run), Name: plan.Call.Name, Info: plan.Info, Known: plan.Known,
			Arguments: plan.Call.Arguments, Grants: p.Grants(run)})
	}
	have := decodeGrants(run, key)
	for _, text := range rules {
		if _, err := parseRule(text, listAllow); err != nil {
			run.Logger().Warn("permission: not remembering invalid rule", "run_id", run.ID(), "error", err)
			continue
		}
		if !slices.Contains(have, text) {
			have = append(have, text)
		}
	}
	if len(have) > 0 {
		_ = run.EncodeValue(key, have)
	}
	return nil
}

// Suggest returns allow rules that would approve a call like req: for a
// shell command, the exact commands that need approval and the redirection
// targets outside the allowed paths; for a file tool, the exact paths; for
// a fetch, the domain; for other tools, the tool. A command that contains
// "*" is left out, since a rule for it would be a wildcard pattern.
func (p *Policy) Suggest(req *Request) []string {
	mode := cmp.Or(string(req.Mode), string(p.mode))
	c := p.prepare(req)
	grants := p.grantRules(req.Grants)
	srcs := p.sources
	if grants != nil {
		srcs = append(slices.Clone(srcs), grants)
	}
	var out []string
	add := func(r string) {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	switch c.kind {
	case KindBash:
		if c.bash.bad {
			return nil
		}
		gitAway := p.cdAwayWithGit(c.bash)
		for _, s := range c.bash.subs {
			if s.inline || p.ruleAllows(c, s, srcs) || s.readOnly(p.inWorkDir) && (!gitAway || s.program() != "git") ||
				Mode(mode) == ModeAcceptEdits && p.editInScope(s) || strings.Contains(s.text, "*") || s.text == "" {
				continue
			}
			add("Bash(" + s.text + ")")
		}
		for _, t := range c.files {
			if t.redirect && t.known && (t.write || !p.inWorkDirs(t)) {
				add(pathRule(t))
			}
		}
	case KindRead, KindEdit:
		for _, t := range c.files {
			add(pathRule(t))
		}
	case KindFetch:
		if _, err := parseDomain(c.host); err == nil {
			add("WebFetch(domain:" + normalizeHost(c.host) + ")")
		}
	default:
		if _, err := parseRule(c.name, listAllow); err == nil {
			add(c.name)
		}
	}
	return out
}

// pathRule returns a rule for exactly the file t.
func pathRule(t target) string {
	kind := "Read"
	if t.write {
		kind = "Edit"
	}
	return kind + "(//" + escapePath(strings.TrimPrefix(t.cands[0], "/")) + ")"
}

// snapshot is what CheckRedirect needs to know about the call it runs in.
type snapshot struct {
	mode   Mode
	grants []string
	name   string
	info   tool.Info
}

type snapshotKey struct{}

// WrapTool implements loop.ToolWrapper. It records the run's mode and
// grants in the context of each call for CheckRedirect, and carries path
// restrictions into recursive file searches.
func (p *Policy) WrapTool(run *loop.Run, next tool.Invoker) tool.Invoker {
	return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
		snap := &snapshot{mode: p.Mode(run), grants: p.Grants(run), name: call.Name}
		if call.Tool != nil {
			snap.info = call.Tool.Info()
		}
		ctx = context.WithValue(ctx, snapshotKey{}, snap)
		ctx = fileaccess.WithReadFilter(ctx, func(path string) bool {
			return p.searchReadable(call.Name, path)
		})
		return next(ctx, call)
	}
}

// searchReadable checks only path rules: tool-wide and argument approval
// was already handled before execution. A discovered file cannot obtain a
// new approval mid-call. Do not prune directories, since a later carve-out
// may allow a file beneath one.
func (p *Policy) searchReadable(name, path string) bool {
	c := &call{name: name, kind: KindRead}
	t := p.target(path, false)
	for _, list := range []listKind{listDeny, listAsk} {
		for _, src := range p.sources {
			var paths []*rule
			for _, r := range src.list(list) {
				if r.path != nil && c.applies(r) {
					paths = append(paths, r)
				}
			}
			if matchTarget(paths, t, true) != nil {
				return false
			}
		}
	}
	return true
}

// CheckRedirect applies the policy to a redirect of a fetch, with the
// signature of http.Client.CheckRedirect; pass it as
// builtin.Config.CheckRedirect. A redirect to the host of the original
// request is followed, since that host was decided before the fetch. A
// redirect to another host is refused if the policy would deny a fetch of
// it or ask for approval, which cannot be given in the middle of a call;
// the model can then fetch the new URL itself, so that it is approved on
// its own. Within a call run by the policy's runtime, the run's mode and
// remembered rules apply; otherwise Config.Mode does.
func (p *Policy) CheckRedirect(req *http.Request, via []*http.Request) error {
	host := req.URL.Hostname()
	if len(via) > 0 && normalizeHost(via[0].URL.Hostname()) == normalizeHost(host) {
		return nil
	}
	snap, _ := req.Context().Value(snapshotKey{}).(*snapshot)
	if snap == nil {
		snap = &snapshot{mode: p.mode, name: "web_fetch", info: tool.Info{Name: "web_fetch", ReadOnly: true}}
	}
	c := &call{name: snap.name, info: snap.info, known: true, kind: KindFetch, host: host}
	v := p.decide(snap.mode, p.grantRules(snap.grants), loop.NoDecision, c)
	switch v.Decision {
	case loop.Denied:
		return fmt.Errorf("redirect to %s refused: %s", req.URL, v.Reason)
	case loop.Asked:
		return fmt.Errorf("redirect to %s needs approval (%s); fetch that URL directly to ask for it", req.URL, v.Reason)
	}
	return nil
}

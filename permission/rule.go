package permission

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nostosx/agentx/internal/fsmatch"

	"github.com/nostosx/agentx/internal/toolnames"
)

// listKind is the list a rule appears in.
type listKind int

const (
	listAllow listKind = iota
	listAsk
	listDeny
)

func (k listKind) String() string {
	switch k {
	case listAsk:
		return "ask"
	case listDeny:
		return "deny"
	}
	return "allow"
}

// rule is a parsed permission rule in Claude Code syntax: Tool or
// Tool(specifier). See https://code.claude.com/docs/en/permissions#permission-rule-syntax.
type rule struct {
	text string
	list listKind
	// tool is the tool name as written.
	tool string
	// kind selects the tools of a kind, for the Claude Code names Bash,
	// Read, Edit and WebFetch.
	kind Kind
	// names selects tools by exact name, glob by a tool-name glob.
	names []string
	glob  string
	// spec is the specifier; hasSpec is false for a bare tool name.
	spec    string
	hasSpec bool

	// The interpretations of the specifier. A rule for a tool given by
	// name has each one that parses; the call's kind picks one.
	bash   *bashPattern
	pspec  *pathSpec
	path   *pathPattern
	domain *domainPattern
	// param and value make a parameter rule, Tool(param:value), which
	// applies when the tool has a top-level parameter param.
	param, value string
}

// claudeKinds maps the Claude Code tool names whose rules select a kind
// of tool rather than one tool.
var claudeKinds = map[string]Kind{
	"Bash":     KindBash,
	"Read":     KindRead,
	"Edit":     KindEdit,
	"WebFetch": KindFetch,
}

// pathToolHint names the rule to use instead of a path rule for a tool
// whose path rules Claude Code never consults.
var pathToolHint = map[string]string{
	"Write":        "Edit",
	"MultiEdit":    "Edit",
	"NotebookEdit": "Edit",
	"Glob":         "Read",
	"Grep":         "Read",
}

// primaryParams are the parameters holding a tool's primary content, which
// parameter rules cannot match: a rule such as Bash(command:rm *) would be
// bypassable by a compound command.
var primaryParams = map[string]bool{"command": true, "file_path": true, "path": true, "notebook_path": true, "url": true}

var paramSpec = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(.*)$`)

// parseRule parses a rule of list.
func parseRule(text string, list listKind) (*rule, error) {
	r, err := parseRuleText(text, list)
	if err != nil {
		return nil, fmt.Errorf("permission rule %q: %w", text, err)
	}
	return r, nil
}

func parseRuleText(text string, list listKind) (*rule, error) {
	r := &rule{text: strings.TrimSpace(text), list: list}
	s := r.text
	if s == "" {
		return nil, errors.New("empty rule")
	}
	if i := strings.IndexByte(s, '('); i >= 0 {
		if !strings.HasSuffix(s, ")") {
			return nil, errors.New("missing closing parenthesis")
		}
		r.tool, r.spec, r.hasSpec = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:len(s)-1]), true
	} else {
		r.tool = s
	}
	if r.tool == "" || strings.ContainsAny(r.tool, " \t()") {
		return nil, errors.New("invalid tool name")
	}
	if r.hasSpec && r.spec == "" {
		return nil, errors.New("empty specifier")
	}
	if err := r.parseTool(); err != nil {
		return nil, err
	}
	if r.hasSpec {
		if err := r.parseSpec(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// parseTool parses the tool name.
func (r *rule) parseTool() error {
	name := r.tool
	if strings.Contains(name, "*") {
		if r.hasSpec {
			return errors.New("a tool-name glob takes no specifier")
		}
		if r.list == listAllow {
			// Allow globs must name one MCP server.
			server, rest, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__")
			if !strings.HasPrefix(name, "mcp__") || !ok || server == "" || strings.Contains(server, "*") || rest == "" {
				return errors.New(`allow rules accept tool-name globs only after a literal "mcp__<server>__" prefix`)
			}
		}
		r.glob = name
		return nil
	}
	if strings.HasPrefix(name, "mcp__") {
		if r.hasSpec {
			return errors.New("MCP rules take no specifier")
		}
		r.names = []string{name}
		return nil
	}
	if k, ok := claudeKinds[name]; ok {
		r.kind = k
		return nil
	}
	if ex, ok := toolnames.AgentX(name); ok {
		r.names = []string{ex}
		return nil
	}
	if _, ok := pathToolHint[name]; ok {
		// NotebookEdit has no agentx counterpart; its rules match nothing.
		r.names = []string{}
		return nil
	}
	r.names = []string{name}
	return nil
}

// parseSpec parses the specifier according to the tool.
func (r *rule) parseSpec() error {
	if r.kind == KindBash && r.spec == "*" {
		// Bash(*) is the same as Bash.
		r.hasSpec, r.spec = false, ""
		return nil
	}
	param, value, isParam := "", "", false
	if m := paramSpec.FindStringSubmatch(r.spec); m != nil && r.list != listAllow {
		param, value, isParam = m[1], m[2], true
	}
	switch r.kind {
	case KindBash:
		if isParam && param == "command" {
			return errors.New("parameter rules cannot match the command; use Bash(<command pattern>)")
		}
		r.bash = parseBashPattern(r.spec)
	case KindRead, KindEdit:
		if isParam && primaryParams[param] {
			return fmt.Errorf("parameter rules cannot match %s; use %s(<path pattern>)", param, r.tool)
		}
		ps, err := parsePathSpec(r.spec, r.list)
		if err != nil && (r.list == listAllow || !errors.Is(err, errBadPath)) {
			return err
		}
		r.pspec = &ps
	case KindFetch:
		if host, ok := strings.CutPrefix(r.spec, "domain:"); ok {
			d, err := parseDomain(host)
			if err != nil {
				return err
			}
			r.domain = d
			return nil
		}
		if !isParam || param == "url" {
			return errors.New(`WebFetch rules take a "domain:" specifier`)
		}
	default:
		if hint, ok := pathToolHint[r.tool]; ok && !isParam {
			return fmt.Errorf("path rules for %s are never consulted; use %s(%s)", r.tool, hint, r.spec)
		}
		// A tool given by name: keep each interpretation that parses, so
		// the call's kind can pick one.
		r.bash = parseBashPattern(r.spec)
		if ps, err := parsePathSpec(r.spec, r.list); err == nil || (r.list != listAllow && errors.Is(err, errBadPath)) {
			r.pspec = &ps
		}
		if host, ok := strings.CutPrefix(r.spec, "domain:"); ok {
			r.domain, _ = parseDomain(host)
		}
	}
	if isParam {
		r.param, r.value = param, value
	}
	return nil
}

// anchor compiles the rule's path pattern for the policy's directories.
func (r *rule) anchor(base, dir, home []string) {
	if r.pspec != nil {
		r.path = r.pspec.compile(base, dir, home)
	}
}

// selects reports whether the rule applies to calls of the tool name of
// kind k, before its specifier is considered.
func (r *rule) selects(name string, k Kind) bool {
	switch {
	case r.glob != "":
		return fsmatch.Wildcard(r.glob, name)
	case r.kind != KindOther:
		return r.kind == k
	}
	for _, n := range r.names {
		if n == name {
			return true
		}
		if strings.HasPrefix(n, "mcp__") && strings.HasPrefix(name, n+"__") {
			// A server rule such as mcp__github. As an allow rule it
			// names the server before the first "__" only; deny and ask
			// rules also cover servers whose names contain "__".
			if r.list != listAllow {
				return true
			}
			if strings.Count(n, "__") == 1 && !strings.Contains(name[len(n)+2:], "__") {
				return true
			}
		}
	}
	return false
}

// bashPattern is the command pattern of a Bash rule; see
// https://code.claude.com/docs/en/permissions#wildcard-patterns. "*"
// matches any text, including spaces. A pattern without "*" matches one
// exact command. A trailing " *" that is the only wildcard also matches the
// command without it, and a trailing ":*" is the same as " *".
type bashPattern struct {
	pattern string
	wild    bool
	// bare is the command a trailing " *" also matches, or "".
	bare string
}

func parseBashPattern(s string) *bashPattern {
	if p, ok := strings.CutSuffix(s, ":*"); ok {
		s = p + " *"
	}
	bp := &bashPattern{pattern: s, wild: strings.Contains(s, "*")}
	if p, ok := strings.CutSuffix(s, " *"); ok && !strings.Contains(p, "*") {
		bp.bare = p
	}
	return bp
}

func (bp *bashPattern) match(cmd string) bool {
	if !bp.wild {
		return cmd == bp.pattern
	}
	return fsmatch.Wildcard(bp.pattern, cmd) || (bp.bare != "" && cmd == bp.bare)
}

package skill

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/fsmatch"

	"github.com/nostosx/agentx/internal/toolnames"
	"github.com/nostosx/agentx/model"
)

// shellOperators are the command separators and substitutions after which
// a Bash rule no longer describes the whole command.
var shellOperators = []string{"&&", "||", ";", "|", "&", "`", "$(", "<(", ">(", "\n", ">", "<"}

// MatchTool reports whether a tool rule of allowed-tools or
// disallowed-tools, in Claude Code's permission rule syntax
// (https://code.claude.com/docs/en/permissions), matches a tool call. It
// is the default Config.MatchTool.
//
// A rule is a tool name, matching every call of the tool, or a name with a
// specifier in parentheses. Claude Code names select the agentx built-ins
// with the same role (Bash is bash, Read is read_file, and so on), and a
// trailing "*" in the name, or an MCP server prefix such as
// "mcp__github", matches several tools. Specifiers are supported for:
//
//   - Bash: the command, where "*" matches any text and a legacy
//     "prefix:*" any command starting with prefix. A command with shell
//     operators such as &&, ;, | or $( matches no specifier, so a rule
//     cannot approve more than the command it describes.
//   - Read, Write, Edit and MultiEdit: a glob of the file path, relative to
//     root unless it starts with "//" (absolute) or "~/" (home).
//   - WebFetch: "domain:example.com", which also matches subdomains.
//
// Other specifiers match nothing.
func MatchTool(root, rule string, call *model.ToolCall) bool {
	rule = strings.TrimSpace(rule)
	name, spec, hasSpec := strings.Cut(rule, "(")
	if hasSpec {
		if !strings.HasSuffix(spec, ")") {
			return false
		}
		spec = strings.TrimSuffix(spec, ")")
	}
	name = strings.TrimSpace(name)
	if !matchToolName(name, call.Name) {
		return false
	}
	if spec == "" || spec == "*" {
		return true
	}
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := args[k].(string); ok {
				return s
			}
		}
		return ""
	}
	if call.Name == "apply_patch" {
		// A path rule matches a patch only if it matches every file the
		// patch touches; see matchesAnyFile for disallowed-tools.
		files := toolnames.PatchPaths(call.Arguments)
		if len(files) == 0 || !isFileRule(name) {
			return false
		}
		for _, f := range files {
			if !matchFile(root, spec, f) {
				return false
			}
		}
		return true
	}
	switch claudeName(name, call.Name) {
	case "bash":
		return matchCommand(spec, str("command"))
	case "read_file", "write_file", "edit_file":
		file := str("path", "file_path")
		return file != "" && matchFile(root, spec, file)
	case "web_fetch":
		domain, ok := strings.CutPrefix(spec, "domain:")
		u, err := url.Parse(str("url"))
		if !ok || err != nil || u.Hostname() == "" {
			return false
		}
		host := strings.ToLower(u.Hostname())
		domain = strings.ToLower(domain)
		return host == domain || strings.HasSuffix(host, "."+domain)
	}
	return false
}

// claudeName returns the agentx tool a rule name stands for.
func claudeName(rule, tool string) string {
	if t, ok := toolnames.AgentX(rule); ok {
		return t
	}
	if t, ok := toolnames.AgentX(tool); ok {
		return t
	}
	return tool
}

// isFileRule reports whether a rule name selects file tools, whose
// specifier is a path glob.
func isFileRule(name string) bool {
	switch claudeName(name, "") {
	case "read_file", "write_file", "edit_file":
		return true
	}
	return false
}

// matchesAnyFile reports whether a disallowed-tools rule matches call,
// which for an apply_patch call means any of the files the patch touches;
// otherwise it is match.
func matchesAnyFile(match func(string, *model.ToolCall) bool, rule string, call *model.ToolCall) bool {
	name, _, hasSpec := strings.Cut(rule, "(")
	if call.Name != "apply_patch" || !hasSpec || !isFileRule(strings.TrimSpace(name)) {
		return match(rule, call)
	}
	// Check each file as a call of the tool the rule names.
	tool := claudeName(strings.TrimSpace(name), "")
	for _, f := range toolnames.PatchPaths(call.Arguments) {
		args, _ := json.Marshal(map[string]string{"path": f})
		if match(rule, &model.ToolCall{Name: tool, Arguments: args}) {
			return true
		}
	}
	return false
}

func matchToolName(rule, tool string) bool {
	switch {
	case rule == "":
		return false
	case rule == "*":
		return true
	case rule == tool || slices.Contains(toolnames.Claude(tool), rule):
		return true
	case strings.HasSuffix(rule, "*"):
		return strings.HasPrefix(tool, strings.TrimSuffix(rule, "*"))
	case strings.HasPrefix(rule, "mcp__") && strings.Count(rule, "__") == 1:
		// An MCP server rule covers all of its tools.
		return strings.HasPrefix(tool, rule+"__")
	}
	return false
}

// matchCommand matches a Bash rule specifier against a command.
func matchCommand(spec, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	for _, op := range shellOperators {
		if strings.Contains(command, op) {
			return false
		}
	}
	if prefix, ok := strings.CutSuffix(spec, ":*"); ok {
		return command == prefix || strings.HasPrefix(command, prefix+" ")
	}
	if base, ok := strings.CutSuffix(spec, " *"); ok && command == base {
		// "git status *" also covers a bare "git status".
		return true
	}
	return fsmatch.Wildcard(spec, command)
}

// matchFile matches a file rule specifier against a file path.
func matchFile(root, spec, file string) bool {
	switch {
	case strings.HasPrefix(spec, "//"):
		abs := file
		if !filepath.IsAbs(abs) && root != "" {
			abs = filepath.Join(root, abs)
		}
		return compilePaths([]string{strings.TrimPrefix(spec, "/")}).match(strings.TrimPrefix(filepath.ToSlash(abs), "/"))
	case strings.HasPrefix(spec, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		rel, ok := relPath(home, absolute(root, file))
		return ok && compilePaths([]string{spec[2:]}).match(rel)
	}
	rel, ok := relPath(root, file)
	return ok && compilePaths([]string{spec}).match(rel)
}

func absolute(root, file string) string {
	if filepath.IsAbs(file) || root == "" {
		return file
	}
	return filepath.Join(root, file)
}

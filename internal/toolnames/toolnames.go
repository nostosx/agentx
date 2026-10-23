// Package toolnames maps Claude Code's built-in tool names to the agentx
// built-in tools that play the same role. Hook matchers and permission
// rules written for Claude Code, such as "Edit|Write" or "Bash(npm *)",
// use it to select the agentx tools. MCP tools already share Claude Code's
// mcp__<server>__<tool> naming.
package toolnames

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// claude maps each Claude Code tool name to its agentx counterpart.
// BashOutput and KillShell are the former names of the tools that read and
// stopped background shells; current Claude Code stops them with TaskStop
// and had TaskOutput until v2.1.277.
var claude = map[string]string{
	"Bash":       "bash",
	"BashOutput": "bash_output",
	"TaskOutput": "bash_output",
	"KillShell":  "kill_shell",
	"TaskStop":   "kill_shell",
	"Read":       "read_file",
	"Write":      "write_file",
	"Edit":       "edit_file",
	"MultiEdit":  "edit_file",
	"Glob":       "glob",
	"Grep":       "grep",
	"WebFetch":   "web_fetch",
	"TodoWrite":  "todo_write",
}

// AgentX returns the agentx built-in tool named name in Claude Code.
func AgentX(name string) (string, bool) {
	ex, ok := claude[name]
	return ex, ok
}

// patchNames are the Claude Code names of apply_patch, which edits,
// creates and deletes files, so hooks and rules guarding Edit or Write
// cover it too.
var patchNames = []string{"Edit", "Write"}

// Claude returns the Claude Code names of an agentx built-in tool, sorted.
func Claude(name string) []string {
	if name == "apply_patch" {
		return slices.Clone(patchNames)
	}
	var names []string
	for cc, ex := range claude {
		if ex == name {
			names = append(names, cc)
		}
	}
	slices.Sort(names)
	return names
}

// All returns the Claude Code tool names that have an agentx counterpart,
// sorted.
func All() []string { return slices.Sorted(maps.Keys(claude)) }

// PatchPaths returns the files an apply_patch call, whose arguments are
// {"input": patch}, adds, updates, deletes or moves to. Like the tool's
// parser it accepts whitespace around the markers; it may report extra
// paths, for example from context lines that look like markers, but never
// misses one the tool would touch.
func PatchPaths(args json.RawMessage) []string {
	var in struct {
		Input string `json:"input"`
	}
	if json.Unmarshal(args, &in) != nil {
		return nil
	}
	var paths []string
	for line := range strings.Lines(in.Input) {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"} {
			if p, ok := strings.CutPrefix(line, marker); ok {
				if p = strings.TrimSpace(p); p != "" && !slices.Contains(paths, p) {
					paths = append(paths, p)
				}
			}
		}
	}
	return paths
}

package skill_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
)

func pathSkill(t *testing.T, paths ...string) skill.Skill {
	t.Helper()
	s, err := skill.Define(skill.Metadata{Name: "api", Description: "API rules", Paths: paths}, "Body")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The patterns follow https://code.claude.com/docs/en/memory#path-specific-rules.
func TestApplies(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "repo")
	for _, tt := range []struct {
		name  string
		paths []string
		file  string
		want  bool
	}{
		{"any depth", []string{"**/*.ts"}, "src/a/b.ts", true},
		{"root only", []string{"*.md"}, "docs/a.md", false},
		{"root file", []string{"*.md"}, "README.md", true},
		{"under directory", []string{"src/**/*"}, "src/x/y.go", true},
		{"braces", []string{"src/**/*.{ts,tsx}"}, "src/c/x.tsx", true},
		{"braces miss", []string{"src/**/*.{ts,tsx}"}, "src/c/x.js", false},
		{"dot slash", []string{"./lib/*.go"}, "lib/a.go", true},
		{"absolute file", []string{"src/**"}, filepath.Join(root, "src", "a.go"), true},
		{"outside root", []string{"**"}, filepath.Join(string(filepath.Separator), "other", "a.go"), false},
		{"invalid pattern matches nothing", []string{"photos [2024/**", "*.jpg"}, "photos [2024/a.jpg", false},
		{"escaped bracket", []string{`photos \[2024/**`}, "photos [2024/a.jpg", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := skill.Applies(pathSkill(t, tt.paths...), root, []string{tt.file}); got != tt.want {
				t.Errorf("Applies(%v, %q) = %v", tt.paths, tt.file, got)
			}
		})
	}
	if !skill.Applies(newSkill(t, "plain", "Body"), root, nil) || skill.Conditional(newSkill(t, "plain", "Body")) {
		t.Error("an unconditional skill must always apply")
	}
	if skill.Applies(pathSkill(t, "**"), root, nil) {
		t.Error("a conditional skill applied without files")
	}
}

func TestBraceBudget(t *testing.T) {
	t.Parallel()
	// 4^5 = 1024 expansions exceed the budget of 1000, so the pattern is
	// used unexpanded and its braces match literally.
	big := "{a,b,c,d}/{a,b,c,d}/{a,b,c,d}/{a,b,c,d}/{a,b,c,d}.go"
	if skill.Applies(pathSkill(t, big), "", []string{"a/b/c/d/a.go"}) {
		t.Error("over-budget pattern was expanded")
	}
	if !skill.Applies(pathSkill(t, "{a,b}/x.go"), "", []string{"b/x.go"}) {
		t.Error("small pattern not expanded")
	}
}

func TestFilesOf(t *testing.T) {
	t.Parallel()
	call := func(name string, args any) *model.ToolCall {
		b, _ := json.Marshal(args)
		return &model.ToolCall{Name: name, Arguments: b}
	}
	for _, tt := range []struct {
		call *model.ToolCall
		want []string
	}{
		{call("read_file", map[string]any{"path": "a.go"}), []string{"a.go"}},
		{call("Edit", map[string]any{"file_path": "/x/b.go"}), []string{"/x/b.go"}},
		{call("NotebookEdit", map[string]any{"notebook_path": "n.ipynb"}), []string{"n.ipynb"}},
		{call("apply_patch", map[string]any{"input": "*** Begin Patch\n*** Delete File: old.go\n*** End Patch\n"}), []string{"old.go"}},
		{call("bash", map[string]any{"command": "cat a.go"}), nil},
		{&model.ToolCall{Name: "read_file", Arguments: json.RawMessage("{")}, nil},
	} {
		if got := skill.FilesOf(tt.call); len(got) != len(tt.want) || len(got) > 0 && got[0] != tt.want[0] {
			t.Errorf("FilesOf(%s %s) = %v", tt.call.Name, tt.call.Arguments, got)
		}
	}
}

func TestMatchTool(t *testing.T) {
	t.Parallel()
	home, _ := os.UserHomeDir()
	root := filepath.Join(string(filepath.Separator), "repo")
	bash := func(cmd string) *model.ToolCall {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		return &model.ToolCall{Name: "bash", Arguments: b}
	}
	file := func(name, p string) *model.ToolCall {
		b, _ := json.Marshal(map[string]string{"path": p})
		return &model.ToolCall{Name: name, Arguments: b}
	}
	fetch := func(u string) *model.ToolCall {
		b, _ := json.Marshal(map[string]string{"url": u})
		return &model.ToolCall{Name: "web_fetch", Arguments: b}
	}
	patch := func(files ...string) *model.ToolCall {
		body := "*** Begin Patch\n"
		for _, f := range files {
			body += "*** Update File: " + f + "\n@@\n-a\n+b\n"
		}
		b, _ := json.Marshal(map[string]string{"input": body + "*** End Patch\n"})
		return &model.ToolCall{Name: "apply_patch", Arguments: b}
	}
	for _, tt := range []struct {
		rule string
		call *model.ToolCall
		want bool
	}{
		{"bash", bash("ls"), true},
		{"Bash", bash("ls"), true},
		{"Read", file("read_file", "a"), true},
		{"Read", bash("ls"), false},
		{"*", bash("ls"), true},
		{"", bash("ls"), false},
		{"Bash(git add *)", bash("git add ."), true},
		{"Bash(git add *)", bash("git commit"), false},
		{"Bash(git status *)", bash("git status"), true},
		{"Bash(ls *)", bash("lsof"), false},
		{"Bash(git add:*)", bash("git add -A"), true},
		{"Bash(git add:*)", bash("git addx"), false},
		{"Bash(git add *)", bash("git add . && rm -rf /"), false},
		{"Bash(git add *)", bash("git add $(rm -rf /)"), false},
		{"Bash(npm run test)", bash("npm run test"), true},
		{"Bash(*)", bash("anything; else"), true},
		{"Bash(git add *", bash("git add ."), false},
		{"Read(src/**)", file("read_file", "src/a/b.go"), true},
		{"Read(./src/**)", file("read_file", filepath.Join(root, "src", "a.go")), true},
		{"Read(src/**)", file("read_file", "docs/a.md"), false},
		{"Edit(//tmp/*.txt)", file("edit_file", filepath.Join(string(filepath.Separator), "tmp", "x.txt")), filepath.Separator == '/'},
		{"Read(~/notes/*)", file("read_file", filepath.Join(home, "notes", "a")), home != ""},
		{"WebFetch(domain:example.com)", fetch("https://docs.example.com/a"), true},
		{"WebFetch(domain:example.com)", fetch("https://example.org/a"), false},
		{"mcp__github", &model.ToolCall{Name: "mcp__github__get_issue"}, true},
		{"mcp__github__*", &model.ToolCall{Name: "mcp__github__get_issue"}, true},
		{"mcp__git", &model.ToolCall{Name: "mcp__github__get_issue"}, false},
		{"custom(x)", &model.ToolCall{Name: "custom", Arguments: json.RawMessage(`{}`)}, false},
		// Background shells and handoffs.
		{"BashOutput", &model.ToolCall{Name: "bash_output"}, true},
		{"TaskOutput", &model.ToolCall{Name: "bash_output"}, true},
		{"KillShell", &model.ToolCall{Name: "kill_shell"}, true},
		{"TaskStop", &model.ToolCall{Name: "kill_shell"}, true},
		{"Bash", &model.ToolCall{Name: "bash_output"}, false},
		{"transfer_to_*", &model.ToolCall{Name: "transfer_to_billing"}, true},
		{"Bash(dangerously_disable_sandbox:true)", bash("ls"), false},
		// apply_patch is an Edit and Write tool; a path rule must match
		// every file of the patch.
		{"Edit", patch("a.go"), true},
		{"Write", patch("a.go"), true},
		{"Bash", patch("a.go"), false},
		{"Edit(src/**)", patch("src/a.go", "src/b/c.go"), true},
		{"Edit(src/**)", patch("src/a.go", "main.go"), false},
		{"Edit(src/**)", &model.ToolCall{Name: "apply_patch", Arguments: json.RawMessage(`{}`)}, false},
	} {
		if got := skill.MatchTool(root, tt.rule, tt.call); got != tt.want {
			t.Errorf("MatchTool(%q, %s %s) = %v, want %v", tt.rule, tt.call.Name, tt.call.Arguments, got, tt.want)
		}
	}
}

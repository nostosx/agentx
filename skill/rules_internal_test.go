package skill

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestDisallowedRuleMatchesAnyPatchedFile(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "repo")
	match := func(rule string, call *model.ToolCall) bool { return MatchTool(root, rule, call) }
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
		{"Edit(.env)", patch("src/a.go", ".env"), true},
		{"Write(.env)", patch(".env"), true},
		{"Edit(.env)", patch("src/a.go"), false},
		{"Edit", patch("src/a.go"), true},
		{"Bash(rm *)", patch(".env"), false},
	} {
		if got := matchesAnyFile(match, tt.rule, tt.call); got != tt.want {
			t.Errorf("matchesAnyFile(%q, %s) = %v, want %v", tt.rule, tt.call.Arguments, got, tt.want)
		}
	}
}

package skill_test

import (
	"testing"

	"github.com/nostosx/agentx/skill"
)

// The cases follow https://code.claude.com/docs/en/skills#available-string-substitutions.
func TestSubstitute(t *testing.T) {
	t.Parallel()
	named := skill.Metadata{Arguments: []string{"issue", "branch"}}
	vars := skill.Vars{SessionID: "sess-1", ProjectDir: "/repo", Effort: "high"}
	for _, tt := range []struct {
		name, content, args string
		meta                skill.Metadata
		want                string
	}{
		{"all arguments", "Fix GitHub issue $ARGUMENTS following our standards.", "123", skill.Metadata{}, "Fix GitHub issue 123 following our standards."},
		{"indexed", "Migrate the $ARGUMENTS[0] component from $ARGUMENTS[1] to $ARGUMENTS[2].", "SearchBar JavaScript TypeScript", skill.Metadata{}, "Migrate the SearchBar component from JavaScript to TypeScript."},
		{"shorthand", "Migrate the $0 component from $1 to $2.", "SearchBar JavaScript TypeScript", skill.Metadata{}, "Migrate the SearchBar component from JavaScript to TypeScript."},
		{"shell-style quoting", "[$0] [$1] [$ARGUMENTS]", `"hello world" second`, skill.Metadata{}, `[hello world] [second] ["hello world" second]`},
		{"single quotes and escapes", "[$0] [$1] [$2]", `'a "b"' c\ d "e\"f"`, skill.Metadata{}, `[a "b"] [c d] [e"f]`},
		{"missing indexed stays", "Use $0 and $2 and $ARGUMENTS[5].", "one", skill.Metadata{}, "Use one and $2 and $ARGUMENTS[5]."},
		{"named", "Fix $issue on $branch.", "42", named, "Fix 42 on ."},
		{"named boundary", "$issue_x $issues $issue.", "42", named, "$issue_x $issues 42."},
		{"appended when not received", "Do the thing.", "extra words", skill.Metadata{}, "Do the thing.\n\nARGUMENTS: extra words"},
		{"missing index does not receive", "Use $3.", "a", skill.Metadata{}, "Use $3.\n\nARGUMENTS: a"},
		{"named receives without argument", "Use $branch.", "a", named, "Use ."},
		{"no arguments", "Body with $ARGUMENTS.", "", skill.Metadata{}, "Body with ."},
		{"no arguments no append", "Body.", "  ", skill.Metadata{}, "Body."},
		{"escape", `Costs \$1.00 and \$ARGUMENTS, keeps \$HOME and $1.`, "a b", skill.Metadata{}, `Costs $1.00 and $ARGUMENTS, keeps \$HOME and b.`},
		{"escaped placeholder does not receive", `Costs \$0.`, "a", skill.Metadata{}, "Costs $0.\n\nARGUMENTS: a"},
		{"double backslash expands", `\\$0`, "a", skill.Metadata{}, `\\a`},
		{"values are literal", "Summarize $0", `"$ARGUMENTS from yesterday"`, skill.Metadata{}, "Summarize $ARGUMENTS from yesterday"},
		{"claude variables", "${CLAUDE_SESSION_ID} ${CLAUDE_SKILL_DIR} ${CLAUDE_PROJECT_DIR} ${CLAUDE_EFFORT} ${CLAUDE_PLUGIN_ROOT}", "", skill.Metadata{}, "sess-1 /skills/x /repo high ${CLAUDE_PLUGIN_ROOT}"},
		{"claude variables in values", "Read $0", "${CLAUDE_SKILL_DIR}/a.md", skill.Metadata{}, "Read /skills/x/a.md"},
		{"multi-digit index", "$10 $1", "a b c d e f g h i j k", skill.Metadata{}, "k b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := vars
			v.Arguments = tt.args
			if got := skill.Substitute(tt.content, tt.meta, "/skills/x", v); got != tt.want {
				t.Errorf("Substitute() = %q, want %q", got, tt.want)
			}
		})
	}
	if got := skill.Substitute("${CLAUDE_SKILL_DIR}", skill.Metadata{}, "", skill.Vars{}); got != "${CLAUDE_SKILL_DIR}" {
		t.Errorf("unknown variable replaced: %q", got)
	}
}

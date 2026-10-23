package actx

import (
	"slices"
	"testing"
)

func TestImportPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, text string
		want       []string
	}{
		{"start of text", "@a.md", []string{"a.md"}},
		{"after words", "See @README for an overview and @docs/x.md.", []string{"README", "docs/x.md."}},
		{"list item", "- git workflow @docs/git.md\n- @~/.claude/mine.md", []string{"docs/git.md", "~/.claude/mine.md"}},
		{"escaped space", `- API @Design\ Docs/api.md here`, []string{"Design Docs/api.md"}},
		{"unescaped space ends path", "@Design Docs/api.md", []string{"Design"}},
		{"quoted", `@"quoted.md" and @'single.md'`, nil},
		{"email and mention inside word", "mail me@example.com or a@b", nil},
		{"lone at sign", "a @ b", nil},
		{"code span", "Use `@literal.md` but @real.md", []string{"real.md"}},
		{"double backtick span", "``code ` @in.md`` @out.md", []string{"out.md"}},
		{"span across lines of a paragraph", "`start\n@in.md end` @out.md", []string{"out.md"}},
		{"unclosed backtick is literal", "a ` @b.md", []string{"b.md"}},
		{"span ends at paragraph", "`open\n\n@b.md `", []string{"b.md"}},
		{"backtick fence", "```\n@in.md\n```\n@out.md", []string{"out.md"}},
		{"tilde fence with info", "~~~~ markdown\n@in.md\n~~~\n@still-in.md\n~~~~\n@out.md", []string{"out.md"}},
		{"indented fence", "   ```go\n@in.md\n   ```\n@out.md", []string{"out.md"}},
		{"unclosed fence", "```\n@in.md", nil},
		{"backtick info string with backtick is not a fence", "``` a`b\n@x.md", []string{"x.md"}},
		{"four-space indent is not a fence", "    ```\n@x.md", []string{"x.md"}},
		{"crlf", "@a.md\r\n@b.md\r\n", []string{"a.md", "b.md"}},
		{"after tab", "x\t@t.md", []string{"t.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := importPaths(tt.text); !slices.Equal(got, tt.want) {
				t.Errorf("importPaths(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestCutRune(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "ab": "ab", "a\xc3": "a", "\xe2\x82": "", "\xe2\x82\xac": "\xe2\x82\xac", "a\xf0\x9f\x98": "a"} {
		if got := string(cutRune([]byte(in))); got != want {
			t.Errorf("cutRune(%q) = %q, want %q", in, got, want)
		}
	}
}

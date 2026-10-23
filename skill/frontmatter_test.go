package skill_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nostosx/agentx/skill"
)

func TestParseYAMLDetails(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, doc, description string
		tools                  []string
	}{
		{"delimiter with trailing whitespace", "--- \nname: pdf-tools\ndescription: Read PDFs\n---\t\n# Body", "Read PDFs", nil},
		{"inline comments", "---\nname: pdf-tools # the name\ndescription: \"Read # PDFs\" # quoted hash\nallowed-tools: [read_file, bash] # tools\n---\n# Body", "Read # PDFs", []string{"read_file", "bash"}},
		{"hash inside word", "---\nname: pdf-tools\ndescription: Use C# and a#b\n---\n# Body", "Use C# and a#b", nil},
		{"literal keep", "---\nname: pdf-tools\ndescription: |+\n  Read\n  PDFs\n\n---\n# Body", "Read\nPDFs\n\n", nil},
		{"folded keep", "---\nname: pdf-tools\ndescription: >+\n  Read\n  PDFs\n---\n# Body", "Read PDFs\n", nil},
		{"literal strip", "---\nname: pdf-tools\ndescription: |-\n  Read\n  PDFs\n---\n# Body", "Read\nPDFs", nil},
		{"indentation indicator", "---\nname: pdf-tools\ndescription: |2-\n    indented\n  Read\n---\n# Body", "  indented\nRead", nil},
		{"indicator with comment", "---\nname: pdf-tools\ndescription: >- # folded\n  Read\n  PDFs\n---\n# Body", "Read PDFs", nil},
		{"parenthesized tools", "---\nname: pdf-tools\ndescription: Git\nallowed-tools: Bash(git add:*) Bash(git commit:*) Read\n---\n# Body", "Git", []string{"Bash(git add:*)", "Bash(git commit:*)", "Read"}},
		{"comma separated tools", "---\nname: pdf-tools\ndescription: Git\nallowed-tools: Bash(git add:*), Read\n---\n# Body", "Git", []string{"Bash(git add:*)", "Read"}},
		{"flow list tools", "---\nname: pdf-tools\ndescription: Git\nallowed-tools: [\"Bash(git add:*)\", Bash(npm run a, b), Read]\n---\n# Body", "Git", []string{"Bash(git add:*)", "Bash(npm run a, b)", "Read"}},
		{"block list tools", "---\nname: pdf-tools\ndescription: Git\nallowed-tools:\n  - Bash(git add:*) # staging\n  - Read\n---\n# Body", "Git", []string{"Bash(git add:*)", "Read"}},
		{"crlf", "---\r\nname: pdf-tools\r\ndescription: Read PDFs\r\n---\r\n# Body\r\n", "Read PDFs", nil},
		{"single quote escape before hash", "---\nname: pdf-tools\ndescription: 'it''s # fine' # comment\n---\n# Body", "it's # fine", nil},
		{"unquoted hash comment", "---\nname: pdf-tools\ndescription: Read PDFs # comment\n---\n# Body", "Read PDFs", nil},
		{"multi-line double quoted", "---\nname: pdf-tools\ndescription: \"quoted that\n  continues # here\"\n---\n# Body", "quoted that continues # here", nil},
		{"multi-line double quoted with comment", "---\nname: pdf-tools\ndescription: \"quoted that\n  continues\" # comment\nlicense: MIT\n---\n# Body", "quoted that continues", nil},
		// PyYAML folds long single-quoted scalars at the line width.
		{"pyyaml single quoted fold", "---\nname: pdf-tools\ndescription: 'Read PDF files when the user asks about them; it''s fine to use #tags\n  or # signs here'\nallowed-tools: [read_file]\n---\n# Body", "Read PDF files when the user asks about them; it's fine to use #tags or # signs here", []string{"read_file"}},
		{"single quoted blank line", "---\nname: pdf-tools\ndescription: 'first\n\n  second'\n---\n# Body", "first\nsecond", nil},
		// PyYAML breaks long double-quoted scalars with an escaped line break
		// and continues with an escaped space.
		{"pyyaml double quoted escaped break", "---\nname: pdf-tools\ndescription: \"Read \\\"PDF\\\" files\\tand # more\\nwhen the user asks about them, which\\\n  \\ happens often\\x21 \\u00e9\"\n---\n# Body", "Read \"PDF\" files\tand # more\nwhen the user asks about them, which happens often! \u00e9", nil},
		{"quoted tool then plain tool", "---\nname: pdf-tools\ndescription: Git\nallowed-tools: 'Bash(git add:*)' Read # tools\n---\n# Body", "Git", []string{"Bash(git add:*)", "Read"}},
		{"js-yaml double quoted", "---\nname: pdf-tools\ndescription: \"Use: a # b, 'c'\"\n---\n# Body", "Use: a # b, 'c'", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, body, err := skill.Parse(tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			if meta.Name != "pdf-tools" || meta.Description != tt.description || !reflect.DeepEqual(meta.AllowedTools, tt.tools) || body != "# Body" {
				t.Errorf("Parse() = %+v, body=%q", meta, body)
			}
		})
	}
	if _, _, err := skill.Parse("---\nname: x\ndescription: y\n"); err == nil {
		t.Error("missing closing delimiter accepted")
	}
}

func TestLoadDirFollowsSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, skill.FileName), []byte("---\nname: shared\ndescription: Shared skill\n---\nBody"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "shared")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	}
	// A link back to an ancestor must not loop.
	if err := os.Symlink(root, filepath.Join(target, "loop")); err != nil {
		t.Fatal(err)
	}
	skills, err := skill.LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Metadata().Name != "shared" || skills[0].Dir() != filepath.Join(root, "shared") {
		t.Fatalf("LoadDir() = %+v", skills)
	}
}

func TestLoadFSWithoutBaseHasNoDir(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"skills/alpha/SKILL.md": {Data: []byte("---\nname: alpha\ndescription: Alpha skill\n---\nAlpha body")}}
	skills, err := skill.LoadFS(files, "skills", "")
	if err != nil || len(skills) != 1 {
		t.Fatalf("LoadFS() = %v, %v", skills, err)
	}
	if dir := skills[0].Dir(); dir != "" {
		t.Errorf("Dir() = %q, want empty", dir)
	}
	text, err := skill.NewRegistry(skills...).Load(t.Context(), "alpha")
	if err != nil || strings.Contains(text, "Base directory") || !strings.Contains(text, "Alpha body") {
		t.Errorf("Load() = %q, %v", text, err)
	}
	single, err := skill.LoadFS(files, "skills/alpha/SKILL.md", "")
	if err != nil || len(single) != 1 {
		t.Errorf("LoadFS(file) = %v, %v", single, err)
	}
}

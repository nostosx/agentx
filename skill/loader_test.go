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

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, doc, description string
		tools                  []string
	}{
		{"quoted scalar and flow list", "---\nname: pdf-tools\ndescription: 'Read ''PDF'' files'\nallowed-tools: [read_file, 'bash']\nlicense: MIT\ncustom: value\n---\n # Body \n", "Read 'PDF' files", []string{"read_file", "bash"}},
		{"folded block and block list", "---\nname: pdf-tools\ndescription: >-\n  Read PDF\n  files.\n\n  Preserve tables.\nallowed_tools:\n  - read_file\n  - bash\n---\n# Body", "Read PDF files.\nPreserve tables.", []string{"read_file", "bash"}},
		{"literal block and scalar list", "---\nname: pdf-tools\ndescription: |\n  Read PDF\n  files.\nallowed-tools: read_file bash,write_file\n---\n# Body", "Read PDF\nfiles.", []string{"read_file", "bash", "write_file"}},
		{"bom and crlf", "\xef\xbb\xbf---\r\nname: pdf-tools\r\ndescription: Read PDFs\r\n---\r\n# Body\r\n", "Read PDFs", nil},
		{"plain continuation", "---\n# Comment\nname: pdf-tools\ndescription: Read\n  PDF files\n---\n# Body", "Read PDF files", nil},
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
			if tt.name == "quoted scalar and flow list" && (meta.License != "MIT" || meta.Extra["custom"] != "value") {
				t.Errorf("extra fields lost: %+v", meta)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, doc string }{
		{"missing frontmatter", "# Instructions"},
		{"unterminated", "---\nname: skill\n"},
		{"malformed mapping", "---\nname skill\n---\nbody"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := skill.Parse(tt.doc); err == nil {
				t.Error("invalid document accepted")
			}
		})
	}
	meta, body, err := skill.Parse("---\n---")
	if err != nil || meta.Name != "" || body != "" {
		t.Errorf("Parse empty frontmatter = %+v, %q, %v", meta, body, err)
	}
}

func TestLoadFS(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"skills/zeta/SKILL.md":        {Data: []byte("---\nname: zeta\ndescription: Zeta skill\n---\nZeta body")},
		"skills/group/alpha/SKILL.md": {Data: []byte("---\ndescription: Alpha skill\n---\nAlpha body")},
		"skills/bad/SKILL.md":         {Data: []byte("missing frontmatter")},
		"skills/invalid/SKILL.md":     {Data: []byte("---\nname: in<valid>\ndescription: Invalid name\n---\nbody")},
		"skills/README.md":            {Data: []byte("ignored")},
		"outside/SKILL.md":            {Data: []byte("---\nname: outside\ndescription: Outside root\n---\nOutside body")},
	}
	skills, err := skill.LoadFS(files, "skills", "/bundled")
	if err == nil || !strings.Contains(err.Error(), "skills/bad/SKILL.md") || !strings.Contains(err.Error(), "skills/invalid/SKILL.md") {
		t.Errorf("LoadFS error = %v", err)
	}
	if len(skills) != 2 || skills[0].Metadata().Name != "alpha" || skills[1].Metadata().Name != "zeta" {
		t.Fatalf("valid sorted skills = %+v", skills)
	}
	if got, want := skills[0].Dir(), filepath.Join("/bundled", "skills", "group", "alpha"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if body, err := skills[0].Content(t.Context()); err != nil || body != "Alpha body" {
		t.Errorf("Content() = %q, %v", body, err)
	}
	// Each skill retains its own source path when bodies are loaded lazily.
	files["skills/group/alpha/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: alpha\ndescription: Alpha skill\n---\nUpdated alpha")}
	if body, err := skills[0].Content(t.Context()); err != nil || body != "Updated alpha" {
		t.Errorf("lazy Content() = %q, %v", body, err)
	}
	if body, err := skills[1].Content(t.Context()); err != nil || body != "Zeta body" {
		t.Errorf("different skill path leaked: %q, %v", body, err)
	}
	delete(files, "skills/zeta/SKILL.md")
	if _, err := skills[1].Content(t.Context()); !os.IsNotExist(err) {
		t.Errorf("missing lazy source error = %v", err)
	}
	files["skills/group/alpha/SKILL.md"] = &fstest.MapFile{Data: []byte("broken")}
	if _, err := skills[0].Content(t.Context()); err == nil {
		t.Error("malformed lazy source accepted")
	}
	if _, err := skill.LoadFS(files, "../outside", ""); err == nil {
		t.Error("invalid fs root escaped filesystem")
	}
	if _, err := skill.LoadFS(files, "missing", ""); !os.IsNotExist(err) {
		t.Errorf("missing root = %v", err)
	}
}

func TestLoadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if got, err := skill.LoadDir(missing); err != nil || len(got) != 0 {
		t.Errorf("missing directory = %v, %v", got, err)
	}
	path := filepath.Join(dir, "local", skill.FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: local\ndescription: Local skill\n---\nBody"), 0o600); err != nil {
		t.Fatal(err)
	}
	skills, err := skill.LoadDir(dir)
	if err != nil || len(skills) != 1 {
		t.Fatalf("LoadDir() = %v, %v", skills, err)
	}
	if skills[0].Dir() != filepath.Join(dir, "local") {
		t.Errorf("absolute directory = %q", skills[0].Dir())
	}
}

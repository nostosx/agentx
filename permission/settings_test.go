package permission_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nostosx/agentx/permission"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	user := filepath.Join(dir, "user.json")
	write(t, user, `{"permissions": {"deny": ["Bash(curl *)"], "defaultMode": "plan", "disableBypassPermissionsMode": "disable"}}`)
	shared := filepath.Join(project, ".claude", "settings.json")
	write(t, shared, `{"hooks": {}, "permissions": {
		"allow": ["Bash(npm run *)", "Edit(/src/**)", "Write(docs/**)", "mcp__github__get_issue(x)"],
		"ask": ["Bash(git push *)"],
		"defaultMode": "acceptEdits",
		"additionalDirectories": ["../shared", "/abs"]}}`)
	local := filepath.Join(project, ".claude", "settings.local.json")
	write(t, local, `{"permissions": {"defaultMode": "auto"}}`)
	s, warnings, err := permission.LoadSettings(user, shared, local, filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 3 || !strings.Contains(warnings[0], "Write") || !strings.Contains(warnings[1], "MCP") || !strings.Contains(warnings[2], "auto") {
		t.Fatalf("warnings = %q", warnings)
	}
	if s.DefaultMode != permission.ModeAcceptEdits || !s.DisableBypassPermissionsMode {
		t.Fatalf("settings = %+v", s)
	}
	if want := []string{filepath.Join(dir, "shared"), filepath.Clean(filepath.VolumeName(dir) + "/abs")}; !slices.Equal(s.AdditionalDirectories, want) {
		t.Fatalf("directories = %q, want %q", s.AdditionalDirectories, want)
	}
	if len(s.Rules) != 2 || s.Rules[0].Base != dir || s.Rules[1].Base != "" ||
		!slices.Equal(s.Rules[1].Allow, []string{"Bash(npm run *)", "Edit(/src/**)"}) {
		t.Fatalf("rules = %+v", s.Rules)
	}

	cfg := s.Config(project)
	cfg.Mode = ""
	p, err := permission.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := bash(p, "", "npm run build && curl x"); got != deny {
		t.Fatalf("user deny: got %s", name(got))
	}
	if got := bash(p, "", "npm run build"); got != allow {
		t.Fatalf("project allow: got %s", name(got))
	}
	if got := decide(p, "", writeInfo, args("path", "src/a.go")).Decision; got != allow {
		t.Fatalf("anchored edit: got %s", name(got))
	}
	if got := decide(p, permission.ModeAcceptEdits, writeInfo, args("path", filepath.Join(dir, "shared", "x"))).Decision; got != allow {
		t.Fatalf("additional directory: got %s", name(got))
	}
}

func TestLoadSettingsErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	write(t, bad, `{"permissions": [`)
	if _, _, err := permission.LoadSettings(bad); err == nil {
		t.Fatal("malformed JSON: no error")
	}
	s, w, err := permission.ParseSettings([]byte(`{"other": 1}`))
	if err != nil || len(w) != 0 || len(s.Rules) != 0 {
		t.Fatalf("no permissions: %+v %q %v", s, w, err)
	}
}

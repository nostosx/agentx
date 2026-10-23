//go:build !windows

package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/skill"
)

func shellManager(t *testing.T, content string, meta skill.Metadata, cfg skill.Config) (*skill.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "inject")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	front := "---\nname: inject\ndescription: Injects context\n"
	if len(meta.Arguments) > 0 {
		front += "arguments: [" + strings.Join(meta.Arguments, ", ") + "]\n"
	}
	if meta.Shell != "" {
		front += "shell: " + meta.Shell + "\n"
	}
	if err := os.WriteFile(filepath.Join(skillDir, skill.FileName), []byte(front+"---\n"+content), 0o600); err != nil {
		t.Fatal(err)
	}
	skills, err := skill.LoadDir(filepath.Join(dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProjectDir = dir
	return skill.NewManager(skill.NewRegistry(skills...), cfg), skillDir
}

func TestCommandInjection(t *testing.T) {
	t.Parallel()
	content := "Dir: !`pwd`\nArgs: !`printf '%s|' \"$0\" \"$1\" \"$ARGUMENTS\" \"$issue\" \"$ARGUMENTS_1\" \"${CLAUDE_SKILL_DIR}\" \"$CLAUDE_SESSION_ID\"`\n" +
		"Literal: KEY=!`echo no` and `!`echo no` stay.\n" +
		"```!\necho first\necho second\n```\n" +
		"Output is not rescanned: !`printf '!%secho nested%s' \"$(printf '\\140')\" \"$(printf '\\140')\"`\n" +
		"Text $0 and $ARGUMENTS[1]."
	m, skillDir := shellManager(t, content, skill.Metadata{Arguments: []string{"issue"}}, skill.Config{Shell: true})
	got, err := m.Invoke(t.Context(), "sess-9", skill.Call{Name: "inject", Arguments: `"a b; rm -rf /" $(id)`})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(skillDir)))
	for _, want := range []string{
		"Dir: " + real + "\n",
		"Args: a b; rm -rf /|$(id)|\"a b; rm -rf /\" $(id)|a b; rm -rf /|$(id)|" + skillDir + "|sess-9|\n",
		"Literal: KEY=!`echo no` and `!`echo no` stay.",
		"first\nsecond\n",
		"Output is not rescanned: !`echo nested`\n",
		"Text a b; rm -rf / and $(id).",
	} {
		if !strings.Contains(got, want) && !strings.Contains(got, strings.Replace(want, real, filepath.Dir(filepath.Dir(skillDir)), 1)) {
			t.Errorf("Invoke() = %q\nmissing %q", got, want)
		}
	}
	if strings.Contains(got, "ARGUMENTS:") {
		t.Errorf("arguments appended though commands received them: %q", got)
	}

	// Disabled, placeholders stay as written.
	off, _ := shellManager(t, "Run !`echo hi` now.\n```!\necho block\n```", skill.Metadata{}, skill.Config{})
	got, err = off.Invoke(t.Context(), "", skill.Call{Name: "inject"})
	if err != nil || !strings.Contains(got, "Run !`echo hi` now.\n```!\necho block\n```") {
		t.Errorf("disabled injection = %q, %v", got, err)
	}
}

func TestCommandInjectionFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, content string
		cfg           skill.Config
		fail          string
	}{
		{"non-zero exit", "A: !`echo out; echo err >&2; exit 3`", skill.Config{Shell: true}, "exit status 3"},
		{"benign grep", "A: !`echo x | grep missing`", skill.Config{Shell: true}, ""},
		{"benign git diff", "A: !`git diff --no-index /dev/null /dev/null; exit 1`", skill.Config{Shell: true}, "exit status 1"},
		{"diff exit 1", "A: !`diff <(echo a) <(echo b)`", skill.Config{Shell: true}, ""},
		{"timeout", "A: !`sleep 5`", skill.Config{Shell: true, ShellTimeout: 100 * time.Millisecond}, "timed out"},
		{"control character", "A: !`echo a\x07`\nB: !`touch should-not-exist`", skill.Config{Shell: true}, "no shell command"},
		{"output limit", "A: !`printf '%0100d' 0`", skill.Config{Shell: true, ShellOutputLimit: 10}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, skillDir := shellManager(t, tt.content, skill.Metadata{}, tt.cfg)
			got, err := m.Invoke(t.Context(), "", skill.Call{Name: "inject"})
			if tt.fail == "" {
				if err != nil {
					t.Fatalf("Invoke() error = %v", err)
				}
				if tt.cfg.ShellOutputLimit > 0 {
					_, injected, ok := strings.Cut(got, "\n\nA: ")
					output, _, closed := strings.Cut(injected, "\n</skill>")
					if !ok || !closed || len(output) > tt.cfg.ShellOutputLimit {
						t.Errorf("injected output exceeds budget: %q", got)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.fail) || !strings.Contains(err.Error(), "shell command failed") && tt.fail != "no shell command" {
				t.Fatalf("Invoke() = %q, %v; want error %q", got, err, tt.fail)
			}
			if tt.name == "non-zero exit" && !strings.Contains(err.Error(), "[stderr]\nout\nerr") {
				t.Errorf("error lacks output: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(skillDir)), "should-not-exist")); statErr == nil {
				t.Error("a command ran although another held a control character")
			}
		})
	}
}

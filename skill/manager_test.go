package skill_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

func defined(t *testing.T, meta skill.Metadata, body string) skill.Skill {
	t.Helper()
	if meta.Description == "" {
		meta.Description = "Skill " + meta.Name
	}
	s, err := skill.Define(meta, body)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManagerTool(t *testing.T) {
	t.Parallel()
	inline := defined(t, skill.Metadata{Name: "inline"}, "Use $0.")
	fork := defined(t, skill.Metadata{Name: "fork", Context: "fork"}, "Task $ARGUMENTS.")
	forker := func(_ context.Context, s skill.Skill, task string) (*tool.Result, error) {
		return tool.Text("forked " + s.Metadata().Name + ": " + task), nil
	}
	for _, tt := range []struct {
		name     string
		skills   []skill.Skill
		cfg      skill.Config
		readOnly bool
	}{
		{"inline", []skill.Skill{inline, fork}, skill.Config{}, true},
		{"fork", []skill.Skill{inline, fork}, skill.Config{Fork: forker}, false},
		{"fork without fork skills", []skill.Skill{inline}, skill.Config{Fork: forker}, true},
		{"shell", []skill.Skill{inline}, skill.Config{Shell: true}, false},
	} {
		if got := skill.NewManager(skill.NewRegistry(tt.skills...), tt.cfg).Tool().Info().ReadOnly; got != tt.readOnly {
			t.Errorf("%s: ReadOnly = %v", tt.name, got)
		}
	}

	m := skill.NewManager(skill.NewRegistry(inline, fork), skill.Config{Fork: forker})
	tl := m.Tool()
	res, err := tl.Call(t.Context(), json.RawMessage(`{"name":"fork","arguments":"a b"}`))
	if err != nil || res.Content != "forked fork: Task a b." {
		t.Errorf("fork call = %+v, %v", res, err)
	}
	res, err = tl.Call(t.Context(), json.RawMessage(`{"name":"inline","arguments":"x"}`))
	if err != nil || !strings.Contains(res.Content, "Use x.") {
		t.Errorf("inline call = %+v, %v", res, err)
	}
	// Without a Forker, forked skills run inline.
	res, err = skill.NewRegistry(fork).Tool().Call(t.Context(), json.RawMessage(`{"name":"fork","arguments":"a"}`))
	if err != nil || !strings.Contains(res.Content, "<skill name=\"fork\">\nTask a.") {
		t.Errorf("inline fork = %+v, %v", res, err)
	}
	// ${CLAUDE_EFFORT} defaults to the configured effort.
	effort := skill.NewManager(skill.NewRegistry(defined(t, skill.Metadata{Name: "e"}, "E=${CLAUDE_EFFORT}")), skill.Config{Effort: "high"})
	if text, err := effort.Invoke(t.Context(), "", skill.Call{Name: "e"}); err != nil || !strings.Contains(text, "E=high") {
		t.Errorf("effort = %q, %v", text, err)
	}
	props := tl.Info().Parameters
	b, _ := json.Marshal(props)
	if !strings.Contains(string(b), `"arguments"`) {
		t.Errorf("schema = %s", b)
	}
}

func TestManagerPrompt(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("d", 1530)
	r := skill.NewRegistry(
		defined(t, skill.Metadata{Name: "plain", Description: long, WhenToUse: "when needed"}, "x"),
		defined(t, skill.Metadata{Name: "manual", DisableModelInvocation: true}, "x"),
		defined(t, skill.Metadata{Name: "api", Paths: []string{"src/**"}}, "x"),
	)
	if p := r.Prompt(); !strings.Contains(p, "- plain: "+long+" when…\n") || strings.Contains(p, "manual") || strings.Contains(p, "api") {
		t.Errorf("Prompt() = %q", p)
	}
	m := skill.NewManager(r, skill.Config{ProjectDir: "/repo"})
	read := &model.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartToolCall, ToolCall: &model.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"src/a.go"}`)}}}}
	if p := m.Prompt([]*model.Message{read}); !strings.Contains(p, "- api: Skill api") || strings.Contains(p, "manual") {
		t.Errorf("Manager.Prompt() = %q", p)
	}
	if p := skill.NewRegistry(defined(t, skill.Metadata{Name: "manual", DisableModelInvocation: true}, "x")).Prompt(); p != "" {
		t.Errorf("Prompt() without offered skills = %q", p)
	}
}

func TestSkillHooksParse(t *testing.T) {
	t.Parallel()
	meta := skill.Metadata{Name: "h", Hooks: map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "Bash", "hooks": []any{
				map[string]any{"type": "command", "command": "true", "once": true},
				map[string]any{"type": "http", "url": "http://127.0.0.1:1"},
				"not a handler",
			}},
			"not a group",
		},
		"Stop":    []any{map[string]any{"matcher": "ignored", "hooks": []any{map[string]any{"type": "command", "command": "true", "once": true}}}},
		"Unknown": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "true"}}}},
		"Broken":  "nope",
	}}
	m, warnings, err := skill.SkillHooks(meta, hook.SettingsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has(hook.PreToolUse) || !m.Has(hook.Stop) {
		t.Error("hooks not registered")
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"hooks.PreToolUse[0].hooks[2]: not a mapping", "hooks.PreToolUse[1]: not a mapping", "hooks.Broken: not a list", "event not dispatched by agentx"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", joined, want)
		}
	}
	if got := skill.NewRegistry(defined(t, meta, "x"), defined(t, skill.Metadata{Name: "none"}, "x")).HookEvents(); !reflect.DeepEqual(got, []hook.EventType{"Broken", hook.PreToolUse, hook.Stop, "Unknown"}) {
		t.Errorf("HookEvents() = %v", got)
	}
	if err := skill.NewManager(nil, skill.Config{}).AttachHooks(nil); err == nil {
		t.Error("AttachHooks(nil) accepted")
	}
}

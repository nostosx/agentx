package skill_test

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/nostosx/agentx/skill"
)

func TestParseTypedFields(t *testing.T) {
	t.Parallel()
	doc := `---
name: deploy
description: Deploy the application
when_to_use: When the user says ship it
argument-hint: "[environment] [version]"
arguments: [environment, version]
disable-model-invocation: yes
user-invocable: Off
allowed-tools: Bash(git add *) Bash(git commit *) Read
disallowed-tools:
  - AskUserQuestion
model: claude-opus-4-1
effort: high
context: fork
agent: Explore
background: false
paths: "src/**/*.{ts,tsx}, lib/**/*.ts"
shell: powershell
license: Apache-2.0
compatibility: Requires git
version: 1.2.0
metadata:
  author: example-org
  version: "1.0"
  tags: [a, b]
  nested:
    key: 1.10
hooks:
  PreToolUse:
    - matcher: "Bash"
      hooks:
        - type: command
          command: "./scripts/check.sh"
          timeout: 30
          once: true
  PostToolUse:
  - matcher: Edit|Write
    hooks: [{type: command, command: "echo done # not a comment", async: true}]
custom:
  a: [1, {b: c}]
---
# Deploy
`
	meta, body, err := skill.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if body != "# Deploy" {
		t.Errorf("body = %q", body)
	}
	f := false
	want := skill.Metadata{
		Name:                   "deploy",
		Description:            "Deploy the application",
		WhenToUse:              "When the user says ship it",
		ArgumentHint:           "[environment] [version]",
		Arguments:              []string{"environment", "version"},
		DisableModelInvocation: true,
		UserInvocable:          &f,
		AllowedTools:           []string{"Bash(git add *)", "Bash(git commit *)", "Read"},
		DisallowedTools:        []string{"AskUserQuestion"},
		Model:                  "claude-opus-4-1",
		Effort:                 "high",
		Context:                "fork",
		Agent:                  "Explore",
		Background:             &f,
		Paths:                  []string{"src/**/*.{ts,tsx}", "lib/**/*.ts"},
		Shell:                  "powershell",
		License:                "Apache-2.0",
		Compatibility:          "Requires git",
		Metadata: map[string]any{
			"author":  "example-org",
			"version": "1.0",
			"tags":    []any{"a", "b"},
			"nested":  map[string]any{"key": "1.10"},
		},
		Hooks: map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
				map[string]any{"type": "command", "command": "./scripts/check.sh", "timeout": int64(30), "once": true},
			}}},
			"PostToolUse": []any{map[string]any{"matcher": "Edit|Write", "hooks": []any{
				map[string]any{"type": "command", "command": "echo done # not a comment", "async": true},
			}}},
		},
		Extra: map[string]string{"version": "1.2.0", "custom": `{"a":[1,{"b":"c"}]}`},
	}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("Parse() =\n%#v\nwant\n%#v", meta, want)
	}
	if !meta.Fork() || meta.IsUserInvocable() {
		t.Errorf("Fork() = %v, IsUserInvocable() = %v", meta.Fork(), meta.IsUserInvocable())
	}
}

func TestParseYAMLStructures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, front string
		want        skill.Metadata
	}{
		{"sequence at key indentation", "allowed-tools:\n- Read\n- Bash(git status)\nlicense: MIT", skill.Metadata{AllowedTools: []string{"Read", "Bash(git status)"}, License: "MIT"}},
		{"multi-line flow sequence", "paths: [\n  \"a/**\",\n  b/*.go,\n]", skill.Metadata{Paths: []string{"a/**", "b/*.go"}}},
		{"arguments string", "arguments: issue branch", skill.Metadata{Arguments: []string{"issue", "branch"}}},
		{"paths keep spaces", "paths: docs/my notes/*.md, src/**", skill.Metadata{Paths: []string{"docs/my notes/*.md", "src/**"}}},
		{"booleans", "disable-model-invocation: TRUE\nuser-invocable: 1\nbackground: No", skill.Metadata{DisableModelInvocation: true, UserInvocable: new(true), Background: new(false)}},
		{"invalid boolean kept", "disable-model-invocation: maybe", skill.Metadata{Extra: map[string]string{"disable-model-invocation": "maybe"}}},
		{"metadata that is not a map is dropped", "metadata: [a]", skill.Metadata{}},
		{"quoted keys and nested quoted values", "metadata:\n  \"a key\": 'it''s'\n  b: \"x\\ty\"", skill.Metadata{Metadata: map[string]any{"a key": "it's", "b": "x\ty"}}},
		{"block scalar in nested map", "metadata:\n  notes: |\n    line 1\n    line 2\n  after: x", skill.Metadata{Metadata: map[string]any{"notes": "line 1\nline 2", "after": "x"}}},
		{"sequence of sequences", "metadata:\n  k:\n    - - a\n      - b\n    - c", skill.Metadata{Metadata: map[string]any{"k": []any{[]any{"a", "b"}, "c"}}}},
		{"sequence joined in Extra", "custom:\n  - a\n  - b", skill.Metadata{Extra: map[string]string{"custom": "a, b"}}},
		{"comments between entries", "metadata:\n  # comment\n  a: b # trailing\n\n  c: d", skill.Metadata{Metadata: map[string]any{"a": "b", "c": "d"}}},
		{"empty value", "license:\nmodel: inherit", skill.Metadata{Model: "inherit"}},
		{"flow map", "custom: {a: 1, b: [x, y], 'c d': \"e\"}", skill.Metadata{Extra: map[string]string{"custom": `{"a":1,"b":["x","y"],"c d":"e"}`}}},
		{"unterminated flow sequence stays lenient", "allowed-tools: [Read, Bash", skill.Metadata{AllowedTools: []string{"Read", "Bash"}}},
		{"colon without space is a scalar", "allowed-tools:\n  - Bash(git add:*)\n  - mcp__github__get:issue", skill.Metadata{AllowedTools: []string{"Bash(git add:*)", "mcp__github__get:issue"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, _, err := skill.Parse("---\n" + tt.front + "\n---\n")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(meta, tt.want) {
				t.Errorf("Parse() =\n%#v\nwant\n%#v", meta, tt.want)
			}
		})
	}
}

func TestLoadDefaultsAndStrict(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"skills/notes/SKILL.md":     {Data: []byte("---\nallowed-tools: Read\n---\n\n# Take notes\nMore")},
		"skills/Mixed/SKILL.md":     {Data: []byte("---\ndescription: Claude Code accepts this name\n---\nBody")},
		"skills/other/SKILL.md":     {Data: []byte("---\nname: renamed\ndescription: Name differs from its directory\n---\nBody")},
		"skills/strict-ok/SKILL.md": {Data: []byte("---\nname: strict-ok\ndescription: Valid everywhere\n---\nBody")},
	}
	skills, err := skill.LoadFS(files, "skills", "")
	if err != nil || len(skills) != 4 {
		t.Fatalf("LoadFS() = %v, %v", skills, err)
	}
	r := skill.NewRegistry(skills...)
	if s, ok := r.Get("notes"); !ok || s.Metadata().Description != "# Take notes" {
		t.Errorf("description fallback = %+v", s)
	}
	strict, err := skill.LoadFS(files, "skills", "", skill.Strict())
	if err == nil || len(strict) != 1 || strict[0].Metadata().Name != "strict-ok" {
		t.Errorf("strict LoadFS() = %v, %v", strict, err)
	}
}

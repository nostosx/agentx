package skill_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/skill"
)

func newSkill(t *testing.T, name, content string) skill.Skill {
	t.Helper()
	s, err := skill.New(name, "Skill "+name, content, "read_file")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	a, z := newSkill(t, "alpha", "Alpha body"), newSkill(t, "zeta", "Zeta body")
	replacement := newSkill(t, "alpha", "Replacement body")
	r := skill.NewRegistry(z, a, replacement)
	if r.Len() != 2 || !reflect.DeepEqual(r.Names(), []string{"alpha", "zeta"}) {
		t.Errorf("registry names = %v, len=%d", r.Names(), r.Len())
	}
	if got, ok := r.Get("alpha"); !ok || got != replacement {
		t.Errorf("replacement = %v, %v", got, ok)
	}
	list := r.List()
	if len(list) != 2 || list[0] != replacement || list[1] != z {
		t.Errorf("sorted list = %v", list)
	}
	list[0] = z
	if got, _ := r.Get("alpha"); got != replacement {
		t.Error("List exposed registry backing state")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get found missing skill")
	}
	if _, err := r.Load(t.Context(), "missing"); err == nil || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Errorf("missing skill error = %v", err)
	}
	prompt := r.Prompt()
	if !strings.Contains(prompt, "- alpha: Skill alpha\n- zeta: Skill zeta") || strings.Contains(prompt, "Replacement body") {
		t.Errorf("Prompt() = %q", prompt)
	}
	loaded, err := r.Load(t.Context(), "alpha")
	if err != nil || !strings.Contains(loaded, "<skill name=\"alpha\">") || !strings.Contains(loaded, "Tools this skill uses: read_file") || !strings.Contains(loaded, "Replacement body") {
		t.Errorf("Load() = %q, %v", loaded, err)
	}
	var zero skill.Registry
	zero.Register(a)
	if zero.Len() != 1 {
		t.Error("zero-value registry not usable")
	}
	var nilRegistry *skill.Registry
	if nilRegistry.Len() != 0 || nilRegistry.List() != nil || nilRegistry.Names() != nil || nilRegistry.Prompt() != "" {
		t.Error("nil registry should be empty")
	}
	if _, ok := nilRegistry.Get("alpha"); ok {
		t.Error("nil registry found skill")
	}
}

type errorSkill struct {
	skill.Skill
	err error
}

func (s errorSkill) Content(context.Context) (string, error) { return "", s.err }

func TestRegistryLoadContentError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("source unavailable")
	r := skill.NewRegistry(errorSkill{Skill: newSkill(t, "broken", ""), err: wantErr})
	if _, err := r.Load(t.Context(), "broken"); !errors.Is(err, wantErr) {
		t.Errorf("Load error = %v", err)
	}
}

func TestRegistryTool(t *testing.T) {
	t.Parallel()
	r := skill.NewRegistry(newSkill(t, "alpha", "Instructions"))
	tool := r.Tool()
	if tool.Info().Name != skill.ToolName || !tool.Info().ReadOnly {
		t.Errorf("Tool info = %+v", tool.Info())
	}
	result, err := tool.Call(t.Context(), json.RawMessage(`{"name":" alpha "}`))
	if err != nil || result.IsError || !strings.Contains(result.Content, "Instructions") {
		t.Fatalf("Tool call = %+v, %v", result, err)
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"name":"missing"}`)); err == nil {
		t.Error("tool accepted missing skill")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := skill.NewRegistry()
	s := newSkill(t, "alpha", "Body")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 30 {
				r.Register(s)
				if _, ok := r.Get("alpha"); !ok {
					t.Error("registered skill missing")
				}
				_ = r.Names()
				_ = r.List()
				_ = r.Len()
				_ = r.Prompt()
			}
		})
	}
	wg.Wait()
}

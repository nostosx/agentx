package permission_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/tool/builtin"
)

func TestSearchFiltersRestrictedDescendants(t *testing.T) {
	t.Parallel()
	for _, mode := range []permission.Mode{permission.ModeDefault, permission.ModePlan, permission.ModeDontAsk, permission.ModeBypassPermissions} {
		for _, ask := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ask=%v", mode, ask), func(t *testing.T) {
				dir := t.TempDir()
				for name, text := range map[string]string{
					".env": "MATCH_SECRET", "nested/.env": "MATCH_SECRET", "secrets/private.txt": "MATCH_SECRET",
					"public.txt": "MATCH_PUBLIC", "secrets/public.txt": "MATCH_CARVEOUT", ".gitignore": ".env\n",
				} {
					path := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				rules := permission.Rules{Allow: []string{"Read"}}
				restricted := []string{"Read(.env)", "Read(secrets/**)", "Read(!secrets/public.txt)"}
				if ask {
					rules.Ask = restricted
				} else {
					rules.Deny = restricted
				}
				p := permission.MustNew(permission.Config{Dir: dir, Mode: mode, Rules: []permission.Rules{rules}})
				m := modeltest.New(modeltest.CallTools(
					modeltest.Call{ID: "grep", Name: "grep", Args: map[string]any{"path": ".", "pattern": "MATCH_", "include_ignored": true}},
					modeltest.Call{ID: "glob", Name: "glob", Args: map[string]any{"path": ".", "pattern": "**/*", "include_ignored": true}},
				), modeltest.Reply("done"))
				a := agent.MustNew("search", agent.WithModel(m), agent.WithPermissions(p),
					agent.WithTools(builtin.ReadOnlyTools(builtin.Config{Dir: dir})...))
				res, err := loop.Collect(a.Run(t.Context(), agent.Text("search")), nil)
				if err != nil || res.Status != loop.StatusCompleted {
					t.Fatalf("run=%+v err=%v", res, err)
				}
				results := toolResults(res.Messages)
				if len(results) != 2 {
					t.Fatalf("results=%+v", results)
				}
				for _, result := range results {
					if result.IsError || strings.Contains(result.Content, "SECRET") || strings.Contains(result.Content, ".env") || strings.Contains(result.Content, "private.txt") {
						t.Errorf("restricted file exposed: %+v", result)
					}
					if !strings.Contains(result.Content, "public.txt") || !strings.Contains(result.Content, "secrets/public.txt") {
						t.Errorf("unrestricted or carved-out file missing: %+v", result)
					}
				}
			})
		}
	}
}

func TestSearchApprovalForExplicitFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("approved content"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := permission.MustNew(permission.Config{Dir: dir, Rules: []permission.Rules{{Ask: []string{"Read(secret.txt)", "Grep"}}}})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "grep", Args: map[string]string{"path": "secret.txt", "pattern": "approved"}}), modeltest.Reply("done"))
	var asked int
	a := agent.MustNew("search", agent.WithModel(m), agent.WithPermissions(p), agent.WithTools(builtin.Grep(builtin.Config{Dir: dir})),
		agent.WithApprover(loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
			asked++
			return loop.Approve, nil
		})))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("search")), nil)
	if err != nil {
		t.Fatal(err)
	}
	results := toolResults(res.Messages)
	if asked != 1 || len(results) != 1 || results[0].IsError || !strings.Contains(results[0].Content, "approved content") {
		t.Fatalf("asked=%d results=%+v", asked, results)
	}
}

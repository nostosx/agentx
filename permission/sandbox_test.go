package permission_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/tool/builtin"
)

// markSandbox is a sandbox that only marks the commands it wraps.
type markSandbox struct{}

func (markSandbox) Available() error { return nil }

func (markSandbox) Wrap(cmd *exec.Cmd, _ string) error {
	cmd.Env = append(cmd.Env, "IN_SANDBOX=yes")
	return nil
}

func sandboxedBashCall(id, cmd string, escape bool) modeltest.Step {
	return modeltest.CallTools(modeltest.Call{ID: id, Name: "bash", Args: map[string]any{"command": cmd, "dangerously_disable_sandbox": escape}})
}

// Sandboxed bash commands need no approval, so dontAsk mode runs them,
// while a command that leaves the sandbox needs approval and is denied.
func TestSandboxedBashInDontAskMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	dir := t.TempDir()
	bash := builtin.Bash(builtin.Config{Dir: dir, Sandbox: markSandbox{}})
	p := permission.MustNew(permission.Config{Dir: dir, Mode: permission.ModeDontAsk})
	m := modeltest.New(sandboxedBashCall("c1", "touch in && echo $IN_SANDBOX", false), sandboxedBashCall("c2", "touch out", true), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(bash), agent.WithPermissions(p))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := toolResults(res.Messages)
	if len(results) != 2 || results[0].IsError || !strings.Contains(results[0].Content, "yes") || !results[1].IsError ||
		!strings.Contains(results[1].Content, "dontAsk") {
		t.Fatalf("results = %+v", results)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsandboxed output: %v, want os.ErrNotExist", err)
	}
}

// A parameter rule asks before every unsandboxed retry, even in
// bypassPermissions mode.
func TestSandboxEscapeAskRule(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	dir := t.TempDir()
	bash := builtin.Bash(builtin.Config{Dir: dir, Sandbox: markSandbox{}})
	p := permission.MustNew(permission.Config{Dir: dir, Mode: permission.ModeBypassPermissions,
		Rules: []permission.Rules{{Ask: []string{"Bash(dangerously_disable_sandbox:true)"}}}})
	var asked []string
	approver := loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
		asked = append(asked, req.Call.ID)
		return loop.Approve, nil
	})
	m := modeltest.New(sandboxedBashCall("c1", "echo one", false), sandboxedBashCall("c2", "echo two", true), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(bash), agent.WithPermissions(p), agent.WithApprover(approver))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if len(asked) != 1 || asked[0] != "c2" {
		t.Fatalf("asked = %v", asked)
	}
}

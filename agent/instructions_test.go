package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestWithInstructionFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Run go test before committing."), 0o644); err != nil {
		t.Fatal(err)
	}
	m := modeltest.New(modeltest.Reply("done"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithInstructions("base"), agent.WithInstructionFiles(dir))
	if _, err := loop.Collect(a.Run(t.Context(), agent.Text("question")), nil); err != nil {
		t.Fatal(err)
	}
	want := "base\n\nContents of AGENTS.md (project instructions):\n\nRun go test before committing."
	if got := m.Requests()[0].System; !strings.HasPrefix(got, want) {
		t.Errorf("system prompt = %q, want prefix %q", got, want)
	}
}

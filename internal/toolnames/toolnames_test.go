package toolnames

import (
	"slices"
	"testing"
)

func TestNames(t *testing.T) {
	t.Parallel()
	if ex, ok := AgentX("MultiEdit"); !ok || ex != "edit_file" {
		t.Fatalf("AgentX(MultiEdit) = %q, %v", ex, ok)
	}
	if _, ok := AgentX("edit_file"); ok {
		t.Fatal("agentx names are not Claude Code names")
	}
	if got := Claude("edit_file"); !slices.Equal(got, []string{"Edit", "MultiEdit"}) {
		t.Fatalf("Claude(edit_file) = %q", got)
	}
	if got := Claude("apply_patch"); !slices.Equal(got, []string{"Edit", "Write"}) {
		t.Fatalf("Claude(apply_patch) = %q", got)
	}
	if got := Claude("kill_shell"); !slices.Equal(got, []string{"KillShell", "TaskStop"}) {
		t.Fatalf("Claude(kill_shell) = %q", got)
	}
	if got := Claude("bash_output"); !slices.Equal(got, []string{"BashOutput", "TaskOutput"}) {
		t.Fatalf("Claude(bash_output) = %q", got)
	}
	if got := All(); len(got) != 13 || !slices.IsSorted(got) {
		t.Fatalf("All() = %q", got)
	}
}

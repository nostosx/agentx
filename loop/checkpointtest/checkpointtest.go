// Package checkpointtest checks loop.Checkpointer implementations
// against the contract the built-in checkpointers follow.
package checkpointtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// Run runs the conformance tests against checkpointers made by
// newCheckpointer, which must return an empty checkpointer independent of
// the others; it is called once per subtest and subtests run in parallel.
// The contract:
//
//   - Load of a missing run returns loop.ErrCheckpointNotFound; Delete
//     of a missing run succeeds.
//   - Save stores a copy of the state, replacing the run's checkpoint;
//     every field round-trips as JSON and later changes to the saved or
//     loaded state are not stored.
//   - A state that cannot be encoded is rejected with an error, not a
//     panic, leaving the stored checkpoint unchanged.
//   - Run IDs are 1 to 128 ASCII letters, digits, '_' and '-'; others are
//     rejected by Save, Load and Delete. They are case-sensitive, though a
//     checkpointer whose medium cannot tell IDs differing only in case
//     apart may reject the second one.
//   - Checkpoints of several megabytes are stored, and concurrent use is
//     safe: a Load returns one of the saved states, never a mix.
func Run(t *testing.T, newCheckpointer func(t *testing.T) loop.Checkpointer) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, loop.Checkpointer)
	}{
		{"NotFound", testNotFound},
		{"RoundTrip", testRoundTrip},
		{"Isolation", testIsolation},
		{"Overwrite", testOverwrite},
		{"Delete", testDelete},
		{"EncodeFailure", testEncodeFailure},
		{"IDs", testIDs},
		{"LargePayload", testLargePayload},
		{"Concurrency", testConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newCheckpointer(t))
		})
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func save(t *testing.T, cp loop.Checkpointer, st *loop.State) {
	t.Helper()
	if err := cp.Save(t.Context(), st); err != nil {
		t.Fatalf("Save %s = %v", st.RunID, err)
	}
}

func load(t *testing.T, cp loop.Checkpointer, id string) *loop.State {
	t.Helper()
	st, err := cp.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("Load %s = %v", id, err)
	}
	return st
}

func testNotFound(t *testing.T, cp loop.Checkpointer) {
	if _, err := cp.Load(t.Context(), "missing"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Load missing = %v, want ErrCheckpointNotFound", err)
	}
	if err := cp.Delete(t.Context(), "missing"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
}

// fullState sets every field of State.
func fullState(id string) *loop.State {
	call := &model.ToolCall{ID: "call-1", Name: "write", Arguments: json.RawMessage(`{"path":"a.txt","text":"héllo \u0000 世界"}`)}
	return &loop.State{
		Version: loop.StateVersion, RunID: id, SessionID: "session-1", Agent: "coder", Turn: 3,
		System: "You are helpful.",
		Messages: []*model.Message{
			model.UserText("write a file"),
			{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartReasoning, Text: "plan", Signature: "sig"}, {Type: model.PartToolCall, ToolCall: call}}},
			{Role: model.RoleUser, Parts: []model.Part{model.ImagePart("image/png", []byte{0, 1, 2, 0xff})}, Metadata: map[string]any{"k": "v"}},
			{Role: model.RoleUser, Parts: []model.Part{
				model.DocumentPart("application/pdf", []byte("%PDF\x00\xff"), "report.pdf"),
				model.AudioPart("audio/wav", []byte{'R', 'I', 'F', 'F', 0}),
				model.FilePart(model.PartVideo, "gemini", "", "video/mp4"),
			}},
			model.ToolResults(&model.ToolResult{CallID: "call-0", Name: "screenshot", Content: "screen",
				Parts: []model.Part{model.ImagePart("image/png", []byte{0x89, 0})}, Metadata: map[string]any{"width": 1280.0}}),
		},
		Start:        1,
		Usage:        model.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5},
		Status:       loop.StatusInterrupted,
		Reason:       "approval",
		FinishReason: model.FinishReason("tool_calls"),
		Pending: []*loop.PendingCall{{
			Call: call, Denied: true, Reason: "no", NeedsApproval: true, ReadOnly: true, Concurrent: true,
			Values: map[string]string{"hook": "value"}, ModelArguments: json.RawMessage(`{"path":"b.txt"}`),
			Started: true, Result: &model.ToolResult{CallID: "call-1", Name: "write", Content: "ok", IsError: true},
			Nested: &loop.Interrupt{RunID: id + "-sub", Requests: []*loop.ApprovalRequest{{Call: call, Reason: "ask", Agent: "sub"}},
				State: &loop.State{RunID: id + "-sub", Status: loop.StatusInterrupted, Messages: []*model.Message{model.UserText("nested")}}},
			NestedApprovals: map[string]loop.Approval{"call-2": {Approved: true, Reason: "fine", UpdatedInput: json.RawMessage(`{"x":1}`), Remember: loop.RememberSession}},
			Approval:        &loop.Approval{Approved: false, Reason: "denied"},
			Resuming:        true,
		}},
		Values:        map[string]string{"middleware": "state", "unicode": "✓", loop.HandoffFromKey: "triage", "agentx.handoff.count": "1"},
		UpdatedAt:     time.Date(2024, 1, 2, 3, 4, 5, 987654321, time.UTC),
		MaxTurns:      7,
		Limits:        &loop.Limits{Requests: 1, ToolCalls: 2, InputTokens: 3, OutputTokens: 4, TotalTokens: 5},
		OutputRetries: 2,
	}
}

func testRoundTrip(t *testing.T, cp loop.Checkpointer) {
	st := fullState("round-trip")
	want := jsonOf(t, st)
	save(t, cp, st)
	got := load(t, cp, "round-trip")
	if g := jsonOf(t, got); g != want {
		t.Fatalf("Load = %s\nwant %s", g, want)
	}
	if !got.UpdatedAt.Equal(st.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, st.UpdatedAt)
	}
	empty := &loop.State{RunID: "empty"}
	save(t, cp, empty)
	if g, w := jsonOf(t, load(t, cp, "empty")), jsonOf(t, empty); g != w {
		t.Fatalf("empty state = %s, want %s", g, w)
	}
}

func testIsolation(t *testing.T, cp loop.Checkpointer) {
	st := fullState("isolated")
	want := jsonOf(t, st)
	save(t, cp, st)
	st.Messages[0].Parts[0].Text = "caller mutation"
	st.Values["middleware"] = "caller mutation"
	st.Pending[0].Call.Arguments[2] = 'X'
	st.Pending[0].Values["hook"] = "caller mutation"
	st.Status = loop.StatusFailed
	got := load(t, cp, "isolated")
	if g := jsonOf(t, got); g != want {
		t.Fatalf("checkpoint changed through the saved state: %s", g)
	}
	got.Messages[0].Parts[0].Text = "reader mutation"
	got.Values["middleware"] = "reader mutation"
	got.Pending[0].Call.Arguments[2] = 'Y'
	got.Pending[0].Nested.State.Messages[0].Parts[0].Text = "reader mutation"
	if g := jsonOf(t, load(t, cp, "isolated")); g != want {
		t.Fatalf("checkpoint changed through a loaded state: %s", g)
	}
}

func testOverwrite(t *testing.T, cp loop.Checkpointer) {
	save(t, cp, fullState("overwrite"))
	next := &loop.State{RunID: "overwrite", Turn: 4, Status: loop.StatusRunning, Messages: []*model.Message{model.UserText("next")}}
	save(t, cp, next)
	if g, w := jsonOf(t, load(t, cp, "overwrite")), jsonOf(t, next); g != w {
		t.Fatalf("overwritten checkpoint = %s, want %s", g, w)
	}
}

func testDelete(t *testing.T, cp loop.Checkpointer) {
	save(t, cp, fullState("deleted"))
	save(t, cp, fullState("kept"))
	if err := cp.Delete(t.Context(), "deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Load(t.Context(), "deleted"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Load deleted = %v, want ErrCheckpointNotFound", err)
	}
	if err := cp.Delete(t.Context(), "deleted"); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	load(t, cp, "kept")
	save(t, cp, &loop.State{RunID: "deleted", Turn: 9})
	if got := load(t, cp, "deleted"); got.Turn != 9 {
		t.Fatalf("checkpoint saved after Delete = %+v", got)
	}
}

func testEncodeFailure(t *testing.T, cp loop.Checkpointer) {
	save(t, cp, &loop.State{RunID: "encode", Turn: 1})
	bad := &loop.State{RunID: "encode", Turn: 2, Pending: []*loop.PendingCall{{
		Call: &model.ToolCall{ID: "c", Name: "n"}, ModelArguments: json.RawMessage(`{not json`)}}}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Save of an unencodable state panicked: %v", r)
			}
		}()
		if err := cp.Save(t.Context(), bad); err == nil {
			t.Error("Save accepted an unencodable state")
		}
	}()
	if got := load(t, cp, "encode"); got.Turn != 1 || len(got.Pending) != 0 {
		t.Fatalf("failed Save changed the checkpoint: %+v", got)
	}
}

func testIDs(t *testing.T, cp loop.Checkpointer) {
	ctx := t.Context()
	valid := []string{"a", "_", "-", "0", "run_ABC-123", strings.Repeat("r", 128)}
	for i, id := range valid {
		save(t, cp, &loop.State{RunID: id, Turn: i})
	}
	for i, id := range valid {
		if got := load(t, cp, id); got.RunID != id || got.Turn != i {
			t.Fatalf("Load %q = %+v; IDs must be distinct", id, got)
		}
	}
	checkCase(t, cp)
	invalid := []string{"", "dot.name", "..", "../escape", "a/b", `a\b`, "a b", "é", "a:b", "a\x00b", strings.Repeat("r", 129)}
	for _, id := range invalid {
		if err := cp.Save(ctx, &loop.State{RunID: id}); err == nil {
			t.Errorf("Save accepted invalid run ID %q", id)
		}
		if _, err := cp.Load(ctx, id); err == nil {
			t.Errorf("Load accepted invalid run ID %q", id)
		}
		if err := cp.Delete(ctx, id); err == nil {
			t.Errorf("Delete accepted invalid run ID %q", id)
		}
	}
}

// checkCase checks that run IDs differing only in case are never
// confused. A checkpointer whose medium cannot tell them apart, such as a
// directory on a case-insensitive file system, may reject the second.
func checkCase(t *testing.T, cp loop.Checkpointer) {
	ctx := t.Context()
	save(t, cp, &loop.State{RunID: "case", Turn: 1})
	if err := cp.Save(ctx, &loop.State{RunID: "CASE", Turn: 2}); err != nil {
		if _, err := cp.Load(ctx, "CASE"); !errors.Is(err, loop.ErrCheckpointNotFound) {
			t.Fatalf("Load of a rejected ID = %v, want ErrCheckpointNotFound", err)
		}
	} else if got := load(t, cp, "CASE"); got.RunID != "CASE" || got.Turn != 2 {
		t.Fatalf("Load CASE = %+v", got)
	}
	if got := load(t, cp, "case"); got.RunID != "case" || got.Turn != 1 {
		t.Fatalf("Load case = %+v; IDs differing in case were confused", got)
	}
	if err := cp.Delete(ctx, "CASE"); err != nil {
		t.Fatal(err)
	}
	if got := load(t, cp, "case"); got.Turn != 1 {
		t.Fatalf("deleting CASE changed case: %+v", got)
	}
}

func testLargePayload(t *testing.T, cp loop.Checkpointer) {
	big := strings.Repeat("large checkpoint ✓ ", 1<<17) // 2.4 MiB
	st := &loop.State{RunID: "large", Messages: []*model.Message{model.UserText(big)}}
	for i := range 2000 {
		st.Messages = append(st.Messages, model.AssistantText(fmt.Sprintf("message %d", i)))
	}
	save(t, cp, st)
	got := load(t, cp, "large")
	if len(got.Messages) != 2001 || got.Messages[0].Text() != big || got.Messages[2000].Text() != "message 1999" {
		t.Fatalf("large checkpoint has %d messages", len(got.Messages))
	}
}

func testConcurrency(t *testing.T, cp loop.Checkpointer) {
	const workers, rounds = 8, 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			own := fmt.Sprintf("run-%d", w)
			for r := range rounds {
				// Every field of a saved state carries the same number, so a
				// mixed checkpoint is detected.
				n := w*rounds + r
				st := &loop.State{RunID: "shared", Turn: n, Start: n, System: fmt.Sprint(n), Messages: []*model.Message{model.UserText(fmt.Sprint(n))}}
				if err := cp.Save(t.Context(), st); err != nil {
					t.Error(err)
					return
				}
				got, err := cp.Load(t.Context(), "shared")
				if err != nil {
					t.Error(err)
					return
				}
				if got == nil || got.Start != got.Turn || got.System != fmt.Sprint(got.Turn) || len(got.Messages) != 1 || got.Messages[0].Text() != fmt.Sprint(got.Turn) {
					t.Errorf("Load returned a mixed checkpoint: %+v", got)
					return
				}
				if err := cp.Save(t.Context(), &loop.State{RunID: own, Turn: r}); err != nil {
					t.Error(err)
					return
				}
				if got, err := cp.Load(t.Context(), own); err != nil || got.Turn != r {
					t.Errorf("Load %s = %+v, %v; want turn %d", own, got, err, r)
					return
				}
				if r%3 == 0 {
					if err := cp.Delete(t.Context(), own); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

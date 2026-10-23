package agent_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/tool/builtin"
)

// writeStep returns a model step that writes files with write_file; files
// alternate paths and contents.
func writeStep(files ...string) modeltest.Step {
	var calls []modeltest.Call
	for i := 0; i+1 < len(files); i += 2 {
		calls = append(calls, modeltest.Call{ID: "w" + files[i], Name: "write_file", Args: map[string]string{"path": files[i], "content": files[i+1]}})
	}
	return modeltest.CallTools(calls...)
}

// coder returns a runner of an agent with the file tools and a file
// history in dir.
func coder(t *testing.T, dir string, store session.Store, m *modeltest.Model) (*agent.Runner, *builtin.FileHistory) {
	t.Helper()
	h, err := builtin.NewFileHistory(builtin.FileHistoryConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tools := builtin.Tools(builtin.Config{Dir: dir, History: h, NoApproval: true})
	a := agent.MustNew("coder", agent.WithModel(m), agent.WithTools(tools...), agent.WithFileHistory(h))
	return agent.NewRunner(a, store), h
}

// files returns the files in dir with their contents.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func messageCount(t *testing.T, r *agent.Runner, id string) int {
	t.Helper()
	sess, err := r.Store().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return len(sess.Messages())
}

func TestRunnerRewind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := session.NewMemoryStore()
	m := modeltest.New(
		writeStep("a.txt", "v1"), modeltest.Reply("ok1"),
		writeStep("a.txt", "v2", "b.txt", "b"), modeltest.Reply("ok2"),
		modeltest.Reply("ok3"),
	)
	r, _ := coder(t, dir, store, m)
	first, err := r.RunText(t.Context(), "s", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.RunText(t.Context(), "s", "second")
	if err != nil {
		t.Fatal(err)
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 2 {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if c := cps[0]; c.ID != first.RunID || c.Index != 0 || c.Message.Text() != "first" || len(c.Changes) != 1 || c.Changes[0].Kind != builtin.FileCreated || !c.Code || c.Time.IsZero() {
		t.Fatalf("first checkpoint = %+v", c)
	}
	if c := cps[1]; c.ID != second.RunID || c.Index != 4 || c.Message.Text() != "second" || len(c.Changes) != 2 || !c.Code {
		t.Fatalf("second checkpoint = %+v", c)
	}

	res, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindBoth)
	if err != nil || res.Message.Text() != "second" || len(res.Restored) != 2 || res.Undo == "" {
		t.Fatalf("Rewind = %+v, %v", res, err)
	}
	if got := files(t, dir); !maps.Equal(got, map[string]string{"a.txt": "v1"}) {
		t.Fatalf("files = %v", got)
	}
	if n := messageCount(t, r, "s"); n != 4 {
		t.Fatalf("messages = %d", n)
	}
	if cps, _ := r.Checkpoints(t.Context(), "s"); len(cps) != 1 {
		t.Fatalf("checkpoints after rewind = %+v", cps)
	}
	// The session continues from the rewound conversation.
	if _, err := r.RunText(t.Context(), "s", "third"); err != nil {
		t.Fatal(err)
	}
	if req := m.Requests()[4]; len(req.Messages) != 5 || req.Messages[4].Text() != "third" {
		t.Fatalf("request after rewind = %+v", req.Messages)
	}

	// Code only keeps the conversation.
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindCode); err != nil {
		t.Fatal(err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
	if n := messageCount(t, r, "s"); n != 6 {
		t.Fatalf("messages = %d", n)
	}
	// Conversation only keeps the files.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = r.Rewind(t.Context(), "s", first.RunID, agent.RewindConversation)
	if err != nil || res.Message.Text() != "first" || len(res.Restored) != 0 {
		t.Fatalf("Rewind = %+v, %v", res, err)
	}
	if n := messageCount(t, r, "s"); n != 0 || len(files(t, dir)) != 1 {
		t.Fatalf("messages = %d, files = %v", n, files(t, dir))
	}
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindBoth); !errors.Is(err, builtin.ErrCheckpointNotFound) {
		t.Fatalf("Rewind to a removed checkpoint = %v", err)
	}
	for _, mode := range []agent.RewindMode{0, 4, agent.RewindBoth | 8} {
		if _, err := r.Rewind(t.Context(), "s", first.RunID, mode); err == nil || !strings.Contains(err.Error(), "invalid rewind mode") {
			t.Fatalf("Rewind with mode %d = %v", mode, err)
		}
	}
}

func TestRunnerRewindWithoutHistory(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("one"), modeltest.Reply("two"))
	r := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), nil)
	if _, err := r.RunText(t.Context(), "s", "first"); err != nil {
		t.Fatal(err)
	}
	second, err := r.RunText(t.Context(), "s", "second")
	if err != nil {
		t.Fatal(err)
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 2 || cps[1].Code || cps[1].Changes != nil {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if _, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindBoth); !errors.Is(err, agent.ErrCannotRewind) || !strings.Contains(err.Error(), "no file history") {
		t.Fatalf("code rewind without history = %v", err)
	}
	res, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindConversation)
	if err != nil || res.Message.Text() != "second" || messageCount(t, r, "s") != 2 {
		t.Fatalf("Rewind = %+v, %v", res, err)
	}
	if _, err := r.Rewind(t.Context(), "missing", second.RunID, agent.RewindConversation); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Rewind of a missing session = %v", err)
	}
}

func TestRunnerRewindRefusesBusyAndInterrupted(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	wait := tool.Func("wait", "wait", func(ctx context.Context, _ struct{}) (string, error) {
		close(started)
		<-release
		return "done", nil
	})
	deploy := tool.Func("deploy", "deploy", func(context.Context, struct{}) (string, error) { return "deployed", nil }, tool.RequireApproval())
	m := modeltest.New(
		modeltest.Reply("one"),
		modeltest.CallTools(modeltest.Call{ID: "w", Name: "wait"}), modeltest.Reply("two"),
		modeltest.CallTools(modeltest.Call{ID: "d", Name: "deploy"}),
	)
	r := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(wait, deploy)), nil)
	first, err := r.RunText(t.Context(), "s", "first")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, err := r.RunText(t.Context(), "s", "second")
		done <- err
	}()
	<-started
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindConversation); !errors.Is(err, agent.ErrSessionBusy) {
		t.Errorf("Rewind during a run = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res, err := r.RunText(t.Context(), "s", "deploy")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindConversation); err == nil || !strings.Contains(err.Error(), "interrupted run") {
		t.Fatalf("Rewind while interrupted = %v", err)
	}
	if err := r.Discard(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindConversation); err != nil {
		t.Fatalf("Rewind after discard = %v", err)
	}
}

func TestRunnerRewindGivesUpFailedRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := modeltest.New(writeStep("a.txt", "v1"), modeltest.Reply("ok"), writeStep("a.txt", "v2"), modeltest.Fail(errors.New("provider down")))
	r, _ := coder(t, dir, nil, m)
	first, err := r.RunText(t.Context(), "s", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunText(t.Context(), "s", "second"); err == nil {
		t.Fatal("failed run succeeded")
	}
	sess, _ := r.Store().Get(t.Context(), "s")
	if _, ok := sess.State().Get(agent.FailedRunKey); !ok {
		t.Fatal("no failed run")
	}
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindCode); err != nil {
		t.Fatal(err)
	}
	sess, _ = r.Store().Get(t.Context(), "s")
	if _, ok := sess.State().Get(agent.FailedRunKey); ok {
		t.Fatal("failed run kept after rewind")
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestRunnerForkRewind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := modeltest.New(writeStep("a.txt", "v1"), modeltest.Reply("ok"), writeStep("b.txt", "fork"), modeltest.Reply("forked"))
	r, h := coder(t, dir, nil, m)
	first, err := r.RunText(t.Context(), "s", "first")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := r.Fork(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunText(t.Context(), fork, "in the fork"); err != nil {
		t.Fatal(err)
	}
	orig, _ := r.Checkpoints(t.Context(), "s")
	forked, _ := r.Checkpoints(t.Context(), fork)
	if len(orig) != 1 || len(forked) != 2 || forked[0].ID != first.RunID || len(forked[0].Changes) != 1 {
		t.Fatalf("checkpoints = %+v, %+v", orig, forked)
	}
	if hist, _ := h.Checkpoints(t.Context(), "s"); len(hist) != 1 {
		t.Fatalf("history of the original = %+v", hist)
	}
	res, err := r.Rewind(t.Context(), fork, first.RunID, agent.RewindBoth)
	if err != nil || len(res.Restored) != 2 {
		t.Fatalf("Rewind of the fork = %+v, %v", res, err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
	// The original keeps its conversation and history.
	if n := messageCount(t, r, "s"); n != 4 {
		t.Fatalf("original messages = %d", n)
	}
	if hist, _ := h.Checkpoints(t.Context(), "s"); len(hist) != 1 || len(hist[0].Changes) != 1 {
		t.Fatalf("history of the original = %+v", hist)
	}
}

func TestRunnerRewindSubAgentChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h, err := builtin.NewFileHistory(builtin.FileHistoryConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tools := builtin.Tools(builtin.Config{Dir: dir, History: h, NoApproval: true})
	helper := agent.MustNew("helper", agent.WithModel(modeltest.New(writeStep("sub.txt", "by helper"), modeltest.Reply("written"))), agent.WithTools(tools...))
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "h", Name: "helper", Args: map[string]string{"prompt": "write"}}), modeltest.Reply("done"))
	r := agent.NewRunner(agent.MustNew("lead", agent.WithModel(m), agent.WithSubAgents(helper), agent.WithFileHistory(h)), nil)
	res, err := r.RunText(t.Context(), "s", "delegate")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 1 || len(cps[0].Changes) != 1 || cps[0].Changes[0].Run != res.RunID {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if _, err := r.Rewind(t.Context(), "s", res.RunID, agent.RewindCode); err != nil {
		t.Fatal(err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestRunnerCheckpointsAfterCompaction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := session.NewMemoryStore()
	m := modeltest.New(modeltest.Reply("one"), writeStep("a.txt", "v"), modeltest.Reply("two"))
	r, _ := coder(t, dir, store, m)
	first, err := r.RunText(t.Context(), "s", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.RunText(t.Context(), "s", "second")
	if err != nil {
		t.Fatal(err)
	}
	// Drop the first exchange, as compaction does.
	sess, err := store.Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	msgs := sess.Messages()
	sess.Replace(append([]*model.Message{model.UserText("summary")}, msgs[2:]...))
	if err := store.Save(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 2 || cps[0].Index != -1 || cps[0].Message != nil || cps[1].Index != 1 || cps[1].Message.Text() != "second" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindBoth); !errors.Is(err, agent.ErrCannotRewind) || !strings.Contains(err.Error(), "no longer in the conversation") {
		t.Fatalf("Rewind to a compacted message = %v", err)
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("files changed by a refused rewind: %v", got)
	}
	res, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindBoth)
	if err != nil || res.Message.Text() != "second" || messageCount(t, r, "s") != 1 || len(files(t, dir)) != 0 {
		t.Fatalf("Rewind = %+v, %v", res, err)
	}
	// The code can still be rewound to the compacted checkpoint.
	if _, err := r.Rewind(t.Context(), "s", first.RunID, agent.RewindCode); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCheckpointsRepeatedPromptAfterCompaction(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("one"), modeltest.Reply("two"), modeltest.Reply("three"))
	r := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), nil)
	var runs []string
	for range 3 {
		res, err := r.RunText(t.Context(), "s", "continue")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, res.RunID)
	}
	// Compact the first two exchanges. Both the summary and the surviving
	// prompt have the same text as the removed prompts.
	sess, err := r.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	msgs := sess.Messages()
	sess.Replace(append([]*model.Message{model.UserText("continue")}, msgs[4:]...))
	if err := r.Store().Save(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	// A new Runner proves checkpoint identity survives a store round trip.
	r = agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), r.Store())
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 3 {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	for i, want := range []int{-1, -1, 1} {
		if cps[i].Index != want || (want < 0 && cps[i].Message != nil) {
			t.Errorf("checkpoint %d = %+v; want index %d", i, cps[i], want)
		}
	}
	for _, id := range runs[:2] {
		if _, err := r.Rewind(t.Context(), "s", id, agent.RewindConversation); !errors.Is(err, agent.ErrCannotRewind) {
			t.Errorf("rewind of compacted checkpoint %s = %v", id, err)
		}
		if got := messageCount(t, r, "s"); got != 3 {
			t.Fatalf("refused rewind changed conversation: %d messages", got)
		}
	}
	res, err := r.Rewind(t.Context(), "s", runs[2], agent.RewindConversation)
	if err != nil || res.Message.Text() != "continue" || messageCount(t, r, "s") != 1 {
		t.Fatalf("rewind of surviving checkpoint = %+v, %v", res, err)
	}
}

func TestRunnerRewindRequiresUniqueMessageIdentity(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"legacy", "duplicate identity"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			r, _ := coder(t, dir, nil, modeltest.New(writeStep("a.txt", "v"), modeltest.Reply("done")))
			res, err := r.RunText(t.Context(), "s", "write")
			if err != nil {
				t.Fatal(err)
			}
			sess, err := r.Store().Get(t.Context(), "s")
			if err != nil {
				t.Fatal(err)
			}
			msgs := sess.Messages()
			if name == "legacy" {
				// Old records have only a content hash and position, neither
				// of which can distinguish a removed repeated prompt.
				sess.State().Set(agent.CheckpointsKey, []map[string]any{{"id": res.RunID, "index": 0, "hash": "legacy"}})
				delete(msgs[0].Metadata, "agentx.checkpoint")
			} else {
				msgs = append(msgs, msgs[0].Clone())
			}
			sess.Replace(msgs)
			if err := r.Store().Save(t.Context(), sess); err != nil {
				t.Fatal(err)
			}
			cps, err := r.Checkpoints(t.Context(), "s")
			if err != nil || len(cps) != 1 || cps[0].Index != -1 || cps[0].Message != nil || !cps[0].Code {
				t.Fatalf("Checkpoints = %+v, %v", cps, err)
			}
			if _, err := r.Rewind(t.Context(), "s", res.RunID, agent.RewindBoth); !errors.Is(err, agent.ErrCannotRewind) {
				t.Fatalf("ambiguous conversation rewind = %v", err)
			}
			if messageCount(t, r, "s") != len(msgs) || len(files(t, dir)) != 1 {
				t.Fatal("refused rewind changed the session or files")
			}
			if _, err := r.Rewind(t.Context(), "s", res.RunID, agent.RewindCode); err != nil {
				t.Fatal(err)
			}
			if messageCount(t, r, "s") != len(msgs) || len(files(t, dir)) != 0 {
				t.Fatal("code rewind did not preserve the conversation and restore files")
			}
		})
	}
}

func TestRunnerRewindOriginalPrompt(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("one"), modeltest.Reply("two"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithContextProvider(
		actx.Static("context", "private context for the model", actx.PlaceReminder),
	))
	r := agent.NewRunner(a, nil)
	original := model.NewMessage(model.RoleUser,
		model.TextPart("Please explain this literal block:\n"+model.WrapReminder("user-authored text")),
		model.ImagePart("image/png", []byte{1, 2, 3}),
		model.TextPart("\nKeep all my text."),
	)
	original.Name = "alice"
	original.Metadata = map[string]any{"app.note": "preserve", "agentx.checkpoint": "untrusted"}
	before := original.Clone()
	res, err := loop.Collect(r.Run(t.Context(), "s", original), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatalf("caller prompt mutated: %+v", original)
	}
	request := m.Requests()[0].Messages[0]
	if !strings.Contains(request.Text(), model.WrapReminder("private context for the model")) || !strings.Contains(request.Text(), before.Text()) {
		t.Fatalf("model did not receive context and user text: %q", request.Text())
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 1 || !reflect.DeepEqual(cps[0].Message, before) {
		t.Fatalf("checkpoint must return original prompt: %+v, %v", cps, err)
	}
	rewound, err := r.Rewind(t.Context(), "s", res.RunID, agent.RewindConversation)
	if err != nil || !reflect.DeepEqual(rewound.Message, before) {
		t.Fatalf("rewind must return original prompt: %+v, %v", rewound, err)
	}
	if _, err := loop.Collect(r.Run(t.Context(), "s", rewound.Message), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(m.Requests()[1].Messages[0].Text(), "private context for the model"); got != 1 {
		t.Fatalf("context duplicated when resending rewound prompt: %d copies", got)
	}
}

// failingStore fails Save while fail is set.
type failingStore struct {
	session.Store
	fail bool
}

func (s *failingStore) Save(ctx context.Context, sess session.Session) error {
	if s.fail {
		return errors.New("store unavailable")
	}
	return s.Store.Save(ctx, sess)
}

func TestRunnerRewindUndoesCodeWhenSaveFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &failingStore{Store: session.NewMemoryStore()}
	m := modeltest.New(writeStep("a.txt", "v1"), modeltest.Reply("ok1"), writeStep("a.txt", "v2"), modeltest.Reply("ok2"))
	r, _ := coder(t, dir, store, m)
	if _, err := r.RunText(t.Context(), "s", "first"); err != nil {
		t.Fatal(err)
	}
	second, err := r.RunText(t.Context(), "s", "second")
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindBoth); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("Rewind = %v", err)
	}
	store.fail = false
	if got := files(t, dir); !maps.Equal(got, map[string]string{"a.txt": "v2"}) {
		t.Fatalf("files = %v", got)
	}
	if n := messageCount(t, r, "s"); n != 8 {
		t.Fatalf("messages = %d", n)
	}
}

// TestRunnerRewindRestoresActiveAgent rewinds the conversation past a
// handoff: the agent active at the checkpoint gets the next message again,
// while other session state is kept.
func TestRunnerRewindRestoresActiveAgent(t *testing.T) {
	t.Parallel()
	bm := modeltest.New(modeltest.Reply("billing 1"), modeltest.Reply("billing 2"), modeltest.Reply("billing 3"))
	billing := agent.MustNew("billing", agent.WithModel(bm))
	tm := modeltest.New(modeltest.Reply("triage 1"), modeltest.CallTools(transfer("h", "billing")), modeltest.Reply("triage 2"))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithHandoffs(billing))
	r := agent.NewRunner(triage, nil)
	active := func() string {
		sess, err := r.Store().Get(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		v, _ := sess.State().Get(agent.ActiveAgentKey)
		s, _ := v.(string)
		return s
	}
	if res, err := r.RunText(t.Context(), "s", "hello"); err != nil || res.Output != "triage 1" {
		t.Fatalf("first run = %+v, %v", res, err)
	}
	second, err := r.RunText(t.Context(), "s", "invoice")
	if err != nil || second.Agent != "billing" || active() != "billing" {
		t.Fatalf("second run = %+v, %v; active %q", second, err, active())
	}
	third, err := r.RunText(t.Context(), "s", "more")
	if err != nil || third.Output != "billing 2" {
		t.Fatalf("third run = %+v, %v", third, err)
	}
	sess, _ := r.Store().Get(t.Context(), "s")
	sess.State().Set("app.note", "kept")
	if err := r.Store().Save(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	cps, err := r.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 3 || cps[1].Agent != "" || cps[2].Agent != "billing" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	// Rewinding to the third run keeps billing active.
	if _, err := r.Rewind(t.Context(), "s", third.RunID, agent.RewindConversation); err != nil || active() != "billing" {
		t.Fatalf("rewind to third: %v; active %q", err, active())
	}
	// Rewinding before the handoff returns the session to triage.
	if _, err := r.Rewind(t.Context(), "s", second.RunID, agent.RewindConversation); err != nil || active() != "" {
		t.Fatalf("rewind to second: %v; active %q", err, active())
	}
	sess, _ = r.Store().Get(t.Context(), "s")
	if v, _ := sess.State().Get("app.note"); v != "kept" {
		t.Fatalf("session state = %v", v)
	}
	if res, err := r.RunText(t.Context(), "s", "again"); err != nil || res.Output != "triage 2" || res.Agent != "triage" {
		t.Fatalf("run after rewind = %+v, %v", res, err)
	}
}

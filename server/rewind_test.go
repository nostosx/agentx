package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool/builtin"
)

func writeStep(id, path, content string) modeltest.Step {
	return modeltest.CallTools(modeltest.Call{ID: id, Name: "write_file", Args: map[string]string{"path": path, "content": content}})
}

func TestRewindEndpoints(t *testing.T) {
	dir := t.TempDir()
	hist, err := builtin.NewFileHistory(builtin.FileHistoryConfig{Dir: filepath.Join(t.TempDir(), "history")})
	if err != nil {
		t.Fatal(err)
	}
	tools := builtin.Tools(builtin.Config{Dir: dir, History: hist, NoApproval: true})
	t.Cleanup(func() { builtin.Close(tools) })
	f := newFixture(t, []modeltest.Step{
		writeStep("w1", "a.txt", "one"), modeltest.Reply("first done"),
		writeStep("w2", "a.txt", "two"), modeltest.Reply("second done"),
		deployStep("call_1", "prod"),
	}, []agent.Option{agent.WithTools(tools...), agent.WithFileHistory(hist)})
	for _, input := range []string{"first", "second"} {
		if resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: input}); resp.StatusCode != http.StatusOK {
			t.Fatalf("run %s: %s", input, resp.Status)
		}
	}
	resp := f.do(http.MethodGet, "/sessions/s1/checkpoints", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoints: %s", resp.Status)
	}
	cps := decodeJSON[struct{ Checkpoints []CheckpointInfo }](t, resp).Checkpoints
	if len(cps) != 2 || cps[1].Index != 4 || cps[1].Message.Text() != "second" || !cps[1].Code || len(cps[1].Changes) != 1 || cps[1].Changes[0].Path != "a.txt" {
		t.Fatalf("checkpoints = %+v", cps)
	}

	for _, tc := range []struct {
		body   any
		status int
		code   string
	}{
		{map[string]any{"checkpoint_id": cps[1].ID}, http.StatusBadRequest, CodeBadRequest},
		{map[string]any{"checkpoint_id": cps[1].ID, "mode": "files"}, http.StatusBadRequest, CodeBadRequest},
		{map[string]any{"checkpoint_id": "run_missing", "mode": "both"}, http.StatusNotFound, CodeNotFound},
	} {
		if resp := f.do(http.MethodPost, "/sessions/s1/rewind", "", tc.body); resp.StatusCode != tc.status || errorCode(t, resp) != tc.code {
			t.Errorf("rewind %v: %s", tc.body, resp.Status)
		}
	}
	if resp := f.do(http.MethodPost, "/sessions/missing/rewind", "", RewindRequest{CheckpointID: "x", Mode: "conversation"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("rewind of a missing session: %s", resp.Status)
	}

	resp = f.do(http.MethodPost, "/sessions/s1/rewind", "", RewindRequest{CheckpointID: cps[1].ID, Mode: "both"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rewind: %s", resp.Status)
	}
	res := decodeJSON[RewindResponse](t, resp)
	if res.Message.Text() != "second" || len(res.Restored) != 1 || res.Undo == "" {
		t.Fatalf("rewind = %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one" {
		t.Fatalf("a.txt = %q", b)
	}
	info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
	if info.MessageCount != 4 {
		t.Fatalf("messages after rewind = %d", info.MessageCount)
	}

	// A session waiting for approvals cannot be rewound.
	if resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: "deploy"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("interrupted run: %s", resp.Status)
	}
	if resp := f.do(http.MethodPost, "/sessions/s1/rewind", "", RewindRequest{CheckpointID: cps[0].ID, Mode: "conversation"}); resp.StatusCode != http.StatusConflict || errorCode(t, resp) != CodeRunInterrupted {
		t.Errorf("rewind of an interrupted session: %s", resp.Status)
	}
}

func TestRewindReturnsOriginalPrompt(t *testing.T) {
	const input = "Explain <system-reminder>literal user text</system-reminder>"
	f := newFixture(t, []modeltest.Step{modeltest.Reply("done")}, []agent.Option{
		agent.WithContextProvider(actx.Static("context", "private context for the model", actx.PlaceReminder)),
	})
	if resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: input}); resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %s", resp.Status)
	}
	cps := decodeJSON[struct{ Checkpoints []CheckpointInfo }](t, f.do(http.MethodGet, "/sessions/s1/checkpoints", "", nil)).Checkpoints
	if len(cps) != 1 || cps[0].Message.Text() != input {
		t.Fatalf("checkpoint returned augmented input: %+v", cps)
	}
	resp := f.do(http.MethodPost, "/sessions/s1/rewind", "", RewindRequest{CheckpointID: cps[0].ID, Mode: "conversation"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rewind: %s", resp.Status)
	}
	if res := decodeJSON[RewindResponse](t, resp); res.Message.Text() != input {
		t.Fatalf("rewind returned augmented input: %q", res.Message.Text())
	}
}

func TestRewindWithoutFileHistory(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("hi")}, nil)
	if resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: "hello"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %s", resp.Status)
	}
	cps := decodeJSON[struct{ Checkpoints []CheckpointInfo }](t, f.do(http.MethodGet, "/sessions/s1/checkpoints", "", nil)).Checkpoints
	if len(cps) != 1 || cps[0].Code {
		t.Fatalf("checkpoints = %+v", cps)
	}
	if resp := f.do(http.MethodPost, "/sessions/s1/rewind", "", RewindRequest{CheckpointID: cps[0].ID, Mode: "code"}); resp.StatusCode != http.StatusConflict || errorCode(t, resp) != CodeCannotRewind {
		t.Fatalf("code rewind without history: %s", resp.Status)
	}
	resp := f.do(http.MethodPost, "/sessions/s1/rewind", "", RewindRequest{CheckpointID: cps[0].ID, Mode: "conversation"})
	if resp.StatusCode != http.StatusOK || decodeJSON[RewindResponse](t, resp).Message.Text() != "hello" {
		t.Fatalf("conversation rewind: %s", resp.Status)
	}
	if resp := f.do(http.MethodGet, "/sessions/missing/checkpoints", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("checkpoints of a missing session: %s", resp.Status)
	}
}

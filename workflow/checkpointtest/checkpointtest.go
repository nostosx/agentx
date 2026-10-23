// Package checkpointtest checks workflow.Checkpointer implementations
// against the contract the built-in checkpointers follow.
package checkpointtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/workflow"
)

// Run runs the conformance tests against checkpointers made by
// newCheckpointer, which must return an empty checkpointer independent of
// the others; it is called once per subtest and subtests run in parallel.
// The contract:
//
//   - Load of a missing run returns workflow.ErrCheckpointNotFound; Delete
//     of a missing run succeeds.
//   - Save stores a copy of the checkpoint, replacing the run's previous
//     one; every field round-trips as JSON and later changes to the saved
//     or loaded checkpoint are not stored.
//   - A checkpoint that cannot be encoded is rejected with an error, not a
//     panic, leaving the stored one unchanged.
//   - Run IDs are 1 to 64 ASCII letters, digits, '_' and '-'; others are
//     rejected by Save, Load and Delete. They are case-sensitive, though a
//     checkpointer whose medium cannot tell IDs differing only in case
//     apart may reject the second one.
//   - Checkpoints of several megabytes are stored, and concurrent use is
//     safe: a Load returns one of the saved checkpoints, never a mix.
func Run(t *testing.T, newCheckpointer func(t *testing.T) workflow.Checkpointer) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, workflow.Checkpointer)
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

// canonical encodes v as compact JSON with raw messages compacted, so
// checkpoints that differ only in the formatting of raw JSON compare equal.
func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func save(t *testing.T, cp workflow.Checkpointer, c *workflow.Checkpoint) {
	t.Helper()
	if err := cp.Save(t.Context(), c); err != nil {
		t.Fatalf("Save %s = %v", c.RunID, err)
	}
}

func load(t *testing.T, cp workflow.Checkpointer, id string) *workflow.Checkpoint {
	t.Helper()
	c, err := cp.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("Load %s = %v", id, err)
	}
	return c
}

func testNotFound(t *testing.T, cp workflow.Checkpointer) {
	if _, err := cp.Load(t.Context(), "missing"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Load missing = %v, want ErrCheckpointNotFound", err)
	}
	if err := cp.Delete(t.Context(), "missing"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
}

// fullCheckpoint sets every field of Checkpoint.
func fullCheckpoint(id string) *workflow.Checkpoint {
	return &workflow.Checkpoint{
		Version: workflow.CheckpointVersion, RunID: id, Workflow: "pipeline", Status: workflow.StatusSuspended, Error: "boom",
		Values:    map[string]json.RawMessage{"text": json.RawMessage(`"héllo \u0000 世界"`), "n": json.RawMessage(`1.5`), "large": json.RawMessage(`9007199254740993`), "obj": json.RawMessage(`{"a":[1,null,true]}`)},
		Done:      []string{"start", "a"},
		Skipped:   []string{"b"},
		Running:   []string{"c"},
		Failed:    []string{"d"},
		Suspended: []string{"e"},
		Routes:    map[string][]string{"a": {"c", "e"}, workflow.Start: {"a"}},
		Activated: []string{"a"},
		Attempts:  map[string]int{"a": 2}, Tries: map[string]int{"a": 1}, Visits: map[string]int{"a": 3},
		Steps:   9,
		Writers: map[string][]string{"text": {"a"}},
		Suspends: map[string][]*workflow.SuspendPoint{"e": {
			{Payload: json.RawMessage(`{"question":"ok?"}`), Resume: json.RawMessage(`"yes"`)},
			{Agent: true, Payload: json.RawMessage(`{"run_id":"x"}`)},
		}},
		Maps: map[string]*workflow.MapProgress{"m": {
			Items: []json.RawMessage{json.RawMessage(`1`), json.RawMessage(`{"x":2}`)},
			Done:  map[int][]workflow.ItemWrite{0: {{Key: "out", Value: "one"}, {Key: "gone", Deleted: true}, {Key: "set", Value: []any{1.0}, Replace: true}}},
		}},
		Writes:    map[string]workflow.ItemWrite{"text": {Key: "text", Value: map[string]any{"v": "w"}}},
		UpdatedAt: time.Date(2024, 1, 2, 3, 4, 5, 987654321, time.UTC),
	}
}

func testRoundTrip(t *testing.T, cp workflow.Checkpointer) {
	c := fullCheckpoint("round-trip")
	want := canonical(t, c)
	save(t, cp, c)
	got := load(t, cp, "round-trip")
	if g := canonical(t, got); g != want {
		t.Fatalf("Load = %s\nwant %s", g, want)
	}
	if !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, c.UpdatedAt)
	}
	if !bytes.Equal(compact(t, got.Values["obj"]), []byte(`{"a":[1,null,true]}`)) {
		t.Fatalf("raw value = %s", got.Values["obj"])
	}
	empty := &workflow.Checkpoint{RunID: "empty"}
	save(t, cp, empty)
	if g, w := canonical(t, load(t, cp, "empty")), canonical(t, empty); g != w {
		t.Fatalf("empty checkpoint = %s, want %s", g, w)
	}
}

func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testIsolation(t *testing.T, cp workflow.Checkpointer) {
	c := fullCheckpoint("isolated")
	want := canonical(t, c)
	save(t, cp, c)
	c.Done[0] = "caller mutation"
	c.Values["n"][0] = '9'
	c.Routes["a"][0] = "caller mutation"
	c.Attempts["a"] = 99
	c.Suspends["e"][0].Resume = json.RawMessage(`"no"`)
	got := load(t, cp, "isolated")
	if g := canonical(t, got); g != want {
		t.Fatalf("checkpoint changed through the saved one: %s", g)
	}
	got.Done[0] = "reader mutation"
	got.Values["n"] = json.RawMessage(`7`)
	got.Maps["m"].Done[0][0].Key = "reader mutation"
	if g := canonical(t, load(t, cp, "isolated")); g != want {
		t.Fatalf("checkpoint changed through a loaded one: %s", g)
	}
}

func testOverwrite(t *testing.T, cp workflow.Checkpointer) {
	save(t, cp, fullCheckpoint("overwrite"))
	next := &workflow.Checkpoint{RunID: "overwrite", Status: workflow.StatusCompleted, Steps: 10, Done: []string{"z"}}
	save(t, cp, next)
	if g, w := canonical(t, load(t, cp, "overwrite")), canonical(t, next); g != w {
		t.Fatalf("overwritten checkpoint = %s, want %s", g, w)
	}
}

func testDelete(t *testing.T, cp workflow.Checkpointer) {
	save(t, cp, fullCheckpoint("deleted"))
	save(t, cp, fullCheckpoint("kept"))
	if err := cp.Delete(t.Context(), "deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Load(t.Context(), "deleted"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Load deleted = %v, want ErrCheckpointNotFound", err)
	}
	if err := cp.Delete(t.Context(), "deleted"); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	load(t, cp, "kept")
	save(t, cp, &workflow.Checkpoint{RunID: "deleted", Steps: 9})
	if got := load(t, cp, "deleted"); got.Steps != 9 {
		t.Fatalf("checkpoint saved after Delete = %+v", got)
	}
}

func testEncodeFailure(t *testing.T, cp workflow.Checkpointer) {
	save(t, cp, &workflow.Checkpoint{RunID: "encode", Steps: 1})
	for _, bad := range []*workflow.Checkpoint{
		{RunID: "encode", Steps: 2, Values: map[string]json.RawMessage{"v": json.RawMessage(`{not json`)}},
		{RunID: "encode", Steps: 3, Writes: map[string]workflow.ItemWrite{"v": {Key: "v", Value: make(chan int)}}},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Save of an unencodable checkpoint panicked: %v", r)
				}
			}()
			if err := cp.Save(t.Context(), bad); err == nil {
				t.Errorf("Save accepted an unencodable checkpoint: %+v", bad)
			}
		}()
		if got := load(t, cp, "encode"); got.Steps != 1 {
			t.Fatalf("failed Save changed the checkpoint: %+v", got)
		}
	}
}

func testIDs(t *testing.T, cp workflow.Checkpointer) {
	ctx := t.Context()
	valid := []string{"a", "_", "-", "0", "wf_ABC-123", strings.Repeat("w", 64), workflow.SubRunID("wf", "node", 1, 2)}
	for i, id := range valid {
		save(t, cp, &workflow.Checkpoint{RunID: id, Steps: i})
	}
	for i, id := range valid {
		if got := load(t, cp, id); got.RunID != id || got.Steps != i {
			t.Fatalf("Load %q = %+v; IDs must be distinct", id, got)
		}
	}
	checkCase(t, cp)
	invalid := []string{"", "dot.name", "..", "../escape", "a/b", `a\b`, "a b", "é", "a:b", "a\x00b", strings.Repeat("w", 65)}
	for _, id := range invalid {
		if err := cp.Save(ctx, &workflow.Checkpoint{RunID: id}); err == nil {
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

func checkCase(t *testing.T, cp workflow.Checkpointer) {
	ctx := t.Context()
	save(t, cp, &workflow.Checkpoint{RunID: "case", Steps: 1})
	if err := cp.Save(ctx, &workflow.Checkpoint{RunID: "CASE", Steps: 2}); err != nil {
		if _, err := cp.Load(ctx, "CASE"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
			t.Fatalf("Load of a rejected ID = %v, want ErrCheckpointNotFound", err)
		}
	} else if got := load(t, cp, "CASE"); got.RunID != "CASE" || got.Steps != 2 {
		t.Fatalf("Load CASE = %+v", got)
	}
	if got := load(t, cp, "case"); got.RunID != "case" || got.Steps != 1 {
		t.Fatalf("Load case = %+v; IDs differing in case were confused", got)
	}
	if err := cp.Delete(ctx, "CASE"); err != nil {
		t.Fatal(err)
	}
	if got := load(t, cp, "case"); got.Steps != 1 {
		t.Fatalf("deleting CASE changed case: %+v", got)
	}
}

func testLargePayload(t *testing.T, cp workflow.Checkpointer) {
	big, err := json.Marshal(strings.Repeat("large checkpoint ✓ ", 1<<17)) // 2.4 MiB
	if err != nil {
		t.Fatal(err)
	}
	c := &workflow.Checkpoint{RunID: "large", Values: map[string]json.RawMessage{"big": big}, Done: make([]string, 0, 5000)}
	for i := range 5000 {
		c.Done = append(c.Done, fmt.Sprintf("node-%d", i))
	}
	save(t, cp, c)
	got := load(t, cp, "large")
	if !bytes.Equal(got.Values["big"], big) || len(got.Done) != 5000 || got.Done[4999] != "node-4999" {
		t.Fatalf("large checkpoint changed: %d bytes, %d nodes", len(got.Values["big"]), len(got.Done))
	}
}

func testConcurrency(t *testing.T, cp workflow.Checkpointer) {
	const workers, rounds = 8, 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			own := fmt.Sprintf("run-%d", w)
			for r := range rounds {
				// Every field of a saved checkpoint carries the same number,
				// so a mixed checkpoint is detected.
				n := w*rounds + r
				c := &workflow.Checkpoint{RunID: "shared", Steps: n, Error: fmt.Sprint(n), Done: []string{fmt.Sprint(n)}}
				if err := cp.Save(t.Context(), c); err != nil {
					t.Error(err)
					return
				}
				got, err := cp.Load(t.Context(), "shared")
				if err != nil {
					t.Error(err)
					return
				}
				if got.Error != fmt.Sprint(got.Steps) || len(got.Done) != 1 || got.Done[0] != got.Error {
					t.Errorf("Load returned a mixed checkpoint: %+v", got)
					return
				}
				if err := cp.Save(t.Context(), &workflow.Checkpoint{RunID: own, Steps: r}); err != nil {
					t.Error(err)
					return
				}
				if got, err := cp.Load(t.Context(), own); err != nil || got.Steps != r {
					t.Errorf("Load %s = %+v, %v; want step %d", own, got, err, r)
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

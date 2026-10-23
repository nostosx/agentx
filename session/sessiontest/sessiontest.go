// Package sessiontest checks session.Store implementations against the
// contract the built-in stores follow.
package sessiontest

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
)

// Run runs the conformance tests against stores made by newStore, which
// must return an empty store independent of the others; it is called once
// per subtest and subtests run in parallel. The contract:
//
//   - Get of a missing session returns session.ErrNotFound; Delete of a
//     missing session succeeds.
//   - Save stores a snapshot: messages, state (as JSON values), ID and
//     timestamps round-trip, and later changes to the saved or loaded
//     session are not stored.
//   - Each Save increments the stored revision by one, starting at 1, and
//     Get returns the session at its stored revision. A session loaded
//     before its stored copy was deleted is stored again by Save, at the
//     next revision after the one it was loaded at.
//   - Saving a session loaded, created by session.GetOrCreate or saved
//     earlier returns session.ErrConflict when the stored session changed
//     since, without changing it; other sessions overwrite. This holds
//     for concurrent saves.
//   - List returns every session, most recently updated first, then by ID.
//   - IDs are case-sensitive, though a store whose medium cannot tell
//     IDs differing only in case apart may reject the second one; IDs
//     outside the pattern documented on session.Store are rejected by
//     Get, Save and Delete.
//   - Payloads of several megabytes are stored.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, session.Store)
	}{
		{"NotFound", testNotFound},
		{"RoundTrip", testRoundTrip},
		{"Isolation", testIsolation},
		{"Overwrite", testOverwrite},
		{"Revisions", testRevisions},
		{"Conflicts", testConflicts},
		{"SaveAfterDelete", testSaveAfterDelete},
		{"SaveFailure", testSaveFailure},
		{"List", testList},
		{"IDs", testIDs},
		{"LargePayload", testLargePayload},
		{"ConcurrentUpdates", testConcurrentUpdates},
		{"ConcurrentCreate", testConcurrentCreate},
		{"ConcurrentOverwrite", testConcurrentOverwrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newStore(t))
		})
	}
}

func testNotFound(t *testing.T, store session.Store) {
	ctx := t.Context()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
	if infos, err := store.List(ctx); err != nil || len(infos) != 0 {
		t.Fatalf("List empty store = %v, %v", infos, err)
	}
}

// richMessages covers every part type and characters that storage layers
// tend to mangle.
func richMessages() []*model.Message {
	return []*model.Message{
		model.SystemText("system prompt"),
		{Role: model.RoleUser, Name: "alice", Metadata: map[string]any{"source": "test", "n": 1.5},
			Parts: []model.Part{model.TextPart("unicode: héllo 世界 🚀, NUL: \x00, quote: \" ' \\ `"),
				model.ImagePart("image/png", []byte{0x89, 'P', 'N', 'G', 0, 0xff}), model.ImageURLPart("https://example.com/a.png")}},
		{Role: model.RoleAssistant, Parts: []model.Part{
			{Type: model.PartReasoning, Text: "thinking", Signature: "sig=="},
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi","n":[1,2.5,null]}`)}},
			{Type: model.PartRaw, Provider: "anthropic", Raw: json.RawMessage(`{"type":"server_tool_use"}`)},
		}},
		model.ToolResults(&model.ToolResult{CallID: "call-1", Name: "echo", Content: "hi", IsError: true}),
		// Media parts hold binary data, which JSON stores as base64, in
		// messages and in tool results.
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartDocument, MediaType: "application/pdf", Data: []byte("%PDF-1.7\x00\xff"), Filename: "report.pdf", Citations: true},
			model.AudioPart("audio/wav", []byte{'R', 'I', 'F', 'F', 0, 0x80}),
			{Type: model.PartVideo, URL: "https://www.youtube.com/watch?v=x", Detail: "low"},
			model.FilePart(model.PartDocument, "openai", "file-abc", "application/pdf"),
		}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call-2", Name: "screenshot", Arguments: json.RawMessage(`{}`)}}}},
		model.ToolResults(&model.ToolResult{CallID: "call-2", Name: "screenshot", Content: "screen",
			Parts:    []model.Part{model.ImagePart("image/png", []byte{0x89, 'P', 'N', 'G', 0}), model.TextDocumentPart("notes.txt", "notes")},
			Metadata: map[string]any{"width": 1280.0, "tags": []any{"a"}}}),
		model.AssistantText("done"),
	}
}

func richState() map[string]any {
	return map[string]any{
		"string": "value", "number": 42.5, "bool": true, "null": nil,
		"nested": map[string]any{"list": []any{"a", 1.0, map[string]any{"deep": "x"}}},
		// State an agent.Runner keeps: the agent a session was handed off
		// to, and checkpoints.
		"agentx.active_agent": "billing",
		"agentx.checkpoints":  []any{map[string]any{"id": "run_1", "index": 1.0, "hash": "ab", "time": "2024-05-06T07:08:09Z", "agent": "billing"}},
	}
}

var stamp = time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.FixedZone("UTC+2", 2*3600))

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func save(t *testing.T, store session.Store, s session.Session) {
	t.Helper()
	if err := store.Save(t.Context(), s); err != nil {
		t.Fatalf("Save %s = %v", s.ID(), err)
	}
}

func get(t *testing.T, store session.Store, id string) session.Session {
	t.Helper()
	s, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get %s = %v", id, err)
	}
	if s.ID() != id {
		t.Fatalf("Get %s returned session %s", id, s.ID())
	}
	return s
}

func texts(s session.Session) []string {
	var out []string
	for _, m := range s.Messages() {
		out = append(out, m.Text())
	}
	return out
}

func testRoundTrip(t *testing.T, store session.Store) {
	snap := &session.Snapshot{ID: "round-trip", Messages: richMessages(), State: richState(), CreatedAt: stamp, UpdatedAt: stamp.Add(time.Hour)}
	save(t, store, session.FromSnapshot(snap))
	got := session.TakeSnapshot(get(t, store, "round-trip"))
	if g, w := jsonOf(t, got.Messages), jsonOf(t, snap.Messages); g != w {
		t.Errorf("messages = %s\nwant %s", g, w)
	}
	if g, w := jsonOf(t, got.State), jsonOf(t, snap.State); g != w {
		t.Errorf("state = %s, want %s", g, w)
	}
	if !got.CreatedAt.Equal(snap.CreatedAt) || !got.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("times = %v, %v; want %v, %v", got.CreatedAt, got.UpdatedAt, snap.CreatedAt, snap.UpdatedAt)
	}
	if got.Revision != 1 {
		t.Errorf("revision after first save = %d, want 1", got.Revision)
	}
	var decoded struct {
		List []any `json:"list"`
	}
	wantList := []any{"a", 1.0, map[string]any{"deep": "x"}}
	if ok, err := get(t, store, "round-trip").State().Decode("nested", &decoded); !ok || err != nil || !reflect.DeepEqual(decoded.List, wantList) {
		t.Errorf("Decode nested state = %+v, %v, %v", decoded, ok, err)
	}
	// An empty session round-trips too.
	empty := session.New("empty")
	save(t, store, empty)
	if got := get(t, store, "empty"); len(got.Messages()) != 0 || len(got.State().All()) != 0 {
		t.Errorf("empty session = %v, %v", got.Messages(), got.State().All())
	}
}

func testIsolation(t *testing.T, store session.Store) {
	s := session.New("isolated")
	s.Append(model.UserText("saved"))
	nested := map[string]any{"values": []any{"saved"}}
	s.State().Set("nested", nested)
	save(t, store, s)
	s.Append(model.AssistantText("unsaved"))
	s.State().Set("extra", "unsaved")
	nested["values"].([]any)[0] = "unsaved mutation"

	loaded := get(t, store, "isolated")
	if got := texts(loaded); len(got) != 1 || got[0] != "saved" {
		t.Fatalf("messages = %q, want only the saved one", got)
	}
	if _, ok := loaded.State().Get("extra"); ok {
		t.Fatal("unsaved state value stored")
	}
	v, _ := loaded.State().Get("nested")
	values := v.(map[string]any)["values"].([]any)
	if values[0] != "saved" {
		t.Fatalf("nested value changed through the saved session: %v", values)
	}
	values[0] = "reader mutation"
	loaded.Replace([]*model.Message{model.UserText("reader mutation")})
	loaded.State().Set("extra", "reader mutation")

	again := get(t, store, "isolated")
	if got := texts(again); len(got) != 1 || got[0] != "saved" {
		t.Fatalf("messages changed through a loaded session: %q", got)
	}
	v, _ = again.State().Get("nested")
	if got := v.(map[string]any)["values"].([]any)[0]; got != "saved" {
		t.Fatalf("nested value changed through a loaded session: %v", got)
	}
	if _, ok := again.State().Get("extra"); ok {
		t.Fatal("state changed through a loaded session")
	}
}

func testOverwrite(t *testing.T, store session.Store) {
	s := session.New("overwrite")
	s.Append(model.UserText("one"), model.AssistantText("two"))
	s.State().Set("keep", "old")
	s.State().Set("drop", "old")
	save(t, store, s)
	s.Replace([]*model.Message{model.UserText("replaced")})
	s.State().Set("keep", "new")
	s.State().Delete("drop")
	save(t, store, s)
	got := get(t, store, "overwrite")
	if msgs := texts(got); len(msgs) != 1 || msgs[0] != "replaced" {
		t.Fatalf("messages = %q", msgs)
	}
	if g := jsonOf(t, got.State().All()); g != `{"keep":"new"}` {
		t.Fatalf("state = %s", g)
	}
	infos, err := store.List(t.Context())
	if err != nil || len(infos) != 1 || infos[0].Messages != 1 {
		t.Fatalf("List after overwrite = %+v, %v", infos, err)
	}
}

func revision(s session.Session) int64 { return session.TakeSnapshot(s).Revision }

func testRevisions(t *testing.T, store session.Store) {
	s := session.New("revisions")
	for want := int64(1); want <= 3; want++ {
		s.Append(model.UserText(fmt.Sprint(want)))
		save(t, store, s)
		if got := revision(s); got != want {
			t.Fatalf("revision of saved session = %d, want %d", got, want)
		}
		if got := revision(get(t, store, "revisions")); got != want {
			t.Fatalf("revision of loaded session = %d, want %d", got, want)
		}
	}
	// A session loaded and saved continues the count.
	loaded := get(t, store, "revisions")
	save(t, store, loaded)
	if got := revision(get(t, store, "revisions")); got != 4 {
		t.Fatalf("revision after saving a loaded session = %d, want 4", got)
	}
	// So does an untracked session that overwrites.
	save(t, store, session.New("revisions"))
	if got := revision(get(t, store, "revisions")); got != 5 {
		t.Fatalf("revision after overwrite = %d, want 5", got)
	}
}

func testConflicts(t *testing.T, store session.Store) {
	ctx := t.Context()
	s, created, err := session.GetOrCreate(ctx, store, "shared")
	if err != nil || !created {
		t.Fatalf("GetOrCreate = %v, %v", created, err)
	}
	s.Append(model.UserText("first"))
	save(t, store, s)
	// Saving the same session again is fine.
	save(t, store, s)
	a := get(t, store, "shared")
	b := get(t, store, "shared")
	a.Append(model.UserText("from a"))
	save(t, store, a)
	b.Append(model.UserText("from b"))
	if err := store.Save(ctx, b); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("stale Save = %v, want ErrConflict", err)
	}
	if err := store.Save(ctx, s); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("stale Save of a saved session = %v, want ErrConflict", err)
	}
	got := get(t, store, "shared")
	if msgs := texts(got); len(msgs) != 2 || msgs[1] != "from a" {
		t.Fatalf("stored messages after conflicts = %q", msgs)
	}
	if rev := revision(got); rev != 3 {
		t.Fatalf("revision after conflicts = %d, want 3", rev)
	}
	// Reloading and reapplying the change succeeds.
	got.Append(model.UserText("from b"))
	save(t, store, got)
	if msgs := texts(get(t, store, "shared")); len(msgs) != 3 || msgs[2] != "from b" {
		t.Fatalf("messages after retry = %q", msgs)
	}

	// A session GetOrCreate made while another writer created the same ID.
	fresh, created, err := session.GetOrCreate(ctx, store, "raced")
	if err != nil || !created {
		t.Fatalf("GetOrCreate = %v, %v", created, err)
	}
	other := session.New("raced")
	other.Append(model.UserText("winner"))
	save(t, store, other)
	fresh.Append(model.UserText("loser"))
	if err := store.Save(ctx, fresh); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("Save of raced new session = %v, want ErrConflict", err)
	}
	if msgs := texts(get(t, store, "raced")); len(msgs) != 1 || msgs[0] != "winner" {
		t.Fatalf("raced session = %q", msgs)
	}
	// GetOrCreate of a stored session loads it.
	existing, created, err := session.GetOrCreate(ctx, store, "raced")
	if err != nil || created || texts(existing)[0] != "winner" {
		t.Fatalf("GetOrCreate existing = %v, %v, %v", existing, created, err)
	}

	// Untracked sessions overwrite.
	save(t, store, session.New("shared"))
	if msgs := texts(get(t, store, "shared")); len(msgs) != 0 {
		t.Fatalf("untracked Save did not overwrite: %q", msgs)
	}
}

func testSaveAfterDelete(t *testing.T, store session.Store) {
	ctx := t.Context()
	s := session.New("deleted")
	s.Append(model.UserText("first"))
	save(t, store, s)
	loaded := get(t, store, "deleted")
	if err := store.Delete(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "deleted"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get deleted = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	// A session loaded before the deletion is stored again, at a revision
	// above the one it was loaded at.
	loaded.Append(model.UserText("second"))
	save(t, store, loaded)
	got := get(t, store, "deleted")
	if msgs := texts(got); len(msgs) != 2 || revision(got) != 2 {
		t.Fatalf("recreated session = %q at revision %d, want 2 messages at revision 2", msgs, revision(got))
	}
	// So the other copy loaded before the deletion conflicts with it.
	if err := store.Save(ctx, s); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("Save of a session older than the recreated one = %v, want ErrConflict", err)
	}
	// A session created anew after a deletion starts at revision 1.
	if err := store.Delete(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	fresh, created, err := session.GetOrCreate(ctx, store, "deleted")
	if err != nil || !created {
		t.Fatalf("GetOrCreate = %v, %v", created, err)
	}
	save(t, store, fresh)
	if rev := revision(get(t, store, "deleted")); rev != 1 {
		t.Fatalf("revision of a session created after deletion = %d, want 1", rev)
	}
}

func testSaveFailure(t *testing.T, store session.Store) {
	ctx := t.Context()
	s := session.New("failure")
	s.Append(model.UserText("saved"))
	save(t, store, s)
	s.Append(model.AssistantText("unsaved"))
	s.State().Set("bad", make(chan int))
	if err := store.Save(ctx, s); err == nil {
		t.Fatal("Save accepted unserializable state")
	}
	got := get(t, store, "failure")
	if msgs := texts(got); len(msgs) != 1 || revision(got) != 1 {
		t.Fatalf("failed Save changed the stored session: %q at revision %d", msgs, revision(got))
	}
	if _, ok := got.State().Get("bad"); ok {
		t.Fatal("failed Save stored state")
	}
	// The session is still at the stored revision and saves once fixed.
	s.State().Delete("bad")
	save(t, store, s)
	if got := get(t, store, "failure"); len(got.Messages()) != 2 || revision(got) != 2 {
		t.Fatalf("Save after failure = %q at revision %d", texts(got), revision(got))
	}
	bad := session.New("never-stored")
	bad.State().Set("bad", func() {})
	if err := store.Save(ctx, bad); err == nil {
		t.Fatal("Save accepted unserializable state")
	}
	if _, err := store.Get(ctx, "never-stored"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("failed Save of a new session stored it: %v", err)
	}
}

func testList(t *testing.T, store session.Store) {
	ctx := t.Context()
	base := time.Unix(1_700_000_000, 0).UTC()
	sessions := []struct {
		id       string
		created  time.Time
		updated  time.Time
		messages int
	}{
		{"b", base, base.Add(time.Second), 1},
		{"a", base, base.Add(time.Second), 2},
		{"newest", base.Add(-time.Hour), base.Add(time.Minute), 0},
		{"oldest", base, base.Add(-time.Minute), 3},
		{"nanos", base, base.Add(time.Second + time.Nanosecond), 1},
		{"zone", base, base.Add(2 * time.Second).In(time.FixedZone("UTC-5", -5*3600)), 1},
		{"deleted", base, base.Add(time.Hour), 1},
	}
	for _, s := range sessions {
		msgs := make([]*model.Message, s.messages)
		for i := range msgs {
			msgs[i] = model.UserText(s.id)
		}
		save(t, store, session.FromSnapshot(&session.Snapshot{ID: s.id, Messages: msgs, CreatedAt: s.created, UpdatedAt: s.updated}))
	}
	if err := store.Delete(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	infos, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"newest", "zone", "nanos", "a", "b", "oldest"}
	if len(infos) != len(want) {
		t.Fatalf("List = %+v, want %v", infos, want)
	}
	for i, id := range want {
		info := infos[i]
		if info.ID != id {
			t.Fatalf("List[%d] = %s, want order %v", i, info.ID, want)
		}
		for _, s := range sessions {
			if s.id == id && (info.Messages != s.messages || !info.CreatedAt.Equal(s.created) || !info.UpdatedAt.Equal(s.updated)) {
				t.Errorf("List[%d] = %+v, want %d messages, created %v, updated %v", i, info, s.messages, s.created, s.updated)
			}
		}
	}
}

func testIDs(t *testing.T, store session.Store) {
	ctx := t.Context()
	valid := []string{"a", "_", "-", "0", "v1.2", "a..b", "trailing.", "dash-and_under", strings.Repeat("x", 128), session.NewID()}
	for _, id := range valid {
		s := session.New(id)
		s.Append(model.UserText(id))
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save rejected valid ID %q: %v", id, err)
		}
	}
	for _, id := range valid {
		if msgs := texts(get(t, store, id)); len(msgs) != 1 || msgs[0] != id {
			t.Fatalf("Get %q = %q; IDs must be distinct and case-sensitive", id, msgs)
		}
	}
	if infos, err := store.List(ctx); err != nil || len(infos) != len(valid) {
		t.Fatalf("List = %d sessions, %v; want %d", len(infos), err, len(valid))
	}
	invalid := []string{"", ".hidden", "..", "../escape", "a/b", `a\b`, "a b", "tab\t", "é", "a:b", "a\x00b", strings.Repeat("x", 129)}
	for _, id := range invalid {
		// session.New replaces an empty ID with a new one.
		if err := store.Save(ctx, session.New(id)); err == nil && id != "" {
			t.Errorf("Save accepted invalid ID %q", id)
		}
		if _, err := store.Get(ctx, id); err == nil {
			t.Errorf("Get accepted invalid ID %q", id)
		}
		if err := store.Delete(ctx, id); err == nil {
			t.Errorf("Delete accepted invalid ID %q", id)
		}
	}
	checkCase(t, store)
}

// checkCase checks that IDs differing only in case are never confused. A
// store whose medium cannot tell them apart, such as a directory on a
// case-insensitive file system, may reject the second.
func checkCase(t *testing.T, store session.Store) {
	ctx := t.Context()
	lower := session.New("case")
	lower.Append(model.UserText("lower"))
	save(t, store, lower)
	upper := session.New("CASE")
	upper.Append(model.UserText("upper"))
	if err := store.Save(ctx, upper); err != nil {
		if _, err := store.Get(ctx, "CASE"); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("Get of a rejected ID = %v, want ErrNotFound", err)
		}
	} else if msgs := texts(get(t, store, "CASE")); len(msgs) != 1 || msgs[0] != "upper" {
		t.Fatalf("Get CASE = %q", msgs)
	}
	if msgs := texts(get(t, store, "case")); len(msgs) != 1 || msgs[0] != "lower" {
		t.Fatalf("Get case = %q; IDs differing in case were confused", msgs)
	}
	if err := store.Delete(ctx, "CASE"); err != nil {
		t.Fatal(err)
	}
	if msgs := texts(get(t, store, "case")); len(msgs) != 1 {
		t.Fatalf("deleting CASE changed case: %q", msgs)
	}
}

func testLargePayload(t *testing.T, store session.Store) {
	big := strings.Repeat("large payload ✓ ", 1<<17) // 2.25 MiB
	s := session.New("large")
	s.Append(model.UserText(big))
	for i := range 2000 {
		s.Append(model.AssistantText(fmt.Sprintf("message %d", i)))
	}
	s.State().Set("blob", strings.Repeat("s", 1<<20))
	save(t, store, s)
	got := get(t, store, "large")
	msgs := got.Messages()
	if len(msgs) != 2001 || msgs[0].Text() != big || msgs[2000].Text() != "message 1999" {
		t.Fatalf("large session has %d messages", len(msgs))
	}
	if v, _ := got.State().Get("blob"); v != strings.Repeat("s", 1<<20) {
		t.Fatal("large state value changed")
	}
}

// update appends text to the stored session, retrying on conflicts.
func update(t *testing.T, store session.Store, id, text string) {
	for {
		s, err := store.Get(t.Context(), id)
		if err != nil {
			t.Error(err)
			return
		}
		s.Append(model.UserText(text))
		err = store.Save(t.Context(), s)
		if err == nil {
			return
		}
		if !errors.Is(err, session.ErrConflict) {
			t.Error(err)
			return
		}
	}
}

func testConcurrentUpdates(t *testing.T, store session.Store) {
	save(t, store, session.New("counter"))
	const writers, updates = 8, 5
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for u := range updates {
				update(t, store, "counter", fmt.Sprintf("%d-%d", w, u))
			}
		})
	}
	wg.Wait()
	got := get(t, store, "counter")
	seen := map[string]bool{}
	for _, text := range texts(got) {
		if seen[text] {
			t.Fatalf("message %s stored twice", text)
		}
		seen[text] = true
	}
	if len(seen) != writers*updates {
		t.Fatalf("stored %d messages, want %d: updates were lost", len(seen), writers*updates)
	}
	if rev := revision(got); rev != writers*updates+1 {
		t.Fatalf("revision = %d, want %d", rev, writers*updates+1)
	}
}

func testConcurrentCreate(t *testing.T, store session.Store) {
	const writers = 8
	sessions := make([]session.Session, writers)
	for i := range sessions {
		s, created, err := session.GetOrCreate(t.Context(), store, "created")
		if err != nil || !created {
			t.Fatalf("GetOrCreate = %v, %v", created, err)
		}
		s.Append(model.UserText(fmt.Sprint(i)))
		sessions[i] = s
	}
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Go(func() { errs[i] = store.Save(t.Context(), s) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("sessions %d and %d were both created", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, session.ErrConflict):
			t.Fatalf("Save %d = %v, want nil or ErrConflict", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no session was created")
	}
	if msgs := texts(get(t, store, "created")); len(msgs) != 1 || msgs[0] != fmt.Sprint(winner) {
		t.Fatalf("stored %q, want the winner %d", msgs, winner)
	}
}

func testConcurrentOverwrite(t *testing.T, store session.Store) {
	const writers = 8
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			for j := range 3 {
				s := session.New("overwritten")
				s.Append(model.UserText(fmt.Sprintf("%d-%d", i, j)))
				if err := store.Save(t.Context(), s); err != nil {
					t.Errorf("untracked Save = %v", err)
				}
			}
		})
	}
	wg.Wait()
	got := get(t, store, "overwritten")
	if msgs := texts(got); len(msgs) != 1 {
		t.Fatalf("overwritten session = %q", msgs)
	}
	if rev := revision(got); rev != writers*3 {
		t.Fatalf("revision = %d, want %d", rev, writers*3)
	}
}

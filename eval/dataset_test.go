package eval_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/eval"
)

type question struct {
	Text  string   `json:"text"`
	Hints []string `json:"hints,omitempty"`
}

func TestDatasetRoundTrip(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[question, answer]{
		Name: "cities",
		Cases: []eval.Case[question, answer]{
			{Name: "kyoto", Input: question{Text: "Where is the Golden Pavilion? <b>"}, Expected: &answer{"Kyoto", 1},
				Metadata: map[string]any{"difficulty": "easy", eval.MetadataToolCalls: []any{map[string]any{"name": "search"}}}},
			{Name: "open", Input: question{Text: "Pick a city", Hints: []string{"any"}}},
		},
		Evaluators: []eval.Evaluator{eval.EqualsExpected()},
	}
	dir := t.TempDir()
	for _, name := range []string{"cities.json", "cities.jsonl", "other.NDJSON"} {
		path := filepath.Join(dir, name)
		if err := ds.Save(path); err != nil {
			t.Fatal(err)
		}
		got, err := eval.LoadDataset[question, answer](path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantName := "cities"
		if name == "other.NDJSON" {
			wantName = "other"
		}
		if got.Name != wantName || len(got.Cases) != 2 || got.Evaluators != nil {
			t.Fatalf("%s: dataset = %+v", name, got)
		}
		if !reflect.DeepEqual(got.Cases[0].Input, ds.Cases[0].Input) || *got.Cases[0].Expected != *ds.Cases[0].Expected ||
			got.Cases[0].Metadata["difficulty"] != "easy" || got.Cases[1].Expected != nil || got.Cases[1].Input.Hints[0] != "any" {
			t.Fatalf("%s: cases = %+v", name, got.Cases)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("<b>")) {
			t.Errorf("%s escapes HTML:\n%s", name, data)
		}
		if eval.FormatOf(path) == eval.FormatJSONL && bytes.Count(data, []byte("\n")) != 2 {
			t.Errorf("%s is not one case per line:\n%s", name, data)
		}
	}
	var b bytes.Buffer
	if err := (&eval.Dataset[string, string]{}).Write(&b, eval.FormatJSON); err != nil || !strings.Contains(b.String(), `"cases": []`) {
		t.Fatalf("empty dataset = %s, %v", b.String(), err)
	}
	if err := ds.Write(&b, "yaml"); err == nil {
		t.Fatal("unknown format written")
	}
}

func TestReadDataset(t *testing.T) {
	t.Parallel()
	jsonl := "{\"name\":\"a\",\"input\":\"x\",\"expected\":\"X\"}\n\n  \n{\"input\":\"y\"}" // no final newline
	d, err := eval.ReadDataset[string, string](strings.NewReader(jsonl), eval.FormatJSONL)
	if err != nil || len(d.Cases) != 2 || *d.Cases[0].Expected != "X" || d.Cases[1].Input != "y" {
		t.Fatalf("JSONL = %+v, %v", d, err)
	}
	d, err = eval.ReadDataset[string, string](strings.NewReader(`{"$schema":"s.json","cases":[{"input":"x"}]}`), eval.FormatJSON)
	if err != nil || d.Name != "" || len(d.Cases) != 1 {
		t.Fatalf("JSON = %+v, %v", d, err)
	}
	if d, err := eval.ReadDataset[string, string](strings.NewReader(`{"name":"empty"}`), eval.FormatJSON); err != nil || d.Name != "empty" || d.Cases != nil {
		t.Fatalf("no cases = %+v, %v", d, err)
	}
	for _, tc := range []struct {
		in   string
		f    eval.Format
		want string
	}{
		{`{"cases":[{"input":"x","expected_output":"X"}]}`, eval.FormatJSON, `case 1: unknown field "expected_output"`},
		{`{"cases":[{"name":"n"}]}`, eval.FormatJSON, `case 1: missing field "input"`},
		{`{"cases":[{"input":1}]}`, eval.FormatJSON, "case 1: json: cannot unmarshal number"},
		{`{"cases":[1]}`, eval.FormatJSON, "case 1: json: cannot unmarshal number"},
		{`{"cases":{}}`, eval.FormatJSON, "cases: json: cannot unmarshal object"},
		{`{"name":1}`, eval.FormatJSON, "name: json: cannot unmarshal number"},
		{`{"evaluators":[]}`, eval.FormatJSON, `unknown field "evaluators"`},
		{`[`, eval.FormatJSON, "unexpected end of JSON input"},
		{"{\"input\":\"x\"}\n{\"inptu\":\"y\"}\n", eval.FormatJSONL, `line 2: unknown field "inptu"`},
		{"{\"input\":\"x\"}\nnot json\n", eval.FormatJSONL, "line 2: invalid character"},
		{"", "csv", `unknown dataset format "csv"`},
	} {
		_, err := eval.ReadDataset[string, string](strings.NewReader(tc.in), tc.f)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ReadDataset(%q) = %v, want %q", tc.in, err, tc.want)
		}
	}
	if _, err := eval.LoadDataset[string, string](filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file loaded")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"cases":[{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.LoadDataset[string, string](bad); err == nil || !strings.Contains(err.Error(), "eval: dataset "+bad+": case 1") {
		t.Errorf("bad file = %v", err)
	}
}

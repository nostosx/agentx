package tool_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/nostosx/agentx/tool"
)

func TestSchemaTagDescription(t *testing.T) {
	t.Parallel()
	// Tags that do not start with a keyword are descriptions, as in
	// github.com/google/jsonschema-go: commas and later "=" need no
	// escaping.
	type Input struct {
		City     string `json:"city" jsonschema:"City name, e.g. Tokyo, Paris"`
		Pairs    string `json:"pairs" jsonschema:"Settings as key=value pairs, comma-separated"`
		Assign   string `json:"assign" jsonschema:"key=value pairs to apply"`
		Required string `json:"required" jsonschema:"required unless pairs is set"`
		Escaped  string `json:"escaped" jsonschema:"kept as written: a\\,b"`
		Untagged string `json:"untagged"`
	}
	s := tool.SchemaOf[Input]()
	for name, want := range map[string]string{
		"city":     "City name, e.g. Tokyo, Paris",
		"pairs":    "Settings as key=value pairs, comma-separated",
		"assign":   "key=value pairs to apply",
		"required": "required unless pairs is set",
		"escaped":  `kept as written: a\,b`,
		"untagged": "",
	} {
		if got := s.Properties[name].Description; got != want {
			t.Errorf("%s description = %q, want %q", name, got, want)
		}
	}
	if want := []string{"city", "pairs", "assign", "required", "escaped", "untagged"}; !slices.Equal(s.Required, want) {
		t.Errorf("required = %v, want %v", s.Required, want)
	}
}

func TestSchemaTagKeywords(t *testing.T) {
	t.Parallel()
	type Input struct {
		Unit   string    `json:"unit" jsonschema:"title=Unit,description=Temperature unit\\, as a symbol,enum=C,enum=F,default=C"`
		Level  int       `json:"level,omitempty" jsonschema:"enum=1,enum=2,enum=3,default=2,minimum=1,maximum=3"`
		Ratio  float64   `json:"ratio" jsonschema:"exclusiveMinimum=0,exclusiveMaximum=1,enum=0.25,enum=0.5"`
		Code   string    `json:"code" jsonschema:"minLength=2,maxLength=4,pattern=^[A-Z]+$"`
		Email  string    `json:"email" jsonschema:"format=email"`
		Tags   []string  `json:"tags" jsonschema:"minItems=1,maxItems=3,enum=a,enum=b,default=a,default=b,maxLength=1"`
		Scores []float64 `json:"scores,omitempty" jsonschema:"minimum=0,maximum=10"`
		Forced *string   `json:"forced" jsonschema:"required"`
		Opt    string    `json:"opt" jsonschema:"required=false, description=Optional"`
		Quoted int       `json:"quoted,string" jsonschema:"enum=1,enum=2"`
		Other  string    `json:"other" jsonschema:"description=Known,example=ignored,stray"`
	}
	s := tool.SchemaOf[Input]()
	p := s.Properties

	unit := p["unit"]
	if unit.Title != "Unit" || unit.Description != "Temperature unit, as a symbol" || !reflect.DeepEqual(unit.Enum, []any{"C", "F"}) || unit.Default != "C" {
		t.Errorf("unit = %+v", unit)
	}
	level := p["level"]
	if !reflect.DeepEqual(level.Enum, []any{int64(1), int64(2), int64(3)}) || level.Default != int64(2) || *level.Minimum != 1 || *level.Maximum != 3 {
		t.Errorf("level = %+v", level)
	}
	ratio := p["ratio"]
	if *ratio.ExclusiveMinimum != 0 || *ratio.ExclusiveMaximum != 1 || !reflect.DeepEqual(ratio.Enum, []any{0.25, 0.5}) {
		t.Errorf("ratio = %+v", ratio)
	}
	if code := p["code"]; *code.MinLength != 2 || *code.MaxLength != 4 || code.Pattern != "^[A-Z]+$" {
		t.Errorf("code = %+v", code)
	}
	if p["email"].Format != "email" {
		t.Errorf("email = %+v", p["email"])
	}
	// On slices, value constraints apply to the elements, and minItems,
	// maxItems and default to the slice.
	tags := p["tags"]
	if *tags.MinItems != 1 || *tags.MaxItems != 3 || tags.Enum != nil || !reflect.DeepEqual(tags.Default, []any{"a", "b"}) ||
		!reflect.DeepEqual(tags.Items.Enum, []any{"a", "b"}) || *tags.Items.MaxLength != 1 {
		t.Errorf("tags = %+v, items %+v", tags, tags.Items)
	}
	if scores := p["scores"]; scores.Minimum != nil || *scores.Items.Minimum != 0 || *scores.Items.Maximum != 10 {
		t.Errorf("scores = %+v, items %+v", scores, scores.Items)
	}
	if opt := p["opt"]; opt.Description != "Optional" {
		t.Errorf("opt = %+v", opt)
	}
	// A ",string" field is a string, so its enum values are too.
	if quoted := p["quoted"]; quoted.Type != "string" || !reflect.DeepEqual(quoted.Enum, []any{"1", "2"}) {
		t.Errorf("quoted = %+v", quoted)
	}
	if other := p["other"]; other.Description != "Known" {
		t.Errorf("other = %+v", other)
	}
	if want := []string{"unit", "ratio", "code", "email", "tags", "forced", "quoted", "other"}; !slices.Equal(s.Required, want) {
		t.Errorf("required = %v, want %v", s.Required, want)
	}

	valid := `{"unit":"F","level":3,"ratio":0.5,"code":"AB","email":"a@b.c","tags":["a"],"scores":[0,10],"forced":"x","quoted":"2","other":""}`
	if err := s.Validate(json.RawMessage(valid)); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	for _, bad := range []string{
		`{"unit":"K","ratio":0.5,"code":"AB","email":"","tags":["a"],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","level":4,"ratio":0.5,"code":"AB","email":"","tags":["a"],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":1,"code":"AB","email":"","tags":["a"],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"ab","email":"","tags":["a"],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"AB","email":"","tags":[],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"AB","email":"","tags":["c"],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"AB","email":"","tags":["a"],"scores":[11],"forced":"x","quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"AB","email":"","tags":["a"],"quoted":"1","other":""}`,
		`{"unit":"C","ratio":0.5,"code":"AB","email":"","tags":["a"],"forced":"x","quoted":"3","other":""}`,
	} {
		if err := s.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSchemaTagNullable(t *testing.T) {
	t.Parallel()
	type Input struct {
		Note  string `json:"note" jsonschema:"nullable,title=Note,description=Free text"`
		Count int    `json:"count" jsonschema:"nullable=true,minimum=1"`
		Any   any    `json:"any" jsonschema:"nullable"`
	}
	s := tool.SchemaOf[Input]()
	note := s.Properties["note"]
	if note.Title != "Note" || note.Description != "Free text" || len(note.AnyOf) != 2 || note.AnyOf[0].Type != "string" || note.AnyOf[0].Title != "" || note.AnyOf[1].Type != "null" {
		t.Fatalf("note = %+v", note)
	}
	if count := s.Properties["count"]; len(count.AnyOf) != 2 || *count.AnyOf[0].Minimum != 1 {
		t.Fatalf("count = %+v", count)
	}
	if a := s.Properties["any"]; a.Type != "" || a.AnyOf != nil {
		t.Fatalf("any = %+v", a)
	}
	if !slices.Equal(s.Required, []string{"note", "count", "any"}) {
		t.Errorf("required = %v", s.Required)
	}
	if err := s.Validate(json.RawMessage(`{"note":null,"count":null,"any":null}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(json.RawMessage(`{"note":"x","count":0,"any":1}`)); err == nil {
		t.Error("count below its minimum accepted")
	}
}

func TestSchemaTagOmit(t *testing.T) {
	t.Parallel()
	type Input struct {
		Kept     string `json:"kept"`
		Internal string `json:"internal" jsonschema:"-"`
		// encoding/json ignores the field, so the jsonschema tag does not
		// bring it back.
		Ignored string `json:"-" jsonschema:"description=Never sent"`
	}
	s := tool.SchemaOf[Input]()
	if _, ok := s.Properties["internal"]; ok {
		t.Error(`field tagged jsonschema:"-" included`)
	}
	if _, ok := s.Properties["Ignored"]; ok {
		t.Error(`field tagged json:"-" included`)
	}
	if !slices.Equal(s.Required, []string{"kept"}) {
		t.Errorf("required = %v", s.Required)
	}
}

func TestSchemaTagInvalidValues(t *testing.T) {
	t.Parallel()
	type Input struct {
		N int    `json:"n" jsonschema:"minimum=low,maxLength=-1,minItems=x,required=maybe,enum=1,enum=two"`
		S string `json:"s" jsonschema:"default="`
	}
	s := tool.SchemaOf[Input]()
	n := s.Properties["n"]
	if n.Minimum != nil || n.MaxLength != nil || n.MinItems != nil {
		t.Errorf("invalid limits were set: %+v", n)
	}
	// Values that are not of the field's type are kept as strings, so the
	// enum still lists them.
	if !reflect.DeepEqual(n.Enum, []any{int64(1), "two"}) {
		t.Errorf("enum = %v", n.Enum)
	}
	if d := s.Properties["s"].Default; d != "" {
		t.Errorf("default = %#v, want the empty string", d)
	}
	if !slices.Equal(s.Required, []string{"n", "s"}) {
		t.Errorf("required = %v", s.Required)
	}
}

func TestSchemaExclusiveLimits(t *testing.T) {
	t.Parallel()
	s := &tool.Schema{Type: "number", ExclusiveMinimum: new(0.0), ExclusiveMaximum: new(1.0)}
	for raw, ok := range map[string]bool{"0.5": true, "0": false, "1": false, "-1": false} {
		if err := s.Validate(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("Validate(%s) = %v", raw, err)
		}
	}
	if got := string(s.JSON()); got != `{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":1}` {
		t.Errorf("JSON = %s", got)
	}
}

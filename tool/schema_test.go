package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/tool"
)

func TestSchemaOf(t *testing.T) {
	t.Parallel()
	type Embedded struct {
		ID int `json:"id"`
	}
	type Input struct {
		Embedded
		Name     string          `json:"name" jsonschema:"enum=a,enum=b,default=a,description=display name"`
		Count    int             `json:"count,omitempty" jsonschema:"default=2"`
		Enabled  bool            `json:"enabled" jsonschema:"default=true,required=false"`
		Ratio    float64         `json:"ratio,omitzero" jsonschema:"default=1.5"`
		Optional *string         `json:"optional"`
		Forced   *string         `json:"forced" jsonschema:"required"`
		At       time.Time       `json:"at"`
		Raw      json.RawMessage `json:"raw"`
		Bytes    []byte          `json:"bytes"`
		Items    []int           `json:"items"`
		Hidden   string          `json:"-"`
		private  string
	}
	sample := Input{private: "hidden fixture"}
	if value := reflect.ValueOf(sample).FieldByName("private"); !value.IsValid() || value.String() != "hidden fixture" {
		t.Fatal("fixture must contain an unexported field")
	}
	schema := tool.SchemaOf[Input]()
	if schema.Type != "object" || !reflect.DeepEqual(schema.Required, []string{"id", "name", "forced", "at", "raw", "bytes", "items"}) {
		t.Fatalf("schema=%+v", schema)
	}
	if schema.Properties["name"].Description != "display name" || !reflect.DeepEqual(schema.Properties["name"].Enum, []any{"a", "b"}) || schema.Properties["name"].Default != "a" {
		t.Fatalf("name schema=%+v", schema.Properties["name"])
	}
	if schema.Properties["count"].Default != int64(2) || schema.Properties["enabled"].Default != true || schema.Properties["ratio"].Default != 1.5 {
		t.Fatal("typed defaults were not parsed")
	}
	if schema.Properties["at"].Format != "date-time" || schema.Properties["bytes"].Format != "byte" || schema.Properties["raw"].Type != "" || schema.Properties["items"].Items.Type != "integer" {
		t.Fatal("special types mapped incorrectly")
	}
	if _, ok := schema.Properties["Hidden"]; ok {
		t.Error("ignored field included")
	}
	if _, ok := schema.Properties["private"]; ok {
		t.Error("unexported field included")
	}
	type Recursive struct {
		Child *Recursive `json:"child"`
	}
	if s := tool.SchemaOf[Recursive](); s.Properties["child"].Type != "object" {
		t.Fatalf("recursive schema=%+v", s)
	}
	if !json.Valid(schema.JSON()) || !json.Valid((*tool.Schema)(nil).JSON()) {
		t.Fatal("schema JSON is invalid")
	}
}

func TestSchemaValidate(t *testing.T) {
	t.Parallel()
	schema := tool.Object(map[string]*tool.Schema{
		"name":  {Type: "string", MinLength: new(2), MaxLength: new(3), Enum: []any{"你好", "abc"}},
		"count": {Type: "integer", Minimum: new(1.0), Maximum: new(5.0)},
		"items": {Type: "array", Items: tool.Boolean(""), MinItems: new(1), MaxItems: new(2)},
	}, "name", "count")
	schema.AdditionalProperties = new(false)
	tests := []struct{ name, raw, want string }{
		{"valid unicode", `{"name":"你好","count":2,"items":[true]}`, ""},
		{"integral exponent", `{"name":"abc","count":2e0}`, ""},
		{"required", `{}`, `missing required property "name"`},
		{"wrong root", `[]`, "expected object"},
		{"null", `null`, "got null"},
		{"malformed", `{`, "not valid JSON"},
		{"two values", `{"name":"abc","count":2} {}`, "exactly one JSON value"},
		{"duplicate name", `{"name":"abc","count":2,"count":9}`, `duplicate object member name "count"`},
		{"nested duplicate", `{"name":"abc","count":2,"items":[{"a":1,"a":2}]}`, "duplicate object member name"},
		{"invalid UTF-8", "{\"name\":\"ab\xff\",\"count\":2}", "invalid UTF-8"},
		{"lone surrogate", `{"name":"\ud800","count":2}`, "invalid surrogate"},
		{"enum", `{"name":"def","count":2}`, "one of"},
		{"short", `{"name":"a","count":2}`, "at least 2 characters"},
		{"long", `{"name":"abcd","count":2}`, "at most 3 characters"},
		{"fraction", `{"name":"abc","count":2.5}`, "expected integer"},
		{"minimum", `{"name":"abc","count":0}`, "must be >= 1"},
		{"maximum", `{"name":"abc","count":6}`, "must be <= 5"},
		{"additional", `{"name":"abc","count":2,"extra":0}`, `unexpected property "extra"`},
		{"case variant", `{"name":"abc","count":2,"Count":9}`, `property "Count" differs from "count" only in case`},
		{"unicode case variant", `{"name":"abc","count":2,"itemſ":[true]}`, `property "itemſ" differs from "items" only in case`},
		{"array minimum", `{"name":"abc","count":2,"items":[]}`, "at least 1 items"},
		{"array maximum", `{"name":"abc","count":2,"items":[true,false,true]}`, "at most 2 items"},
		{"array nested", `{"name":"abc","count":2,"items":[1]}`, "$.items[0]: expected boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schema.Validate(json.RawMessage(tt.raw))
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ve *tool.ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate=%v, want %q", err, tt.want)
			}
		})
	}
	union := &tool.Schema{AnyOf: []*tool.Schema{tool.String(""), {Type: "null"}}}
	for _, raw := range []string{`"yes"`, `null`} {
		if err := union.Validate(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := union.Validate(json.RawMessage(`true`)); err == nil {
		t.Fatal("union accepted boolean")
	}
	if err := (*tool.Schema)(nil).Validate(json.RawMessage(`garbage`)); err != nil {
		t.Fatal("nil schema should disable validation")
	}
	if err := tool.Object(nil).Validate(nil); err != nil {
		t.Fatalf("empty arguments=%v", err)
	}
	if !slices.Contains(schema.Required, "name") {
		t.Fatal("validation mutated schema")
	}
}

func TestSchemaValidateRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{} {}`, `{} garbage`} {
		if err := tool.Object(nil).Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted trailing JSON: %q", raw)
		}
	}
}

func TestSchemaValidatePattern(t *testing.T) {
	t.Parallel()
	s := &tool.Schema{Type: "string", Pattern: `^[a-z]+$`}
	if err := s.Validate(json.RawMessage(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(json.RawMessage(`"123"`)); err == nil {
		t.Error("accepted string that violates Pattern")
	}
}

func TestSchemaValidateEnumPreservesJSONTypes(t *testing.T) {
	t.Parallel()
	s := &tool.Schema{Enum: []any{1, true}}
	for _, raw := range []string{`1`, `true`} {
		if err := s.Validate(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`"1"`, `"true"`} {
		if err := s.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted string %s as numeric or boolean enum", raw)
		}
	}
}

func TestSchemaValidateComplexEnum(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		enum  []any
		raw   string
		valid bool
	}{
		{"equivalent number", []any{1}, `1.0`, true},
		{"equivalent exponent", []any{json.Number("1e3")}, `1000.0`, true},
		{"nested object", []any{map[string]any{"items": []any{1, true, "x"}}}, `{"items":[1.0,true,"x"]}`, true},
		{"nested type mismatch", []any{map[string]any{"items": []any{1, true, "x"}}}, `{"items":["1",true,"x"]}`, false},
		{"different object fields", []any{map[string]any{"x": 1}}, `{"x":1,"y":2}`, false},
		{"large equal integer", []any{json.Number("9007199254740993")}, `9007199254740993`, true},
		{"large unequal integer", []any{json.Number("9007199254740993")}, `9007199254740992`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &tool.Schema{Enum: tt.enum}
			if err := s.Validate(json.RawMessage(tt.raw)); (err == nil) != tt.valid {
				t.Fatalf("Validate(%s)=%v, valid=%v", tt.raw, err, tt.valid)
			}
		})
	}
}

func TestSchemaValidateEnumNullMembership(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		enum  []any
		valid bool
	}{
		{"null absent", []any{1}, false},
		{"null present", []any{nil, 1}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &tool.Schema{Enum: tt.enum}
			if err := s.Validate(json.RawMessage(`null`)); (err == nil) != tt.valid {
				t.Fatalf("Validate(null)=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestSchemaValidateAnyOfSiblingConstraints(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		schema *tool.Schema
		raw    string
		valid  bool
	}{
		{"matching sibling type", &tool.Schema{Type: "string", AnyOf: []*tool.Schema{tool.String(""), tool.Integer("")}}, `"ok"`, true},
		{"violating sibling type", &tool.Schema{Type: "string", AnyOf: []*tool.Schema{tool.String(""), tool.Integer("")}}, `1`, false},
		{"violating minimum", &tool.Schema{Minimum: new(2.0), AnyOf: []*tool.Schema{tool.Integer(""), tool.String("")}}, `1`, false},
		{"matching minimum", &tool.Schema{Minimum: new(2.0), AnyOf: []*tool.Schema{tool.Integer(""), tool.String("")}}, `3`, true},
		{"violating enum", &tool.Schema{Enum: []any{"allowed"}, AnyOf: []*tool.Schema{tool.String("")}}, `"other"`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.schema.Validate(json.RawMessage(tt.raw)); (err == nil) != tt.valid {
				t.Fatalf("Validate(%s)=%v, valid=%v", tt.raw, err, tt.valid)
			}
		})
	}
}

func TestSchemaValidateNullOptional(t *testing.T) {
	t.Parallel()
	type Input struct {
		A string  `json:"a"`
		B *string `json:"b"`
		C int     `json:"c,omitempty"`
	}
	s := tool.SchemaOf[Input]()
	if err := s.Validate(json.RawMessage(`{"a":"x","b":null,"c":null}`)); err != nil {
		t.Fatalf("null optional properties rejected: %v", err)
	}
	if err := s.Validate(json.RawMessage(`{"a":null}`)); err == nil {
		t.Fatal("null required property accepted")
	}
}

func TestStrictJSON(t *testing.T) {
	t.Parallel()
	type Item struct {
		Name string `json:"name"`
		Note string `json:"note,omitempty" jsonschema:"free text"`
	}
	type Input struct {
		Query string `json:"query"`
		Limit *int   `json:"limit" jsonschema:"max results"`
		Items []Item `json:"items,omitempty"`
		Mode  any    `json:"mode,omitempty"`
	}
	s := tool.SchemaOf[Input]()
	var got map[string]any
	if err := json.Unmarshal(s.StrictJSON(), &got); err != nil {
		t.Fatal(err)
	}
	if got["additionalProperties"] != false || !reflect.DeepEqual(got["required"], []any{"items", "limit", "mode", "query"}) {
		t.Fatalf("top level: %v", got)
	}
	props := got["properties"].(map[string]any)
	limit := props["limit"].(map[string]any)
	if limit["description"] != "max results" || !reflect.DeepEqual(limit["anyOf"], []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}) {
		t.Fatalf("limit: %v", limit)
	}
	if props["query"].(map[string]any)["type"] != "string" {
		t.Fatalf("query: %v", props["query"])
	}
	items := props["items"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false || !reflect.DeepEqual(items["required"], []any{"name", "note"}) {
		t.Fatalf("nested object: %v", items)
	}
	if note := items["properties"].(map[string]any)["note"].(map[string]any); note["description"] != "free text" || note["anyOf"] == nil {
		t.Fatalf("nested optional: %v", note)
	}
	// The original schema is unchanged and still accepts the null values
	// that strict generation produces.
	if s.AdditionalProperties != nil || slices.Contains(s.Required, "limit") {
		t.Fatalf("StrictJSON modified the schema: %+v", s)
	}
	if err := s.Validate(json.RawMessage(`{"query":"q","limit":null,"items":null,"mode":null}`)); err != nil {
		t.Fatal(err)
	}
	if string((*tool.Schema)(nil).StrictJSON()) != `{"type":"object","properties":{},"required":[],"additionalProperties":false}` {
		t.Fatalf("nil schema: %s", (*tool.Schema)(nil).StrictJSON())
	}
}

func TestSpecUsesStrictSchema(t *testing.T) {
	t.Parallel()
	type Input struct {
		A string `json:"a,omitempty"`
	}
	fn := func(context.Context, Input) (string, error) { return "", nil }
	strict := tool.Spec(tool.Func("f", "", fn, tool.Strict()))
	if !strict.Strict || !strings.Contains(string(strict.Parameters), `"additionalProperties":false`) {
		t.Fatalf("strict spec: %+v %s", strict, strict.Parameters)
	}
	plain := tool.Spec(tool.Func("f", "", fn))
	if strings.Contains(string(plain.Parameters), "additionalProperties") {
		t.Fatalf("plain spec: %s", plain.Parameters)
	}
}

type schemaInner struct {
	Shadowed string `json:"shadowed"`
	Inner    string `json:"inner"`
	Tie      string
}

type schemaOther struct {
	Tie string
	Tg  string `json:"Tagged"`
}

type schemaUnexported struct {
	Promoted string `json:"promoted"`
}

type schemaTagWins struct {
	Tagged string
}

type schemaJSONRules struct {
	schemaInner
	schemaOther
	schemaUnexported
	schemaTagWins
	Shadowed string      `json:"shadowed"`
	Named    schemaOther `json:"named"`
	Quoted   int         `json:"quoted,string"`
	QuotedP  *bool       `json:"quoted_p,string"`
	Fixed    [4]byte     `json:"fixed"`
	Tags     []string    `json:"tags" jsonschema:"enum=a,enum=b"`
	Levels   [2]int      `json:"levels" jsonschema:"enum=1,enum=2"`
}

func TestSchemaOfFollowsEncodingJSON(t *testing.T) {
	t.Parallel()
	v := schemaJSONRules{QuotedP: new(true), Tags: []string{"a"}, Levels: [2]int{1, 2}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(b, &encoded); err != nil {
		t.Fatal(err)
	}
	s := tool.SchemaOf[schemaJSONRules]()
	if got, want := slices.Sorted(maps.Keys(s.Properties)), slices.Sorted(maps.Keys(encoded)); !slices.Equal(got, want) {
		t.Fatalf("properties = %v, encoding/json uses %v", got, want)
	}
	if want := []string{"inner", "Tagged", "promoted", "shadowed", "named", "quoted", "fixed", "tags", "levels"}; !slices.Equal(s.Required, want) {
		t.Errorf("required = %v, want %v", s.Required, want)
	}
	if p := s.Properties["quoted"]; p.Type != "string" {
		t.Errorf("quoted = %+v, want a string", p)
	}
	if p := s.Properties["quoted_p"]; p.Type != "string" {
		t.Errorf("quoted_p = %+v, want a string", p)
	}
	if p := s.Properties["fixed"]; p.Type != "array" || p.Items.Type != "integer" {
		t.Errorf("fixed = %+v, want an array of integers", p)
	}
	if p := s.Properties["tags"]; p.Enum != nil || !reflect.DeepEqual(p.Items.Enum, []any{"a", "b"}) {
		t.Errorf("tags enum = %v, items %v", p.Enum, p.Items.Enum)
	}
	if p := s.Properties["levels"]; !reflect.DeepEqual(p.Items.Enum, []any{int64(1), int64(2)}) {
		t.Errorf("levels items enum = %v", p.Items.Enum)
	}
	// What encoding/json produces must validate, and decode back.
	if err := s.Validate(b); err != nil {
		t.Errorf("encoded value does not validate: %v\n%s", err, b)
	}
	if err := s.Validate(json.RawMessage(`{"inner":"","promoted":"","Tagged":"","shadowed":"","named":{"Tie":"","Tagged":""},"quoted":"1","fixed":[1,2,3,4],"tags":["c"],"levels":[1,2]}`)); err == nil {
		t.Error("enum of slice items not enforced")
	}
	var round schemaJSONRules
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
}

type schemaPtrInner struct {
	Hidden string `json:"hidden"`
	Shared string `json:"shared"`
}

type SchemaPtrOther struct {
	Shared string `json:"shared"`
	Kept   string `json:"kept"`
}

type schemaUnexportedPtr struct {
	*schemaPtrInner
	*SchemaPtrOther
	Y int `json:"y"`
}

func TestSchemaOfSkipsUnexportedEmbeddedPointer(t *testing.T) {
	t.Parallel()
	s := tool.SchemaOf[schemaUnexportedPtr]()
	// encoding/json cannot decode hidden, and the tie on shared drops it.
	if got, want := slices.Sorted(maps.Keys(s.Properties)), []string{"kept", "y"}; !slices.Equal(got, want) {
		t.Fatalf("properties = %v, want %v", got, want)
	}
	if want := []string{"kept", "y"}; !slices.Equal(s.Required, want) {
		t.Errorf("required = %v, want %v", s.Required, want)
	}
	var v schemaUnexportedPtr
	if err := json.Unmarshal([]byte(`{"hidden":"x"}`), &v); err == nil {
		t.Fatal("encoding/json decoded a field of an unexported embedded pointer")
	}
	if err := json.Unmarshal([]byte(`{"kept":"x","y":1}`), &v); err != nil {
		t.Fatalf("decode of the listed fields: %v", err)
	}
}

func TestSchemaOfFixedArrayLength(t *testing.T) {
	t.Parallel()
	type Input struct {
		Pair [2]int `json:"pair"`
	}
	s := tool.SchemaOf[Input]()
	p := s.Properties["pair"]
	if p.MinItems == nil || *p.MinItems != 2 || p.MaxItems == nil || *p.MaxItems != 2 {
		t.Fatalf("pair = %+v, want exactly 2 items", p)
	}
	if err := s.Validate(json.RawMessage(`{"pair":[1,2,3]}`)); err == nil {
		t.Error("three items validated")
	}
	if err := s.Validate(json.RawMessage(`{"pair":[1,2]}`)); err != nil {
		t.Error(err)
	}
}

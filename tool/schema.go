package tool

import (
	"bytes"
	"cmp"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Schema is a JSON Schema subset suitable for describing tool arguments.
type Schema struct {
	Type                 string             `json:"type,omitempty"`
	Title                string             `json:"title,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Default              any                `json:"default,omitempty"`
	Format               string             `json:"format,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	ExclusiveMinimum     *float64           `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *float64           `json:"exclusiveMaximum,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
	AnyOf                []*Schema          `json:"anyOf,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
}

// Object returns an object schema with the given properties. Every property
// listed in required must exist in props.
func Object(props map[string]*Schema, required ...string) *Schema {
	if props == nil {
		props = map[string]*Schema{}
	}
	return &Schema{Type: "object", Properties: props, Required: required}
}

// String returns a string schema.
func String(description string) *Schema { return &Schema{Type: "string", Description: description} }

// Integer returns an integer schema.
func Integer(description string) *Schema { return &Schema{Type: "integer", Description: description} }

// Number returns a number schema.
func Number(description string) *Schema { return &Schema{Type: "number", Description: description} }

// Boolean returns a boolean schema.
func Boolean(description string) *Schema { return &Schema{Type: "boolean", Description: description} }

// Array returns an array schema.
func Array(items *Schema, description string) *Schema {
	return &Schema{Type: "array", Items: items, Description: description}
}

// Enum returns a string schema restricted to values.
func Enum(description string, values ...string) *Schema {
	s := String(description)
	for _, v := range values {
		s.Enum = append(s.Enum, v)
	}
	return s
}

// JSON returns the schema encoded as JSON.
func (s *Schema) JSON() json.RawMessage {
	if s == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	b, err := json.Marshal(s)
	if err != nil {
		// Schema contains only JSON-safe values; Default is the only field
		// that can fail and is caller supplied.
		panic(fmt.Sprintf("tool: marshal schema: %v", err))
	}
	return b
}

// StrictJSON returns the schema encoded for providers' strict modes, which
// require every object to set additionalProperties to false and to list all
// of its properties as required. Optional properties become nullable
// instead, and Validate accepts null for them.
func (s *Schema) StrictJSON() json.RawMessage {
	if s == nil || (s.Type == "object" && len(s.Properties) == 0 && s.Description == "") {
		// Encode the empty object explicitly; omitempty would drop the
		// empty properties and required lists.
		return json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)
	}
	return s.strict().JSON()
}

// strict returns a copy of s converted for strict mode.
func (s *Schema) strict() *Schema {
	if s == nil {
		return nil
	}
	c := *s
	c.Items = s.Items.strict()
	if s.AnyOf != nil {
		c.AnyOf = make([]*Schema, len(s.AnyOf))
		for i, alt := range s.AnyOf {
			c.AnyOf[i] = alt.strict()
		}
	}
	if c.Type != "object" {
		return &c
	}
	c.AdditionalProperties = new(false)
	c.Properties = make(map[string]*Schema, len(s.Properties))
	c.Required = slices.Sorted(maps.Keys(s.Properties))
	for name, ps := range s.Properties {
		ps = ps.strict()
		if !slices.Contains(s.Required, name) {
			ps = nullable(ps)
		}
		c.Properties[name] = ps
	}
	return &c
}

// nullable returns a schema that also accepts null. The title and
// description stay on the outer schema, where models look for them.
func nullable(s *Schema) *Schema {
	switch {
	case s == nil || s.Type == "null":
		return s
	case s.Type == "":
		if len(s.AnyOf) == 0 || slices.ContainsFunc(s.AnyOf, func(alt *Schema) bool { return alt != nil && alt.Type == "null" }) {
			return s
		}
		c := *s
		c.AnyOf = append(slices.Clone(s.AnyOf), &Schema{Type: "null"})
		return &c
	}
	inner := *s
	inner.Title, inner.Description = "", ""
	return &Schema{Title: s.Title, Description: s.Description, AnyOf: []*Schema{&inner, {Type: "null"}}}
}

// SchemaOf derives an object schema from a Go struct type.
//
// Properties are the fields encoding/json decodes, under the same names:
// json tags rename or omit fields, the exported fields of embedded structs
// are promoted, and outer fields shadow embedded ones. Fields promoted
// through an embedded pointer to an unexported struct type are left out,
// since encoding/json cannot allocate that struct when decoding. Fields with
// the ",string" option are strings, and byte slices, but not byte arrays,
// are base64 strings; arrays of length N require exactly N items. Fields
// without omitempty or omitzero that are not pointers are required.
//
// The jsonschema struct tag describes a field in one of the two forms
// common in the Go ecosystem. A tag that starts with one of the keywords
// or flags below uses the keyword form of github.com/invopop/jsonschema: comma-separated
// key=value pairs and flags. A comma inside a value is escaped with a
// backslash, which the tag literal doubles:
//
//	jsonschema:"title=Unit,description=Unit\\, as a word,enum=celsius,enum=fahrenheit,default=celsius"
//
// Any other tag is the field's description, as in
// github.com/google/jsonschema-go, which the official MCP Go SDK and ADK Go
// use; commas need no escaping there:
//
//	jsonschema:"City name, e.g. Tokyo"
//
// The keywords are title, description, default, enum, format, pattern,
// minimum, maximum, exclusiveMinimum, exclusiveMaximum, minLength,
// maxLength, minItems and maxItems. enum may repeat, and so may default for
// slices and arrays. On a slice or array, enum, format, pattern and the
// numeric and length limits apply to its elements; minItems, maxItems and
// default to the slice itself. Values are parsed as the type of the field
// or element. The flag required makes a field required and nullable lets
// it be null; required=false makes a field optional. Unknown keywords are
// ignored, and a tag of "-" leaves the field out of the schema.
func SchemaOf[T any]() *Schema {
	return Reflect(reflect.TypeFor[T]())
}

// Reflect derives a schema from a Go type.
func Reflect(t reflect.Type) *Schema {
	return reflectType(t, map[reflect.Type]bool{})
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

func reflectType(t reflect.Type, seen map[reflect.Type]bool) *Schema {
	if t == nil {
		return &Schema{Type: "object", Properties: map[string]*Schema{}}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return &Schema{Type: "string", Format: "date-time"}
	case rawMessageType:
		return &Schema{}
	}
	switch t.Kind() {
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.Slice, reflect.Array:
		// encoding/json encodes byte slices, but not byte arrays, as
		// base64 strings.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		s := &Schema{Type: "array", Items: reflectType(t.Elem(), seen)}
		if t.Kind() == reflect.Array {
			n := t.Len()
			s.MinItems, s.MaxItems = &n, &n
		}
		return s
	case reflect.Map:
		return &Schema{Type: "object"}
	case reflect.Struct:
		if seen[t] {
			return &Schema{Type: "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		s := &Schema{Type: "object", Properties: map[string]*Schema{}}
		addFields(s, t, seen)
		return s
	}
	// Interfaces and other kinds accept any value.
	return &Schema{}
}

func addFields(s *Schema, t reflect.Type, seen map[reflect.Type]bool) {
	for _, f := range jsonFields(t) {
		tag := f.Tag.Get("jsonschema")
		if tag == "-" {
			continue
		}
		fs := reflectType(f.Type, seen)
		if f.quoted {
			// The ",string" option encodes the value inside a JSON string.
			fs = &Schema{Type: "string"}
		}
		required := !f.hasOption("omitempty") && !f.hasOption("omitzero") && f.Type.Kind() != reflect.Pointer
		if parts, ok := tagKeywords(tag); ok {
			fs, required = applyKeywords(fs, parts, required)
		} else if tag != "" {
			fs.Description = tag
		}
		s.Properties[f.name] = fs
		if required {
			s.Required = append(s.Required, f.name)
		}
	}
}

// tagKeywords splits a jsonschema tag in the keyword form into its parts.
// It reports false for a tag that does not start with a keyword or flag,
// which is a description.
func tagKeywords(tag string) ([]string, bool) {
	if tag == "" {
		return nil, false
	}
	parts := splitTag(tag)
	key, _, hasValue := strings.Cut(parts[0], "=")
	key = strings.TrimSpace(key)
	if hasValue && !schemaKeywords[key] || !hasValue && key != "required" && key != "nullable" {
		return nil, false
	}
	return parts, true
}

// schemaKeywords are the keys of the keyword form of jsonschema tags.
var schemaKeywords = map[string]bool{
	"title": true, "description": true, "default": true, "enum": true,
	"format": true, "pattern": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
	"required": true, "nullable": true,
}

// splitTag splits a tag at the commas not escaped with a backslash and
// unescapes the others, as github.com/invopop/jsonschema does.
func splitTag(tag string) []string {
	var parts []string
	for part := range strings.SplitSeq(tag, ",") {
		if n := len(parts); n > 0 && strings.HasSuffix(parts[n-1], `\`) {
			parts[n-1] = strings.TrimSuffix(parts[n-1], `\`) + "," + part
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

// applyKeywords applies the parts of a jsonschema tag in the keyword form
// to fs. It returns the schema, which nullable replaces, and whether the
// field is required.
func applyKeywords(fs *Schema, parts []string, required bool) (*Schema, bool) {
	// The elements of slices and arrays take the value constraints.
	elem := fs
	if fs.Type == "array" && fs.Items != nil {
		elem = fs.Items
	}
	var orNull bool
	var defaults []any
	for _, part := range parts {
		key, value, hasValue := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !hasValue {
			switch key {
			case "required":
				required = true
			case "nullable":
				orNull = true
			}
			continue
		}
		switch key {
		case "title":
			fs.Title = value
		case "description":
			fs.Description = value
		case "default":
			if elem != fs {
				defaults = append(defaults, parseScalar(value, elem.Type))
			} else {
				fs.Default = parseScalar(value, fs.Type)
			}
		case "enum":
			elem.Enum = append(elem.Enum, parseScalar(value, elem.Type))
		case "format":
			elem.Format = value
		case "pattern":
			elem.Pattern = value
		case "minimum":
			elem.Minimum = parseFloat(value, elem.Minimum)
		case "maximum":
			elem.Maximum = parseFloat(value, elem.Maximum)
		case "exclusiveMinimum":
			elem.ExclusiveMinimum = parseFloat(value, elem.ExclusiveMinimum)
		case "exclusiveMaximum":
			elem.ExclusiveMaximum = parseFloat(value, elem.ExclusiveMaximum)
		case "minLength":
			elem.MinLength = parseInt(value, elem.MinLength)
		case "maxLength":
			elem.MaxLength = parseInt(value, elem.MaxLength)
		case "minItems":
			fs.MinItems = parseInt(value, fs.MinItems)
		case "maxItems":
			fs.MaxItems = parseInt(value, fs.MaxItems)
		case "required":
			if b, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				required = b
			}
		case "nullable":
			if b, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				orNull = b
			}
		}
	}
	if defaults != nil {
		fs.Default = defaults
	}
	if orNull {
		fs = nullable(fs)
	}
	return fs, required
}

// parseFloat returns value as a number, or old if it is not one.
func parseFloat(value string, old *float64) *float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
		return &f
	}
	return old
}

// parseInt returns value as a non-negative integer, or old if it is not
// one.
func parseInt(value string, old *int) *int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
		return &n
	}
	return old
}

// jsonField is a struct field as encoding/json sees it.
type jsonField struct {
	reflect.StructField
	name    string
	tagged  bool  // the name comes from the json tag
	path    []int // index sequence from the outer struct
	options string
	quoted  bool // the ",string" option applies
	// unsettable fields are promoted through an embedded pointer to an
	// unexported struct, which encoding/json cannot allocate when decoding.
	unsettable bool
}

func (f *jsonField) hasOption(opt string) bool {
	return slices.Contains(strings.Split(f.options, ","), opt)
}

// jsonFields returns the fields encoding/json encodes for struct type t, in
// encoding order. It applies the same rules: the exported fields of
// embedded structs, exported or not, are promoted unless the embedded field
// is named by a tag; of several fields with one name, the shallowest wins,
// then the one named by a tag, and remaining ties drop them all. Fields
// that encoding/json encodes but cannot decode, those promoted through an
// embedded pointer to an unexported struct, take part in these rules but
// are left out of the result.
func jsonFields(t reflect.Type) []jsonField {
	type embedded struct {
		typ        reflect.Type
		path       []int
		unsettable bool
	}
	var fields []jsonField
	current, next := []embedded{}, []embedded{{typ: t}}
	// count holds how often each struct of the current depth is embedded
	// there, nextCount the same for the next depth.
	var count map[reflect.Type]int
	nextCount := map[reflect.Type]int{}
	visited := map[reflect.Type]bool{}
	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, e := range current {
			if visited[e.typ] {
				continue
			}
			visited[e.typ] = true
			for sf := range e.typ.Fields() {
				if sf.Anonymous {
					ft := sf.Type
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if !sf.IsExported() && ft.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if !validJSONName(name) {
					name = ""
				}
				path := append(slices.Clone(e.path), sf.Index...)
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if name == "" && sf.Anonymous && ft.Kind() == reflect.Struct {
					nextCount[ft]++
					if nextCount[ft] == 1 {
						unsettable := e.unsettable || !sf.IsExported() && sf.Type.Kind() == reflect.Pointer
						next = append(next, embedded{typ: ft, path: path, unsettable: unsettable})
					}
					continue
				}
				f := jsonField{StructField: sf, name: name, tagged: name != "", path: path, options: options, unsettable: e.unsettable}
				if f.name == "" {
					f.name = sf.Name
				}
				if f.hasOption("string") {
					switch ft.Kind() {
					case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
						reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
						f.quoted = true
					}
				}
				fields = append(fields, f)
				if count[e.typ] > 1 {
					// The struct is embedded more than once at this depth, so
					// its fields conflict with themselves.
					fields = append(fields, f)
				}
			}
		}
	}
	slices.SortFunc(fields, func(a, b jsonField) int {
		return cmp.Or(strings.Compare(a.name, b.name), cmp.Compare(len(a.path), len(b.path)),
			boolOrder(a.tagged, b.tagged), slices.Compare(a.path, b.path))
	})
	out := fields[:0]
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		// Sorted by depth, then tagged first: the first field dominates
		// unless the second ties with it.
		dominant := j-i == 1 || len(fields[i].path) != len(fields[i+1].path) || fields[i].tagged != fields[i+1].tagged
		if dominant && !fields[i].unsettable {
			out = append(out, fields[i])
		}
		i = j
	}
	slices.SortFunc(out, func(a, b jsonField) int { return slices.Compare(a.path, b.path) })
	return out
}

// boolOrder orders true before false.
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}

// validJSONName reports whether encoding/json accepts name from a tag.
func validJSONName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func parseScalar(v, typ string) any {
	switch typ {
	case "integer":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case "number":
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	case "boolean":
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return v
}

// ValidationError describes arguments that do not satisfy a schema.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid arguments: " + strings.Join(e.Problems, "; ")
}

// Validate checks that raw JSON satisfies the schema. It supports the
// keywords represented by Schema.
//
// The JSON must also be an I-JSON message (RFC 7493): objects that repeat
// a member name and strings that are not valid UTF-8 are rejected, since
// parsers disagree on which duplicate wins and how to repair invalid text,
// and the arguments of a call are read by hooks, permission rules and
// remote servers as well as by the tool.
func (s *Schema) Validate(raw json.RawMessage) error {
	if s == nil {
		return nil
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	val, err := dec.ReadValue()
	if err != nil {
		return &ValidationError{Problems: []string{"arguments are not valid JSON: " + err.Error()}}
	}
	val = val.Clone()
	if _, err := dec.ReadValue(); !errors.Is(err, io.EOF) {
		return &ValidationError{Problems: []string{"arguments must contain exactly one JSON value"}}
	}
	var v any
	vd := json.NewDecoder(bytes.NewReader(val))
	vd.UseNumber()
	if err := vd.Decode(&v); err != nil {
		return &ValidationError{Problems: []string{"arguments are not valid JSON: " + err.Error()}}
	}
	var problems []string
	s.validate("$", v, &problems)
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func (s *Schema) validate(path string, v any, problems *[]string) {
	if s == nil {
		return
	}
	if len(s.AnyOf) > 0 {
		matched := false
		for _, alt := range s.AnyOf {
			var p []string
			alt.validate(path, v, &p)
			if len(p) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			*problems = append(*problems, path+": does not match any allowed schema")
			return
		}
	}
	if len(s.Enum) > 0 && !inEnum(s.Enum, v) {
		*problems = append(*problems, fmt.Sprintf("%s: value must be one of %v", path, s.Enum))
	}
	if v == nil {
		if s.Type != "" && s.Type != "null" {
			*problems = append(*problems, fmt.Sprintf("%s: expected %s, got null", path, s.Type))
		}
		return
	}
	if !matchesType(s.Type, v) {
		*problems = append(*problems, fmt.Sprintf("%s: expected %s, got %s", path, s.Type, jsonType(v)))
		return
	}
	switch val := v.(type) {
	case map[string]any:
		for _, r := range s.Required {
			if _, ok := val[r]; !ok {
				*problems = append(*problems, fmt.Sprintf("%s: missing required property %q", path, r))
			}
		}
		for _, k := range slices.Sorted(maps.Keys(val)) {
			ps, ok := s.Properties[k]
			if !ok {
				// encoding/json matches field names ignoring case, so a
				// tool decoding the arguments could take this key for a
				// declared property that hooks and permission rules,
				// which read keys exactly, never saw.
				if name, found := foldedProperty(s.Properties, k); found {
					*problems = append(*problems, fmt.Sprintf("%s: property %q differs from %q only in case", path, k, name))
					continue
				}
				if s.AdditionalProperties != nil && !*s.AdditionalProperties {
					*problems = append(*problems, fmt.Sprintf("%s: unexpected property %q", path, k))
				}
				continue
			}
			// Models often send null for optional properties they leave
			// unset, and strict schemas make every optional property
			// nullable. Treat such a null as an absent property.
			if val[k] == nil && !slices.Contains(s.Required, k) {
				continue
			}
			ps.validate(path+"."+k, val[k], problems)
		}
	case []any:
		if s.MinItems != nil && len(val) < *s.MinItems {
			*problems = append(*problems, fmt.Sprintf("%s: expected at least %d items", path, *s.MinItems))
		}
		if s.MaxItems != nil && len(val) > *s.MaxItems {
			*problems = append(*problems, fmt.Sprintf("%s: expected at most %d items", path, *s.MaxItems))
		}
		for i, item := range val {
			s.Items.validate(fmt.Sprintf("%s[%d]", path, i), item, problems)
		}
	case string:
		if s.Pattern != "" {
			matched, err := regexp.MatchString(s.Pattern, val)
			if err != nil {
				*problems = append(*problems, fmt.Sprintf("%s: invalid pattern: %v", path, err))
			} else if !matched {
				*problems = append(*problems, fmt.Sprintf("%s: must match pattern %q", path, s.Pattern))
			}
		}
		n := len([]rune(val))
		if s.MinLength != nil && n < *s.MinLength {
			*problems = append(*problems, fmt.Sprintf("%s: expected at least %d characters", path, *s.MinLength))
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			*problems = append(*problems, fmt.Sprintf("%s: expected at most %d characters", path, *s.MaxLength))
		}
	case json.Number:
		f, _ := val.Float64()
		if s.Minimum != nil && f < *s.Minimum {
			*problems = append(*problems, fmt.Sprintf("%s: must be >= %v", path, *s.Minimum))
		}
		if s.Maximum != nil && f > *s.Maximum {
			*problems = append(*problems, fmt.Sprintf("%s: must be <= %v", path, *s.Maximum))
		}
		if s.ExclusiveMinimum != nil && f <= *s.ExclusiveMinimum {
			*problems = append(*problems, fmt.Sprintf("%s: must be > %v", path, *s.ExclusiveMinimum))
		}
		if s.ExclusiveMaximum != nil && f >= *s.ExclusiveMaximum {
			*problems = append(*problems, fmt.Sprintf("%s: must be < %v", path, *s.ExclusiveMaximum))
		}
	}
}

func matchesType(typ string, v any) bool {
	switch typ {
	case "":
		return true
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		if _, err := n.Int64(); err == nil {
			return true
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	case "null":
		return v == nil
	}
	return true
}

func jsonType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	}
	return "null"
}

func inEnum(enum []any, v any) bool {
	return slices.ContainsFunc(enum, func(e any) bool {
		raw, err := json.Marshal(e)
		if err != nil {
			return false
		}
		var normalized any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&normalized); err != nil {
			return false
		}
		return equalJSON(normalized, v)
	})
}

func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, xok := new(big.Rat).SetString(string(a))
		y, yok := new(big.Rat).SetString(string(b))
		return xok && yok && x.Cmp(y) == 0
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !equalJSON(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			other, exists := b[k]
			if !exists || !equalJSON(v, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// foldedProperty returns the declared property that k equals under
// Unicode case folding, as encoding/json compares field names.
func foldedProperty(props map[string]*Schema, k string) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(props)) {
		if strings.EqualFold(name, k) {
			return name, true
		}
	}
	return "", false
}

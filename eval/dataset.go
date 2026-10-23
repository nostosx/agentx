package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Format is a dataset file format.
type Format string

const (
	// FormatJSON is an object with the dataset's "name" and its "cases".
	FormatJSON Format = "json"
	// FormatJSONL holds one case per line, as JSON Lines.
	FormatJSONL Format = "jsonl"
)

// FormatOf returns the format of a dataset file by its extension:
// FormatJSONL for .jsonl and .ndjson, FormatJSON otherwise.
func FormatOf(path string) Format {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return FormatJSONL
	}
	return FormatJSON
}

// caseKeys are the fields of a case in a file. Evaluators are code and
// are not stored; ToolTrajectory reads expected tool calls from metadata.
var caseKeys = []string{"name", "input", "expected", "metadata"}

// LoadDataset reads a dataset file in the format FormatOf reports. A
// dataset without a name is named after the file, without its extension.
// Unknown fields of cases, such as a misspelled "expected", are errors.
//
// A JSON file looks like this; JSON Lines files hold one case object per
// line:
//
//	{
//	  "name": "capitals",
//	  "cases": [
//	    {"name": "france", "input": "Capital of France?", "expected": "Paris",
//	     "metadata": {"difficulty": "easy"}}
//	  ]
//	}
func LoadDataset[In, Out any](path string) (*Dataset[In, Out], error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, err := ReadDataset[In, Out](f, FormatOf(path))
	if err != nil {
		return nil, fmt.Errorf("eval: dataset %s: %w", path, err)
	}
	if d.Name == "" {
		d.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return d, nil
}

// ReadDataset reads a dataset in format f.
func ReadDataset[In, Out any](r io.Reader, f Format) (*Dataset[In, Out], error) {
	d := &Dataset[In, Out]{}
	switch f {
	case FormatJSON:
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, err
		}
		if err := checkKeys(top, "name", "cases", "$schema"); err != nil {
			return nil, err
		}
		if raw, ok := top["name"]; ok {
			if err := json.Unmarshal(raw, &d.Name); err != nil {
				return nil, fmt.Errorf("name: %w", err)
			}
		}
		var cases []json.RawMessage
		if raw, ok := top["cases"]; ok {
			if err := json.Unmarshal(raw, &cases); err != nil {
				return nil, fmt.Errorf("cases: %w", err)
			}
		}
		for i, raw := range cases {
			c, err := decodeCase[In, Out](raw)
			if err != nil {
				return nil, fmt.Errorf("case %d: %w", i+1, err)
			}
			d.Cases = append(d.Cases, c)
		}
	case FormatJSONL:
		br := bufio.NewReader(r)
		for line := 1; ; line++ {
			b, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(b)) > 0 {
				c, cerr := decodeCase[In, Out](b)
				if cerr != nil {
					return nil, fmt.Errorf("line %d: %w", line, cerr)
				}
				d.Cases = append(d.Cases, c)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown dataset format %q", f)
	}
	return d, nil
}

func checkKeys(m map[string]json.RawMessage, allowed ...string) error {
	for k := range m {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("unknown field %q; want %s", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func decodeCase[In, Out any](raw json.RawMessage) (Case[In, Out], error) {
	var c Case[In, Out]
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return c, err
	}
	if err := checkKeys(m, caseKeys...); err != nil {
		return c, err
	}
	if _, ok := m["input"]; !ok {
		return c, errors.New(`missing field "input"`)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}

// Save writes the dataset to a file in the format FormatOf reports.
// Evaluators are not saved.
func (d *Dataset[In, Out]) Save(path string) error {
	var b bytes.Buffer
	if err := d.Write(&b, FormatOf(path)); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// Write writes the dataset in format f: indented JSON, or one compact
// case per line for FormatJSONL, which has no dataset name.
func (d *Dataset[In, Out]) Write(w io.Writer, f Format) error {
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		cases := d.Cases
		if cases == nil {
			cases = []Case[In, Out]{}
		}
		return enc.Encode(struct {
			Name  string          `json:"name,omitempty"`
			Cases []Case[In, Out] `json:"cases"`
		}{d.Name, cases})
	case FormatJSONL:
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for i := range d.Cases {
			if err := enc.Encode(&d.Cases[i]); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("eval: unknown dataset format %q", f)
}

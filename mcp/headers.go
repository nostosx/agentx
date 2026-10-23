package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// paramHeadersKey carries the Mcp-Param headers of a tool call from the
// client to the HTTP transport.
type paramHeadersKey struct{}

// maxSafeInteger is the largest integer JavaScript represents exactly.
const maxSafeInteger = 1<<53 - 1

// paramHeader is a tool parameter mirrored into an Mcp-Param header.
type paramHeader struct {
	name string
	path []string
}

// paramHeaders returns the x-mcp-header annotations of a tool's input
// schema. It fails if any annotation is invalid, in which case the tool must
// not be used over Streamable HTTP.
func paramHeaders(schema json.RawMessage) ([]paramHeader, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("decode input schema: %w", err)
	}
	var out []paramHeader
	seen := map[string]bool{}
	// reachable reports whether node is reached from the root through
	// properties keys only.
	var walk func(node any, path []string, reachable bool) error
	walk = func(node any, path []string, reachable bool) error {
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				if err := walk(v, path, false); err != nil {
					return err
				}
			}
		case map[string]any:
			if v, ok := n["x-mcp-header"]; ok {
				name, _ := v.(string)
				switch {
				case !isToken(name):
					return fmt.Errorf("invalid x-mcp-header %v", v)
				case !reachable || len(path) == 0:
					return fmt.Errorf("x-mcp-header %q is not on a property reachable through properties", name)
				case !headerType(n["type"]):
					return fmt.Errorf("x-mcp-header %q must annotate an integer, string or boolean", name)
				case seen[strings.ToLower(name)]:
					return fmt.Errorf("duplicate x-mcp-header %q", name)
				}
				seen[strings.ToLower(name)] = true
				out = append(out, paramHeader{name: name, path: path})
			}
			for k, v := range n {
				switch k {
				case "properties":
					props, _ := v.(map[string]any)
					for prop, sub := range props {
						if err := walk(sub, append(slices.Clone(path), prop), reachable); err != nil {
							return err
						}
					}
				case "x-mcp-header", "const", "default", "enum", "examples":
					// Values, not schemas.
				default:
					if err := walk(v, path, false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(root, nil, true); err != nil {
		return nil, err
	}
	return out, nil
}

// headerType reports whether a schema type is one x-mcp-header may annotate.
func headerType(t any) bool {
	allowed := func(s any) bool { return s == "integer" || s == "string" || s == "boolean" }
	switch t := t.(type) {
	case string:
		return allowed(t)
	case []any:
		var types []any
		for _, s := range t {
			if s != "null" {
				types = append(types, s)
			}
		}
		return len(types) == 1 && allowed(types[0])
	}
	return false
}

// isToken reports whether s is an HTTP field-name token (RFC 9110).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		b := s[i]
		ok := b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
			strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
		if !ok {
			return false
		}
	}
	return true
}

// paramValues returns the Mcp-Param headers for a tool call's arguments.
// Parameters without a primitive value are omitted.
func paramValues(headers []paramHeader, args json.RawMessage) http.Header {
	d := json.NewDecoder(bytes.NewReader(args))
	d.UseNumber()
	var root any
	if d.Decode(&root) != nil {
		return nil
	}
	h := http.Header{}
	for _, ph := range headers {
		v := root
		for _, k := range ph.path {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		s, err := headerString(v)
		if err != nil {
			continue
		}
		h.Set("Mcp-Param-"+ph.name, headerValue(s))
	}
	return h
}

func headerString(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		f, err := v.Float64()
		if err != nil || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
			return "", errors.New("not a safe integer")
		}
		return strconv.FormatInt(int64(f), 10), nil
	}
	return "", errors.New("not a primitive value")
}

// headerValue encodes s for an Mcp-Name or Mcp-Param header, using the
// =?base64?...?= form when s is not a plain ASCII header value.
func headerValue(s string) string {
	plain := strings.Trim(s, " \t") == s &&
		(!strings.HasPrefix(s, "=?base64?") || !strings.HasSuffix(s, "?="))
	for i := 0; plain && i < len(s); i++ {
		plain = s[i] == '\t' || s[i] >= 0x20 && s[i] <= 0x7e
	}
	if plain {
		return s
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

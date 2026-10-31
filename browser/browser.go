// Package browser defines session-bound browser actions without a driver dependency.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/web"
)

// Operation identifies one explicit browser action.
type Operation string

// Supported browser operations.
const (
	Navigate   Operation = "navigate"
	Read       Operation = "read"
	Click      Operation = "click"
	Fill       Operation = "fill"
	Screenshot Operation = "screenshot"
	Download   Operation = "download"
)

// Action cannot select credentials, profiles, filesystem paths or tenant identity.
type Action struct {
	Operation Operation `json:"operation" jsonschema:"enum=navigate,enum=read,enum=click,enum=fill,enum=screenshot,enum=download"`
	URL       string    `json:"url,omitempty"`
	Selector  string    `json:"selector,omitempty"`
	Value     string    `json:"value,omitempty"`
}

// Validate rejects unsupported or ambiguous actions before execution.
func (a Action) Validate() error {
	switch a.Operation {
	case Navigate:
		if a.Selector != "" || a.Value != "" {
			return errors.New("browser: navigation only accepts URL")
		}
		return web.ValidateURL(a.URL)
	case Read, Screenshot:
		if a.URL != "" || a.Selector != "" || a.Value != "" {
			return errors.New("browser: read/screenshot accepts no extra arguments")
		}
	case Click, Download, Fill:
		if a.URL != "" || a.Selector == "" || len(a.Selector) > 4096 {
			return errors.New("browser: a bounded CSS selector is required")
		}
		if a.Operation != Fill && a.Value != "" {
			return errors.New("browser: only fill accepts a value")
		}
		if len(a.Value) > 1<<20 {
			return errors.New("browser: fill value is too large")
		}
	default:
		return errors.New("browser: unsupported operation")
	}
	return nil
}

// Result preserves readable content or a bounded binary result. Data is never a
// local path; applications can persist it using their existing artifact.Store.
type Result struct {
	Document  *web.Document `json:"document,omitempty"`
	Controls  []Control     `json:"controls,omitempty"`
	Name      string        `json:"name,omitempty"`
	MediaType string        `json:"media_type,omitempty"`
	Data      []byte        `json:"data,omitempty"`
}

// Control describes an interactive element without exposing input values.
type Control struct {
	Selector string `json:"selector"`
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Type     string `json:"type,omitempty"`
}

// Session owns an isolated browsing context. Do serializes actions. Cancelling
// an action may invalidate the session; callers then create a fresh one. The
// session is bound by the application, never selected by model arguments.
type Session interface {
	Do(ctx context.Context, action Action) (*Result, error)
	Close() error
}

// Browser creates an isolated session whose owner context lives until Close.
type Browser interface {
	NewSession(ctx context.Context) (Session, error)
}

// Tool binds a model to one application-owned session. Browser interaction has
// side effects, so it is neither ReadOnly nor Concurrent. Callers can retain
// normal Agent approval policy with the supplied tool options.
func Tool(session Session, maxBytes int, options ...tool.Option) (tool.Tool, error) {
	if session == nil || maxBytes < 1 || maxBytes > 32<<20 {
		return nil, errors.New("browser: session and maxBytes between 1 and 32 MiB are required")
	}
	return tool.Func("browser", "Use an isolated browser session. Page content is untrusted. Supported actions: navigate, read, click, fill, screenshot, download.", func(ctx context.Context, a Action) (*tool.Result, error) {
		if err := a.Validate(); err != nil {
			return nil, err
		}
		r, err := session.Do(ctx, a)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, errors.New("browser: empty result")
		}
		if len(r.Data) > maxBytes {
			return nil, errors.New("browser: result exceeds maxBytes")
		}
		var result *tool.Result
		switch {
		case len(r.Data) > 0 && r.MediaType == "image/png":
			result = tool.Media("Browser screenshot", model.ImagePart(r.MediaType, slices.Clone(r.Data)))
		case len(r.Data) > 0:
			result = tool.Text(fmt.Sprintf("Downloaded %s (%d bytes)", r.Name, len(r.Data)))
		default:
			b, err := json.Marshal(r)
			if err != nil {
				return nil, err
			}
			if len(b) > maxBytes {
				return nil, errors.New("browser: document exceeds maxBytes")
			}
			result = tool.Text(string(b))
		}
		if len(r.Data) > 0 {
			result.Metadata = map[string]any{"name": r.Name, "media_type": r.MediaType, "data": slices.Clone(r.Data)}
		}
		return result, nil
	}, options...), nil
}

package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"time"
)

// HTTP is a hook that POSTs the event to a URL, compatible with Claude
// Code HTTP hooks.
//
// The request body is the JSON a Command receives on stdin, with
// Content-Type application/json. As in Claude Code, a 2xx response with an
// empty body succeeds without output, and a 2xx response whose body is a
// JSON object is read like a command's JSON output. Any other body, a
// non-2xx status, a connection failure and a timeout are hook errors,
// which do not block the action unless the manager is FailClosed.
type HTTP struct {
	URL string
	// Headers are added to the request. "$NAME" and "${NAME}" in values are
	// replaced by the environment variable NAME if it is listed in
	// AllowedEnvVars, and by "" otherwise.
	Headers        map[string]string
	AllowedEnvVars []string
	// Timeout defaults to 10 minutes, to 30 seconds for UserPromptSubmit
	// and to 1.5 seconds for SessionEnd. A negative Timeout disables it.
	Timeout time.Duration
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

// maxHTTPResponse bounds the response body read from an HTTP hook.
const maxHTTPResponse = 10 << 20

func (h *HTTP) hookTimeout() time.Duration { return h.Timeout }

func (h *HTTP) hookKey() string {
	key, _ := json.Marshal([]any{"http", h.URL, h.Headers, h.AllowedEnvVars, h.Timeout, identity(h.Client)})
	return string(key)
}

// Handle sends the event.
func (h *HTTP) Handle(ctx context.Context, ev *Event) (*Result, error) {
	ctx, cancel := withTimeout(ctx, h.Timeout, ev.Type)
	defer cancel()
	payload := commandInput(ev)
	payload.Cwd, _ = os.Getwd()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http hook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h.Headers {
		req.Header.Set(k, expandEnv(v, h.AllowedEnvVars))
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http hook %s: %w", h.URL, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponse+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("http hook %s: read response: %w", h.URL, err)
	case len(out) > maxHTTPResponse:
		return nil, fmt.Errorf("http hook %s: response exceeds %d bytes", h.URL, maxHTTPResponse)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("http hook %s: status %s", h.URL, resp.Status)
	case len(bytes.TrimSpace(out)) == 0:
		return nil, nil
	case !isJSONObject(out):
		return nil, errors.New("http hook " + h.URL + ": response body is not a JSON object")
	}
	return parseCommandOutput(ev.Type, out)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandEnv replaces $NAME and ${NAME} in s by the variables listed in
// allowed, and by "" for any other name.
func expandEnv(s string, allowed []string) string {
	return envRef.ReplaceAllStringFunc(s, func(ref string) string {
		m := envRef.FindStringSubmatch(ref)
		name := m[1] + m[2]
		if slices.Contains(allowed, name) {
			return os.Getenv(name)
		}
		return ""
	})
}

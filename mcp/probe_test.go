package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// TestSlowStdioServers checks the probe of servers that answer
// server/discover only after the probe timeout, when initialize is sent
// as well.
func TestSlowStdioServers(t *testing.T) {
	const slow = 150 * time.Millisecond
	// probeID is the ID of the probe the server answers after rejecting
	// initialize.
	var probeID json.RawMessage
	for name, tc := range map[string]struct {
		// serve answers the probe, initialize and tools/list.
		serve  func(m wireMessage, write func(string), discovers int)
		modern bool
	}{
		"modern server answers the probe late": {modern: true, serve: func(m wireMessage, write func(string), _ int) {
			switch m.Method {
			case "server/discover":
				time.Sleep(slow)
				write(result(m.ID, discoverResult))
			case "initialize":
				write(rpcError(m.ID, CodeMethodNotFound, "Method not found"))
			}
		}},
		// The initialize error arrives first: it does not identify the
		// server, so the client waits for the late probe answer.
		"modern server rejects initialize before answering the probe": {modern: true, serve: func(m wireMessage, write func(string), _ int) {
			switch m.Method {
			case "server/discover":
				probeID = m.ID
			case "initialize":
				write(rpcError(m.ID, CodeMethodNotFound, "Method not found"))
				time.Sleep(slow)
				write(result(probeID, discoverResult))
			}
		}},
		"modern server rejects initialize naming its versions": {modern: true, serve: func(m wireMessage, write func(string), discovers int) {
			switch m.Method {
			case "server/discover":
				if discovers > 1 { // the first one is lost while starting
					write(result(m.ID, discoverResult))
				}
			case "initialize":
				write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32022,"message":"Unsupported protocol version",`+
					`"data":{"supported":[%q]}}}`, m.ID, ProtocolVersion))
			}
		}},
		"legacy server rejects the probe late": {serve: func(m wireMessage, write func(string), _ int) {
			switch m.Method {
			case "server/discover":
				time.Sleep(slow)
				write(rpcError(m.ID, CodeInvalidRequest, "not initialized"))
			case "initialize":
				write(initResult(m.ID, "2025-06-18", `{"tools":{}}`))
			}
		}},
		"dual-era server answers initialize first": {serve: func(m wireMessage, write func(string), _ int) {
			switch m.Method {
			case "server/discover":
				go func() {
					time.Sleep(2 * slow)
					write(result(m.ID, discoverResult))
				}()
			case "initialize":
				write(initResult(m.ID, "2025-06-18", `{"tools":{}}`))
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var methods []string
				discovers := 0
				tr := pipeServer(t, func(m wireMessage, write func(string)) {
					mu.Lock()
					methods = append(methods, m.Method)
					if m.Method == "server/discover" {
						discovers++
					}
					n := discovers
					mu.Unlock()
					if m.Method == "tools/list" {
						write(result(m.ID, `{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`))
						return
					}
					tc.serve(m, write, n)
				})
				c, err := Connect(t.Context(), tr, WithProbeTimeout(30*time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				want := "2025-06-18"
				if tc.modern {
					want = ProtocolVersion
				}
				if c.ProtocolVersion() != want {
					t.Errorf("ProtocolVersion = %q, want %q", c.ProtocolVersion(), want)
				}
				if tools, err := c.ListTools(t.Context()); err != nil || len(tools) != 1 {
					t.Fatalf("tools = %v, err = %v", tools, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if !slices.Contains(methods, "initialize") {
					t.Errorf("methods = %v: initialize was not sent after the timeout", methods)
				}
				if tc.modern == slices.Contains(methods, "notifications/initialized") {
					t.Errorf("methods = %v", methods)
				}
				// Let late answers of the server go out before the bubble ends.
				time.Sleep(2 * slow)
			})
		})
	}
}

// TestProbeTimeoutDisabled checks that without a probe timeout Connect
// waits for the answer to server/discover.
func TestProbeTimeoutDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var methods []string
		var mu sync.Mutex
		tr := pipeServer(t, func(m wireMessage, write func(string)) {
			mu.Lock()
			methods = append(methods, m.Method)
			mu.Unlock()
			if m.Method == "server/discover" {
				time.Sleep(100 * time.Millisecond)
				write(result(m.ID, discoverResult))
			}
		})
		c, err := Connect(t.Context(), tr, WithProbeTimeout(0))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		mu.Lock()
		defer mu.Unlock()
		if c.ProtocolVersion() != ProtocolVersion || !slices.Equal(methods, []string{"server/discover"}) {
			t.Errorf("version = %q, methods = %v", c.ProtocolVersion(), methods)
		}
	})
}

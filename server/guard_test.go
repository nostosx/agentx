package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

// With guard.HoldOutput an answer an output guardrail blocks never
// reaches a stream, in any protocol, and is not kept in the session.
func TestHeldOutputGuardrail(t *testing.T) {
	pii := guard.OutputGuardrail{Name: "pii", Check: func(_ context.Context, _ *loop.Run, out *model.Message) (guard.Result, error) {
		return guard.Result{Tripwire: strings.Contains(out.Text(), "4242"), Message: "card number"}, nil
	}}
	for _, protocol := range []Protocol{ProtocolAgentX, ProtocolVercel, ProtocolAGUI} {
		t.Run(string(protocol), func(t *testing.T) {
			f := newFixture(t, []modeltest.Step{modeltest.Reply("card 4242")},
				[]agent.Option{agent.WithOutputGuardrail(pii, guard.HoldOutput())}, WithProtocol(protocol))
			resp := f.do(http.MethodPost, "/sessions/s1/runs", "text/event-stream", RunRequest{Input: "card?"})
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "4242") {
				t.Fatalf("stream %s:\n%s", resp.Status, body)
			}
			if !strings.Contains(string(body), "blocked") {
				t.Fatalf("stream does not report the block:\n%s", body)
			}
			info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
			if n := len(info.Messages); n != 2 || info.Messages[1].Text() != "[response withheld by guardrail pii]" {
				t.Fatalf("session messages = %+v", info.Messages)
			}
		})
	}
}

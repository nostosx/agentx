package eval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// AgentTask returns a task that runs a on the input text, without history,
// and outputs the run's final answer, Result.Output. A run that ends
// without completing, for example at its turn limit, still produces an
// output; use Completed to require completion. opts configure each run,
// as they do for agent.Runner.
func AgentTask(a agent.Agent, opts ...agent.RunOption) Task[string, string] {
	return AgentTaskFunc(a, textInput(opts), func(res *loop.Result) (string, error) { return res.Output, nil })
}

// TypedAgentTask returns a task that runs a on the input text, without
// history, and decodes the final answer as Out, as agent.AskAs does.
// Configure a with agent.WithOutput[Out]. A run that did not complete, or
// whose answer does not decode, fails the case.
func TypedAgentTask[Out any](a agent.Agent, opts ...agent.RunOption) Task[string, Out] {
	return AgentTaskFunc(a, textInput(opts), (*loop.Result).DecodeAs[Out])
}

func textInput(opts []agent.RunOption) func(string) *agent.Input {
	return func(s string) *agent.Input {
		in := agent.Text(s)
		for _, opt := range opts {
			opt(in)
		}
		return in
	}
}

// AgentTaskFunc returns a task that runs a on the agent input that input
// builds and derives the output from the run's result with output. A
// failed run fails the case, with the partial result's usage reported.
func AgentTaskFunc[In, Out any](a agent.Agent, input func(In) *agent.Input, output func(*loop.Result) (Out, error)) Task[In, Out] {
	return func(ctx context.Context, v In) (Out, *loop.Result, error) {
		var zero Out
		res, err := loop.Collect(a.Run(ctx, input(v)), nil)
		if err != nil {
			return zero, res, err
		}
		out, err := output(res)
		return out, res, err
	}
}

// Scenario is the input of SimulateUser: what a simulated user wants from
// the agent, as in Google ADK's conversation scenarios
// (https://adk.dev/evaluate/user-sim/).
type Scenario struct {
	// Start is the user's first message.
	Start string `json:"start"`
	// Plan describes what the user wants to achieve and how to answer the
	// agent's questions, for example "Book a table for two at 7pm on Friday.
	// Give the name Ada when asked."
	Plan string `json:"plan"`
	// Persona, if set, describes how the user writes and behaves.
	Persona string `json:"persona,omitempty"`
}

// DefaultStopSignal is the reply with which the simulated user ends a
// conversation.
const DefaultStopSignal = "</finished>"

// DefaultMaxInvocations bounds the user messages of a simulated
// conversation, as ADK's max_allowed_invocations does.
const DefaultMaxInvocations = 20

// SimOption configures SimulateUser.
type SimOption func(*simConfig)

type simConfig struct {
	maxInvocations int
	stop           string
	runOpts        []agent.RunOption
}

// MaxInvocations bounds the user messages of a conversation, including the
// first; it defaults to DefaultMaxInvocations. The conversation ends after
// the agent answers the last one.
func MaxInvocations(n int) SimOption { return func(c *simConfig) { c.maxInvocations = n } }

// StopSignal sets the reply with which the simulated user ends the
// conversation; it defaults to DefaultStopSignal.
func StopSignal(s string) SimOption { return func(c *simConfig) { c.stop = s } }

// SimRunOptions configures each run of the agent.
func SimRunOptions(opts ...agent.RunOption) SimOption {
	return func(c *simConfig) { c.runOpts = append(c.runOpts, opts...) }
}

const simInstructions = `You play a user talking to an AI agent, to test it. Write as the user would, in the first person, one message at a time, and never act as the agent.

Your plan:
<plan>
%s
</plan>
%s
Follow the plan: answer the agent's questions with the details the plan gives, and do not invent goals beyond it. When the plan is complete, or the agent cannot help further, reply with exactly %s and nothing else.`

// SimulateUser returns a task that holds a multi-turn conversation between
// a and a user simulated by the model user, as Google ADK's user
// simulation does (https://adk.dev/evaluate/user-sim/). The user opens
// with Scenario.Start; then, after each answer of the agent, user writes
// the next message following Scenario.Plan, until it replies with the stop
// signal or the conversation reaches MaxInvocations.
//
// The output is the agent's last answer. The result is that of the last
// run, with the usage and turns of all runs and NewMessages holding the
// whole conversation, so evaluators such as ToolTrajectory and MaxTokens
// see all of it. The simulated user's own usage is not counted. A run
// that does not complete, for example one waiting for approval, ends the
// conversation.
func SimulateUser(a agent.Agent, user model.Model, opts ...SimOption) Task[Scenario, string] {
	cfg := &simConfig{maxInvocations: DefaultMaxInvocations, stop: DefaultStopSignal}
	for _, opt := range opts {
		opt(cfg)
	}
	return func(ctx context.Context, sc Scenario) (string, *loop.Result, error) {
		if strings.TrimSpace(sc.Start) == "" {
			return "", nil, errors.New("eval: scenario has no start message")
		}
		var (
			combined loop.Result
			history  []*model.Message
			msg      = sc.Start
		)
		for n := 1; ; n++ {
			in := &agent.Input{Message: model.UserText(msg), History: history}
			for _, opt := range cfg.runOpts {
				opt(in)
			}
			res, err := loop.Collect(a.Run(ctx, in), nil)
			if res != nil {
				usage, turns, added := combined.Usage, combined.Turns, combined.NewMessages
				combined = *res
				combined.Usage = usage
				combined.Usage.Add(res.Usage)
				combined.Turns = turns + res.Turns
				combined.NewMessages = slices.Concat(added, res.NewMessages)
			}
			if err != nil {
				return combined.Output, &combined, err
			}
			if res.Status != loop.StatusCompleted || n >= cfg.maxInvocations {
				break
			}
			history = res.Messages
			next, err := simulateTurn(ctx, user, cfg, sc, history)
			if err != nil {
				return combined.Output, &combined, err
			}
			if next == "" {
				break
			}
			msg = next
		}
		return combined.Output, &combined, nil
	}
}

// simulateTurn returns the simulated user's next message, or "" when the
// user stops.
func simulateTurn(ctx context.Context, user model.Model, cfg *simConfig, sc Scenario, history []*model.Message) (string, error) {
	persona := ""
	if sc.Persona != "" {
		persona = "\nYour persona:\n<persona>\n" + sc.Persona + "\n</persona>\n"
	}
	var b strings.Builder
	b.WriteString("The conversation so far:\n<conversation>\n")
	for _, m := range history {
		text := strings.TrimSpace(m.Text())
		if text == "" {
			continue
		}
		switch m.Role {
		case model.RoleUser:
			fmt.Fprintf(&b, "User: %s\n\n", text)
		case model.RoleAssistant:
			fmt.Fprintf(&b, "Agent: %s\n\n", text)
		}
	}
	fmt.Fprintf(&b, "</conversation>\n\nWrite the user's next message, or %s if the conversation is over.", cfg.stop)
	resp, err := user.Generate(ctx, &model.Request{
		System:   fmt.Sprintf(simInstructions, sc.Plan, persona, cfg.stop),
		Messages: []*model.Message{model.UserText(b.String())},
	})
	if err != nil {
		return "", fmt.Errorf("eval: simulated user: %w", err)
	}
	next := strings.TrimSpace(resp.Text())
	next = strings.TrimSpace(strings.TrimPrefix(next, "User:"))
	if next == "" || strings.Contains(next, cfg.stop) {
		return "", nil
	}
	return next, nil
}

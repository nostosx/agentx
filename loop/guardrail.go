package loop

import (
	"cmp"
	"fmt"

	"github.com/nostosx/agentx/model"
)

// Guardrail describes the guardrail whose tripwire ended a run, for
// Result.Guardrail. See the guard package.
type Guardrail struct {
	// Name names the guardrail.
	Name string `json:"name"`
	// Kind is where it ran: "input", "output", "tool_input" or
	// "tool_output".
	Kind string `json:"kind,omitempty"`
	// Message explains the trip, for example to show to the user.
	Message string `json:"message,omitempty"`
	// Info is any information the guardrail attached.
	Info any `json:"info,omitempty"`
}

// GuardrailError is returned by middleware, typically the guard package's,
// when a guardrail's tripwire triggers. Returned from a stage or from a
// model call (see ModelWrapper), it ends the run with StatusBlocked and
// Result.Guardrail set; Result.Output is then empty, so a withheld answer
// is not shown by accident. Returned from BeforeFinish, it withholds the
// final answer: the conversation, the run's result and what a Runner saves
// keep WithheldAnswer in its place, which keeps the conversation valid for
// providers. Its events were sent already unless an AnswerHolder held
// them.
type GuardrailError struct {
	Guardrail Guardrail
	// Usage is the usage of a model call the guardrail cut short or
	// rejected, which a ModelWrapper returning the error reports so the
	// run still counts it.
	Usage model.Usage
}

// WithheldAnswer returns the assistant message that replaces an answer
// the guardrail named name withheld.
func WithheldAnswer(name string) *model.Message {
	return model.AssistantText(fmt.Sprintf("[response withheld by guardrail %s]", name))
}

func (e *GuardrailError) Error() string {
	return fmt.Sprintf("guardrail %s tripped: %s", e.Guardrail.Name, cmp.Or(e.Guardrail.Message, "no reason given"))
}

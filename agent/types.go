package agent

import "github.com/nostosx/agentx/loop"

// Event is an event of an agent run, as Agent.Run and Runner.Run stream
// them; see loop.Event. The alias lets code that only runs agents use the
// agent package alone.
type Event = loop.Event

// Result is the outcome of an agent run, as loop.Collect returns it; see
// loop.Result.
type Result = loop.Result

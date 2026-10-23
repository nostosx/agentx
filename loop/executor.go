package loop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// ApprovalRequest asks for permission to run a tool call.
type ApprovalRequest struct {
	Call *model.ToolCall `json:"call"`
	// Reason is the explanation given by the hook that asked, if any.
	Reason string `json:"reason,omitempty"`
	// Agent names the agent whose call this is. Requests of sub-agents
	// reach the parent's interrupt with their own agent name.
	Agent string `json:"agent,omitempty"`
}

// Approval is the decision for an ApprovalRequest.
type Approval struct {
	Approved bool `json:"approved"`
	// Reason is shown to the model when the call is denied.
	Reason string `json:"reason,omitempty"`
	// UpdatedInput replaces the call arguments when approved.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	// Remember asks middleware that decides approvals, such as the
	// permission package's Policy, to approve later calls like this one
	// without asking again, for the rest of the run or of the session.
	// Middleware reads it with ToolPlan.Approval.
	Remember RememberScope `json:"remember,omitempty"`
	// RememberRules, if set, lists what to remember instead of what the
	// middleware derives from the call, for example permission rules such
	// as "Bash(npm test *)".
	RememberRules []string `json:"remember_rules,omitempty"`
}

// RememberScope is how long an approval is remembered; see
// Approval.Remember.
type RememberScope string

const (
	// RememberRun remembers an approval for the rest of the run.
	RememberRun RememberScope = "run"
	// RememberSession remembers an approval for the rest of the session:
	// it is kept in a run value whose key starts with SessionValuePrefix,
	// which agent.Runner carries to the session's later runs.
	RememberSession RememberScope = "session"
)

// Approve and Deny are convenience approvals.
var (
	Approve = Approval{Approved: true}
	Deny    = Approval{Reason: "The user denied this tool call."}
)

// Approver decides approval requests synchronously, for example by
// prompting a user in a terminal.
type Approver interface {
	Approve(ctx context.Context, req *ApprovalRequest) (Approval, error)
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, req *ApprovalRequest) (Approval, error)

// Approve calls f.
func (f ApproverFunc) Approve(ctx context.Context, req *ApprovalRequest) (Approval, error) {
	return f(ctx, req)
}

// Interrupt describes a run paused for approval. Resume it with a decision
// for each request, keyed by the request's call ID.
type Interrupt struct {
	RunID    string             `json:"run_id"`
	Requests []*ApprovalRequest `json:"requests"`
	// State is the state of the paused run. It lets ResumeState continue
	// the run without a Checkpointer, as sub-agents do.
	State *State `json:"state,omitempty"`
}

// plan runs BeforeTool middleware and determines which calls need approval.
func (l *execution) plan(ctx context.Context, calls []*model.ToolCall) ([]*PendingCall, error) {
	out := make([]*PendingCall, 0, len(calls))
	for _, c := range calls {
		tp := l.toolPlan(&PendingCall{Call: cloneCall(c)})
		original := slices.Clone(c.Arguments)
		for _, m := range l.r.stages.beforeTool {
			if err := m.BeforeTool(ctx, l.handle, tp); err != nil {
				return nil, err
			}
			if tp.decision == Denied {
				break
			}
		}
		p := &PendingCall{Call: tp.Call, ReadOnly: tp.Info.ReadOnly, Concurrent: tp.Info.Concurrent, Values: tp.values}
		if !sameJSON(p.Call.Arguments, original) {
			p.ModelArguments = original
		}
		needs := tp.Info.RequiresApproval
		if p.ModelArguments != nil {
			// Middleware rewrote the arguments, which may change whether
			// the call needs approval.
			if t, ok := l.r.cfg.Tools.Get(p.Call.Name); ok {
				needs = tool.NeedsApproval(t, p.Call.Arguments)
			}
		}
		switch tp.decision {
		case Denied:
			p.Denied = true
			p.Reason = cmp.Or(tp.reason, "This tool call was blocked.")
		case Asked:
			needs = true
			p.Reason = tp.reason
		case Allowed:
			needs = false
		}
		p.NeedsApproval = needs && tp.Known && !p.Denied
		out = append(out, p)
	}
	return out, nil
}

// toolPlan returns the plan of a pending call as seen by middleware. Its
// Info.RequiresApproval is that of the call; see tool.CallApprover.
func (l *execution) toolPlan(p *PendingCall) *ToolPlan {
	tp := &ToolPlan{Call: p.Call, values: p.Values, approval: p.Approval}
	if t, ok := l.r.cfg.Tools.Get(p.Call.Name); ok {
		tp.Info, tp.Known = t.Info(), true
		tp.Info.RequiresApproval = tool.NeedsApproval(t, p.Call.Arguments)
	}
	if p.Denied {
		tp.decision, tp.reason = Denied, p.Reason
	}
	return tp
}

// tools resolves approvals for the pending calls and executes them. It
// returns false when the run ended or was interrupted.
//
// approvals, when non-nil, holds the decisions of a resumed run. They
// answer the requests of the interrupt being resumed; requests raised
// later, by nested runs that ask again, go to the Approver or interrupt the
// run anew.
func (l *execution) tools(ctx context.Context, approvals map[string]Approval) bool {
	st := l.st
	for {
		requests, err := l.resolve(ctx, approvals)
		if !l.check(ctx, err) || l.stopped {
			return false
		}
		if len(requests) > 0 {
			l.interrupt(ctx, requests)
			return false
		}
		approvals = nil
		nested, err := l.execute(ctx, st.Pending)
		if !l.check(ctx, err) || l.stopped {
			return false
		}
		if !nested {
			break
		}
	}
	results := make([]*model.ToolResult, len(st.Pending))
	for i, p := range st.Pending {
		results[i] = p.Result
	}
	st.Pending = nil
	st.Messages = append(st.Messages, model.ToolResults(results...))
	return true
}

// resolve decides the outstanding approvals of the pending calls, their
// own and those of nested runs, from approvals, the Approver or neither. It
// returns the requests left for an interrupt.
func (l *execution) resolve(ctx context.Context, approvals map[string]Approval) ([]*ApprovalRequest, error) {
	cfg := &l.r.cfg
	var requests []*ApprovalRequest
	decide := func(req *ApprovalRequest) (Approval, bool) {
		switch {
		case approvals != nil:
			a, ok := approvals[req.Call.ID]
			if !ok {
				a = Approval{Reason: "No approval was given for this tool call."}
			}
			return a, true
		case cfg.Approver != nil:
			a, err := cfg.Approver.Approve(ctx, req)
			if err != nil {
				a = Approval{Reason: "Approval failed: " + err.Error()}
			}
			return a, true
		}
		requests = append(requests, req)
		return Approval{}, false
	}
	for _, p := range l.st.Pending {
		switch {
		case p.NeedsApproval:
			req := &ApprovalRequest{Call: cloneCall(p.Call), Reason: p.Reason, Agent: l.st.Agent}
			if a, ok := decide(req); ok {
				if p.apply(a) {
					if err := l.revalidate(ctx, p); err != nil {
						return nil, err
					}
				}
			}
		case p.Nested != nil && p.NestedApprovals == nil:
			decided := map[string]Approval{}
			for _, req := range p.Nested.Requests {
				if a, ok := decide(req); ok {
					decided[req.Call.ID] = a
				}
			}
			if len(decided) == len(p.Nested.Requests) {
				p.NestedApprovals = decided
			}
		}
	}
	return requests, nil
}

// revalidate checks an explicitly approved edit without repeating hooks or
// asking for the same approval again. Each checker sees a fresh copy so
// mutations cannot invalidate an earlier check or alter what executes.
func (l *execution) revalidate(ctx context.Context, p *PendingCall) error {
	for _, m := range l.r.stages.revalidateTool {
		tp := l.toolPlan(p)
		tp.Call = cloneCall(p.Call)
		if err := m.RevalidateTool(ctx, l.handle, tp); err != nil {
			return err
		}
		if tp.Call == nil || tp.Call.ID != p.Call.ID || tp.Call.Name != p.Call.Name || !bytes.Equal(tp.Call.Arguments, p.Call.Arguments) {
			p.Denied, p.Reason = true, "This tool call was blocked because a revalidator changed the approved call."
			p.Approval = nil
			return nil
		}
		if tp.decision == Denied {
			p.Denied, p.Reason = true, cmp.Or(tp.reason, "This tool call was blocked.")
			p.Approval = nil
			return nil
		}
	}
	return nil
}

// apply records the decision and reports whether approved arguments changed.
func (p *PendingCall) apply(a Approval) bool {
	p.NeedsApproval = false
	kept := a
	// The arguments that run are kept in Call.
	kept.UpdatedInput = nil
	p.Approval = &kept
	if !a.Approved {
		p.Denied = true
		p.Reason = cmp.Or(a.Reason, Deny.Reason)
		return false
	}
	if a.UpdatedInput != nil && !bytes.Equal(a.UpdatedInput, p.Call.Arguments) {
		if p.ModelArguments == nil {
			p.ModelArguments = p.Call.Arguments
		}
		p.Call.Arguments = slices.Clone(a.UpdatedInput)
		return true
	}
	return false
}

// sameJSON reports whether a and b encode the same JSON value.
func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// lostResult answers a call with side effects that had started when the
// run stopped, for example because its process crashed. Whether it ran is
// unknown, so it is not repeated.
const lostResult = "The run stopped while this tool call was executing, so it may or may not have taken effect. " +
	"Check its effects before calling it again."

// execute runs the pending calls that have no result yet, in order.
// Adjacent read-only and concurrent calls run concurrently; see
// runConcurrently. Before each batch the state is checkpointed with the
// batch marked started, and each finished call keeps its result, so a
// resumed run neither loses nor repeats work. A batch with side effects
// does not run if its checkpoint cannot be saved.
//
// It reports nested when a call interrupted for a nested run's approvals;
// the calls after it are not run. It returns early, before the next batch,
// if the consumer stopped iterating or ctx is done; calls that did not run
// keep no result and are not marked started.
func (l *execution) execute(ctx context.Context, pending []*PendingCall) (nested bool, err error) {
	wrap := l.toolChain()
	todo := make([]*PendingCall, 0, len(pending))
	for _, p := range pending {
		switch {
		case p.Result != nil:
		case p.Started && !p.ReadOnly && !p.Denied && (p.Nested == nil || p.Resuming):
			// A call with side effects was cut off; see lostResult.
			p.Nested, p.NestedApprovals, p.Resuming = nil, nil, false
			p.Result = &model.ToolResult{CallID: p.Call.ID, Name: p.Call.Name, Content: lostResult, IsError: true}
			if err := l.finishCall(ctx, p); err != nil {
				return false, err
			}
		default:
			todo = append(todo, p)
		}
	}
	if l.stopped {
		return false, nil
	}
	// A concurrent call also runs concurrently when it resumes its nested
	// run, so sub-agents interrupted together resume together.
	concurrent := func(p *PendingCall) bool { return !p.Denied && (p.Concurrent || (p.ReadOnly && p.Nested == nil)) }
	for i := 0; i < len(todo); {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		j := i + 1
		if concurrent(todo[i]) {
			for j < len(todo) && concurrent(todo[j]) {
				j++
			}
		}
		batch := todo[i:j]
		started := make([]bool, len(batch))
		effects := false
		for k, p := range batch {
			started[k] = p.Started
			p.Started, p.Resuming = true, p.Nested != nil
			effects = effects || (!p.ReadOnly && !p.Denied)
		}
		// unstart restores the batch's marks when it does not run, so a
		// resumed run neither reports the calls as cut off nor loses them.
		unstart := func() {
			for k, p := range batch {
				p.Started, p.Resuming = started[k], false
			}
		}
		if err := l.r.saveCheckpoint(ctx, l.st); err != nil && effects {
			unstart()
			return false, fmt.Errorf("loop: save checkpoint before running tools: %w", err)
		}
		for _, p := range batch {
			l.emit(&Event{Type: EventToolCall, ToolCall: cloneCall(p.Call)})
		}
		if l.stopped {
			unstart()
			return false, nil
		}
		scopes := make([]*callScope, len(batch))
		results := make([]*model.ToolResult, len(batch))
		if len(batch) == 1 {
			results[0], scopes[0] = l.callOne(ctx, batch[0], wrap)
		} else {
			l.runConcurrently(ctx, batch, wrap, results, scopes)
		}
		// Every call of the batch that ran keeps its result, even if the
		// consumer stops meanwhile.
		cut := false
		for k, p := range batch {
			if results[k] == nil {
				// ctx was done before the call ran.
				p.Started, p.Resuming = started[k], false
				cut = true
				continue
			}
			if sc := scopes[k]; sc != nil {
				l.st.Usage.Add(sc.usage)
				if sc.interrupt != nil {
					p.Nested, p.NestedApprovals, p.Resuming = sc.interrupt, nil, false
					nested = true
					continue
				}
			}
			p.Nested, p.NestedApprovals, p.Resuming = nil, nil, false
			p.Result = results[k]
			if err := l.finishCall(ctx, p); err != nil {
				return false, err
			}
		}
		switch {
		case l.stopped:
			return false, nil
		case cut:
			return false, ctx.Err()
		case nested:
			return true, nil
		}
		i = j
	}
	return false, nil
}

// finishCall runs AfterTool middleware on a call's result and emits it.
func (l *execution) finishCall(ctx context.Context, p *PendingCall) error {
	if err := l.post(ctx, p, p.Result); err != nil {
		return err
	}
	l.emit(&Event{Type: EventToolResult, ToolCall: cloneCall(p.Call), ToolResult: p.Result})
	return nil
}

// callOne executes a call. The returned scope holds the usage and the
// interrupt the call reported; it is nil for denied calls. The result is
// nil if ctx was done before the call ran.
func (l *execution) callOne(ctx context.Context, p *PendingCall, wrap []tool.Middleware) (*model.ToolResult, *callScope) {
	if p.Denied {
		return &model.ToolResult{CallID: p.Call.ID, Name: p.Call.Name, Content: p.Reason, IsError: true}, nil
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	sc := &callScope{run: l.handle, callID: p.Call.ID}
	if p.Nested != nil {
		sc.resumed, sc.approvals = p.Nested, p.NestedApprovals
	}
	res := l.r.exec.ExecuteWith(withScope(ctx, sc), p.Call, wrap...)
	sc.close()
	if p.ModelArguments != nil {
		// The conversation keeps the arguments the model sent, which some
		// providers sign; tell the model what actually ran.
		res.Content = fmt.Sprintf("[This call ran with arguments changed before execution: %s]\n%s", p.Call.Arguments, res.Content)
	}
	return res, sc
}

// post runs AfterTool middleware on a result.
func (l *execution) post(ctx context.Context, p *PendingCall, res *model.ToolResult) error {
	tp := l.toolPlan(p)
	for _, m := range slices.Backward(l.r.stages.afterTool) {
		if err := m.AfterTool(ctx, l.handle, tp, res); err != nil {
			return err
		}
	}
	return nil
}

func cloneCall(c *model.ToolCall) *model.ToolCall {
	return &model.ToolCall{ID: c.ID, Name: c.Name, Arguments: slices.Clone(c.Arguments)}
}

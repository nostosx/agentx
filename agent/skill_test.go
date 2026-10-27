package agent_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

func defineSkill(t *testing.T, meta skill.Metadata, body string) skill.Skill {
	t.Helper()
	if meta.Description == "" {
		meta.Description = "The " + meta.Name + " skill"
	}
	s, err := skill.Define(meta, body)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func invoke(id, name, args string) modeltest.Call {
	return modeltest.Call{ID: id, Name: skill.ToolName, Args: map[string]string{"name": name, "arguments": args}}
}

// dangerTool requires approval and counts its calls.
func dangerTool(calls *atomic.Int32) tool.Tool {
	return tool.Func("danger", "Does something that needs approval.", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "done", nil
	}, tool.RequireApproval())
}

// toolResults returns the tool results of a conversation by call ID.
func toolResults(msgs []*model.Message) map[string]*model.ToolResult {
	out := map[string]*model.ToolResult{}
	for _, m := range msgs {
		for _, r := range m.ToolResults() {
			out[r.CallID] = r
		}
	}
	return out
}

func TestSkillInvocationControl(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "greet", `"big World" second`)),
		modeltest.CallTools(invoke("c2", "deploy", "")),
		modeltest.CallTools(invoke("c3", "greet", `"big World" second`)),
		modeltest.Reply("ok"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "greet", Arguments: []string{"who"}, WhenToUse: "When greeting.", ArgumentHint: "[who]"}, "Hello $who ($1) in ${CLAUDE_SESSION_ID}."),
		defineSkill(t, skill.Metadata{Name: "deploy", DisableModelInvocation: true}, "Deploy."),
		defineSkill(t, skill.Metadata{Name: "background", UserInvocable: new(false)}, "Background knowledge."),
		defineSkill(t, skill.Metadata{Name: "api", Paths: []string{"src/**"}}, "API rules."),
	))
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "sess-1", "hi")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	sys := m.Requests()[0].System
	if !strings.Contains(sys, "- greet [who]: The greet skill When greeting.") || !strings.Contains(sys, "- background:") ||
		strings.Contains(sys, "deploy") || strings.Contains(sys, "- api") {
		t.Errorf("listing = %q", sys)
	}
	results := toolResults(res.Messages)
	if r := results["c1"]; r.IsError || !strings.Contains(r.Content, "Hello big World (second) in sess-1.") {
		t.Errorf("greet = %+v", r)
	}
	if r := results["c2"]; !r.IsError || !strings.Contains(r.Content, "can only be invoked by the user") {
		t.Errorf("deploy = %+v", r)
	}
	if r := results["c3"]; r.IsError || !strings.Contains(r.Content, "already loaded") {
		t.Errorf("repeated greet = %+v", r)
	}
}

func TestSkillAllowedToolsLastForTheRun(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "ops", "")),
		modeltest.CallTools(modeltest.Call{ID: "c2", Name: "danger"}),
		modeltest.Reply("first done"),
		modeltest.CallTools(modeltest.Call{ID: "c3", Name: "danger"}),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "ops", AllowedTools: []string{"danger"}}, "Operate."),
	))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusCompleted || calls.Load() != 1 {
		t.Fatalf("first run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	// The grant clears with the next message.
	res, err = runner.RunText(t.Context(), "s", "again")
	if err != nil || res.Status != loop.StatusInterrupted || calls.Load() != 1 {
		t.Fatalf("second run = %+v, %v; calls = %d", res, err, calls.Load())
	}
}

func TestSkillDisallowedToolsModelAndEffort(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	other := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "x", Name: "missing"}), modeltest.Reply("from other"))
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "focus", "")),
		modeltest.CallTools(modeltest.Call{ID: "c2", Name: "danger"}),
		modeltest.Reply("done"),
		modeltest.CallTools(invoke("c3", "switch", "")),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)),
		agent.WithReasoning(&model.Reasoning{Effort: "medium", BudgetTokens: 100}),
		agent.WithSkillConfig(skill.Config{Models: func(name string) model.Model {
			if name == "other" {
				return other
			}
			return nil
		}}),
		agent.WithSkills(
			defineSkill(t, skill.Metadata{Name: "focus", DisallowedTools: []string{"danger"}, Model: "small-model", Effort: "low"}, "Effort ${CLAUDE_EFFORT}."),
			defineSkill(t, skill.Metadata{Name: "switch", Model: "other"}, "Switch."),
		))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusCompleted || calls.Load() != 0 {
		t.Fatalf("run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	reqs := m.Requests()
	if reqs[0].Model != "" || reqs[0].Reasoning.Effort != "medium" || !hasTool(reqs[0], "danger") {
		t.Errorf("request before the skill = model %q, reasoning %+v", reqs[0].Model, reqs[0].Reasoning)
	}
	for _, req := range reqs[1:] {
		if req.Model != "small-model" || req.Reasoning.Effort != "low" || req.Reasoning.BudgetTokens != 100 || hasTool(req, "danger") {
			t.Errorf("request with the skill = model %q, reasoning %+v, tools %d", req.Model, req.Reasoning, len(req.Tools))
		}
	}
	results := toolResults(res.Messages)
	if r := results["c2"]; !r.IsError || !strings.Contains(r.Content, "not available") {
		t.Errorf("disallowed call = %+v", r)
	}
	if r := results["c1"]; !strings.Contains(r.Content, "Effort low.") {
		t.Errorf("effort substitution = %q", r.Content)
	}

	// A resolved model replaces the agent's model for the rest of the run.
	res, err = runner.RunText(t.Context(), "s2", "switch")
	if err != nil || res.Output != "from other" || len(other.Requests()) != 2 || other.Requests()[0].Model != "" {
		t.Fatalf("switch run = %+v, %v; other requests = %d", res, err, len(other.Requests()))
	}
}

func hasTool(req *model.Request, name string) bool {
	for _, spec := range req.Tools {
		if spec.Name == name {
			return true
		}
	}
	return false
}

// readTool is a read_file tool that records nothing.
var readTool = tool.Func("read_file", "Read a file.", func(_ context.Context, in struct {
	Path string `json:"path"`
}) (string, error) {
	return "contents of " + in.Path, nil
}, tool.ReadOnly())

func TestSkillPathsActivate(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "read_file", Args: map[string]string{"path": "docs/a.md"}}),
		modeltest.CallTools(modeltest.Call{ID: "c2", Name: "read_file", Args: map[string]string{"path": "src/api/x.go"}}),
		modeltest.CallTools(modeltest.Call{ID: "c3", Name: "read_file", Args: map[string]string{"path": "src/api/y.go"}}),
		modeltest.Reply("read"),
		modeltest.Reply("second"),
	)
	dir := t.TempDir()
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(readTool),
		agent.WithSkillConfig(skill.Config{ProjectDir: dir}),
		agent.WithSkills(
			defineSkill(t, skill.Metadata{Name: "api", Paths: []string{"src/**/*.go"}}, "API rules."),
			defineSkill(t, skill.Metadata{Name: "manual", Paths: []string{"src/**"}, DisableModelInvocation: true}, "Manual."),
		))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "read")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.Requests()[0].System, "api") {
		t.Errorf("conditional skill listed before a match: %q", m.Requests()[0].System)
	}
	results := toolResults(res.Messages)
	if strings.Contains(results["c1"].Content, "skill") || !strings.Contains(results["c2"].Content, "- api: The api skill") ||
		strings.Contains(results["c2"].Content, "manual") || strings.Contains(results["c3"].Content, "- api") {
		t.Errorf("announcements = %q / %q / %q", results["c1"].Content, results["c2"].Content, results["c3"].Content)
	}
	if _, err := runner.RunText(t.Context(), "s", "again"); err != nil {
		t.Fatal(err)
	}
	if sys := m.Requests()[4].System; !strings.Contains(sys, "- api: The api skill") || strings.Contains(sys, "manual") {
		t.Errorf("listing after a match = %q", sys)
	}
}

func TestRunSkill(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "danger"}),
		modeltest.CallTools(invoke("c2", "deploy", "staging")),
		modeltest.Reply("deployed"),
		modeltest.CallTools(invoke("c3", "deploy", "again")),
		modeltest.Reply("refused"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "deploy", DisableModelInvocation: true, AllowedTools: []string{"danger"}}, "Deploy $0 now."),
		defineSkill(t, skill.Metadata{Name: "hidden", UserInvocable: new(false)}, "Hidden."),
	))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "please", agent.RunSkill("deploy", "prod"))
	if err != nil || res.Status != loop.StatusCompleted || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	input := m.Requests()[0].Messages[len(m.Requests()[0].Messages)-1]
	if !strings.HasPrefix(input.Parts[0].Text, `<skill name="deploy">`) || !strings.Contains(input.Parts[0].Text, "Deploy prod now.") || !strings.Contains(input.Text(), "please") {
		t.Errorf("input = %q", input.Text())
	}
	if r := toolResults(res.Messages)["c2"]; r.IsError {
		t.Errorf("model invocation of a skill the user invoked in the run = %+v", r)
	}
	// In the next run the model may not invoke it again.
	res, err = runner.RunText(t.Context(), "s", "more")
	if r := toolResults(res.Messages)["c3"]; err != nil || !r.IsError {
		t.Errorf("model invocation in a later run = %+v, %v", r, err)
	}
	for _, name := range []string{"hidden", "missing"} {
		res, err := runner.RunText(t.Context(), "other-"+name, "x", agent.RunSkill(name, ""))
		if err != nil || res.Status != loop.StatusBlocked {
			t.Errorf("RunSkill(%s) = %+v, %v", name, res, err)
		}
	}
	// Invoke renders for users without a run.
	text, err := skill.NewRegistry(defineSkill(t, skill.Metadata{Name: "deploy", DisableModelInvocation: true}, "Deploy $0.")).Invoke(t.Context(), "deploy", "qa")
	if err != nil || !strings.Contains(text, "Deploy qa.") {
		t.Errorf("Invoke() = %q, %v", text, err)
	}
	if _, err := skill.NewRegistry(defineSkill(t, skill.Metadata{Name: "hidden", UserInvocable: new(false)}, "x")).Invoke(t.Context(), "hidden", ""); err == nil {
		t.Error("Invoke accepted a skill hidden from users")
	}
}

func TestForkedSkill(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "research", "Go generics")),
		// The forked sub-agent shares the model.
		modeltest.CallTools(modeltest.Call{ID: "s1", Name: "danger"}),
		modeltest.Reply("found it"),
		modeltest.Reply("done"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "research", Context: "fork", Model: "tiny", Effort: "max", AllowedTools: []string{"Bash", "danger"}}, "Research $ARGUMENTS thoroughly."),
	))
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "s", "research")
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "done" || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	if r := toolResults(res.Messages)["c1"]; r.IsError || r.Content != "found it" {
		t.Errorf("fork result = %+v", r)
	}
	reqs := m.Requests()
	sub := reqs[1]
	if sub.Model != "tiny" || sub.Reasoning == nil || sub.Reasoning.Effort != "max" || !strings.Contains(sub.Messages[0].Text(), "Research Go generics thoroughly.") ||
		!strings.Contains(sub.System, "carry out a task for another AI agent") || hasTool(sub, skill.ToolName) {
		t.Errorf("sub-agent request = model %q, reasoning %+v, system %q, first message %q", sub.Model, sub.Reasoning, sub.System, sub.Messages[0].Text())
	}
	if reqs[3].Model != "" || reqs[3].Reasoning != nil {
		t.Errorf("the forked skill's model or effort leaked into the parent: %q %+v", reqs[3].Model, reqs[3].Reasoning)
	}
	// The usage of the sub-agent counts for the parent run.
	if res.Usage.Requests != 4 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestForkedSkillAgents(t *testing.T) {
	t.Parallel()
	explore := modeltest.New(modeltest.Reply("explored"))
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "scan", ""), invoke("c2", "bad", "")),
		modeltest.Reply("done"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m),
		agent.WithSkillAgents(agent.MustNew("Explore", agent.WithModel(explore), agent.WithInstructions("You explore."))),
		agent.WithSkills(
			defineSkill(t, skill.Metadata{Name: "scan", Context: "fork", Agent: "Explore", Model: "fast"}, "Scan the code."),
			defineSkill(t, skill.Metadata{Name: "bad", Context: "fork", Agent: "nobody"}, "Never runs."),
		))
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "s", "scan")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := toolResults(res.Messages)
	if r := results["c1"]; r.IsError || r.Content != "explored" || explore.Requests()[0].Model != "fast" || explore.Requests()[0].System != "You explore." {
		t.Errorf("Explore result = %+v", r)
	}
	if r := results["c2"]; !r.IsError || !strings.Contains(r.Content, `unknown agent "nobody"`) {
		t.Errorf("unknown agent result = %+v", r)
	}
}

func TestForkedSkillInterruptResumes(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "task", "")),
		modeltest.CallTools(modeltest.Call{ID: "s1", Name: "danger"}),
		modeltest.Reply("sub done"),
		modeltest.Reply("parent done"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "task", Context: "fork"}, "Do the task with danger."),
	))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusInterrupted || len(res.Interrupt.Requests) != 1 {
		t.Fatalf("run = %+v, %v", res, err)
	}
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{res.Interrupt.Requests[0].Call.ID: loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "parent done" || calls.Load() != 1 {
		t.Fatalf("resumed = %+v, %v; calls = %d", res, err, calls.Load())
	}
	if r := toolResults(res.Messages)["c1"]; r.Content != "sub done" {
		t.Errorf("fork result = %+v", r)
	}
}

func TestRunSkillFork(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "report", "weekly")),
		modeltest.Reply("report body"),
		modeltest.Reply("here it is"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithSkills(
		defineSkill(t, skill.Metadata{Name: "report", Context: "fork", DisableModelInvocation: true}, "Write the $0 report."),
	))
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "s", "", agent.RunSkill("report", "weekly"))
	if err != nil || res.Output != "here it is" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if first := m.Requests()[0].Messages[0].Text(); !strings.Contains(first, `The user invoked the "report" skill`) || !strings.Contains(first, `"arguments":"weekly"`) {
		t.Errorf("input = %q", first)
	}
	if r := toolResults(res.Messages)["c1"]; r.IsError || r.Content != "report body" || !strings.Contains(m.Requests()[1].Messages[0].Text(), "Write the weekly report.") {
		t.Errorf("fork result = %+v", r)
	}
}

// A forked skill runs the parent's tools under the parent's permission
// policy: a call the policy denies is denied in the fork too.
func TestForkedSkillKeepsPermissions(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "task", "")),
		modeltest.CallTools(modeltest.Call{ID: "s1", Name: "danger"}),
		modeltest.Reply("sub done"),
		modeltest.Reply("parent done"),
	)
	policy := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Deny: []string{"danger"}}}})
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithPermissions(policy),
		agent.WithSkills(defineSkill(t, skill.Metadata{Name: "task", Context: "fork"}, "Do the task with danger.")))
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "parent done" || calls.Load() != 0 {
		t.Fatalf("run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	sub := m.Requests()[2]
	if r := toolResults(sub.Messages)["s1"]; r == nil || !r.IsError {
		t.Errorf("forked call result = %+v, want a denial", r)
	}
}

// Tool guardrails of the parent check the calls of a forked skill.
func TestForkedSkillKeepsToolGuardrails(t *testing.T) {
	t.Parallel()
	var calls, checks atomic.Int32
	echo := tool.Func("echo", "Echoes.", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "echo", nil
	})
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "task", "")),
		modeltest.CallTools(modeltest.Call{ID: "s1", Name: "echo"}),
		modeltest.Reply("sub done"),
		modeltest.Reply("parent done"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(echo),
		agent.WithToolGuardrail(guard.ToolGuardrail{Name: "no-echo", Tools: []string{"echo"},
			Before: func(context.Context, *loop.Run, *model.ToolCall) (guard.Result, error) {
				checks.Add(1)
				return guard.Result{Tripwire: true, Message: "no echo"}, nil
			}}),
		agent.WithSkills(defineSkill(t, skill.Metadata{Name: "task", Context: "fork"}, "Echo something.")))
	_, _ = agent.NewRunner(a, nil).RunText(t.Context(), "s", "go")
	if checks.Load() != 1 || calls.Load() != 0 {
		t.Errorf("guardrail checks = %d, tool calls = %d; want the fork's call checked and not run", checks.Load(), calls.Load())
	}
}

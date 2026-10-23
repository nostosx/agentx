package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

// generalPurpose is the agent of context: fork skills without an agent
// field, as in Claude Code.
const generalPurpose = "general-purpose"

// builtinForkAgents are Claude Code's built-in agent types, which run on a
// new agent unless WithSkillAgents provides one with the name.
var builtinForkAgents = []string{generalPurpose, "Explore", "Plan"}

// forkInstructions is the system prompt of the sub-agents that run
// context: fork skills without an agent of their own.
const forkInstructions = "You carry out a task for another AI agent. Use your tools to complete the task, then reply with a concise report of what you did and found; the other agent sees only your final reply."

// newSkillManager returns the skill manager of an agent and the hook
// manager its runs use: cfg.Hooks, or a copy of it that dispatches the
// hooks of skills.
func newSkillManager(cfg *Config) (*skill.Manager, *hook.Manager, error) {
	hooks := cfg.Hooks
	attach := len(cfg.Skills.HookEvents()) > 0
	if attach {
		// The agent's own copy, so the dispatchers are not added to a
		// manager the caller passed, or twice when a forked skill rebuilds
		// the agent.
		h := hook.NewManager()
		h.Merge(cfg.Hooks)
		hooks = h
	}
	sc := cfg.SkillConfig
	if sc.Effort == "" && cfg.Reasoning != nil {
		sc.Effort = cfg.Reasoning.Effort
	}
	if sc.HookOptions.Model == nil {
		sc.HookOptions.Model = cfg.Model
	}
	if sc.Fork == nil {
		sc.Fork = skillForker(*cfg, hooks)
	}
	m := skill.NewManager(cfg.Skills, sc)
	if attach {
		if err := m.AttachHooks(hooks); err != nil {
			return nil, nil, err
		}
	}
	return m, hooks, nil
}

// skillForker returns the skill.Forker of an agent configured by parent:
// it runs a context: fork skill on the agent its agent field names, as a
// sub-agent like AsToolWith, with the skill's model.
func skillForker(parent Config, hooks *hook.Manager) skill.Forker {
	return func(ctx context.Context, s skill.Skill, task string) (*tool.Result, error) {
		meta := s.Metadata()
		name := cmp.Or(strings.TrimSpace(meta.Agent), generalPurpose)
		var base Agent
		var names []string
		for _, a := range parent.SkillAgents {
			names = append(names, a.Name())
			if a.Name() == name && base == nil {
				base = a
			}
		}
		if base == nil && !slices.Contains(builtinForkAgents, name) {
			return tool.Errorf("skill %s: unknown agent %q; available agents: %s", meta.Name, name, strings.Join(slices.Concat(builtinForkAgents, names), ", ")), nil
		}
		a, err := forkAgent(parent, base, name, strings.TrimSpace(meta.Model))
		if err != nil {
			return nil, err
		}
		t := AsToolWith(a, ToolOptions{Name: skill.ToolName, Description: meta.Description, Hooks: hooks, Events: parent.SubAgentEvents, MaxDepth: parent.MaxDepth})
		args, err := json.Marshal(taskArgs{Prompt: task})
		if err != nil {
			return nil, err
		}
		return t.Call(ctx, args)
	}
}

// forkAgent returns the agent that runs a context: fork skill: base, or a
// new agent named name with parent's model, tools, tool middleware,
// approver and limits. modelName, unless empty or "inherit", replaces the
// model of an LLMAgent: with the model SkillConfig.Models resolves it to,
// or else with the same model sending the name as model.Request.Model.
// Agents other than LLMAgents run as they are.
func forkAgent(parent Config, base Agent, name, modelName string) (Agent, error) {
	resolve := func(m model.Model) model.Model {
		if modelName == "" || modelName == "inherit" {
			return m
		}
		if r := parent.SkillConfig.Models; r != nil {
			if mdl := r(modelName); mdl != nil {
				return mdl
			}
		}
		return namedModel{Model: m, name: modelName}
	}
	switch b := base.(type) {
	case nil:
		return build(Config{
			Name:            name,
			Model:           resolve(parent.Model),
			Instructions:    forkInstructions,
			Tools:           parent.Tools,
			ToolMiddlewares: parent.ToolMiddlewares,
			Approver:        parent.Approver,
			MaxTurns:        parent.MaxTurns,
			MaxTokens:       parent.MaxTokens,
			Reasoning:       parent.Reasoning,
			Stream:          parent.Stream,
			Parallelism:     parent.Parallelism,
			Logger:          parent.Logger,
			Limits:          parent.Limits,
			SkillConfig: skill.Config{
				ProjectDir: parent.SkillConfig.ProjectDir,
				Files:      parent.SkillConfig.Files,
				MatchTool:  parent.SkillConfig.MatchTool,
			},
			forkedSkill: true,
		})
	case *LLMAgent:
		c := b.cfg
		c.Model = resolve(c.Model)
		c.forkedSkill = true
		return build(c)
	}
	return base, nil
}

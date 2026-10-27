// Package actx implements context engineering for agents: assembling the
// system prompt and a reminder for each new input from providers, tracking
// the context window and compacting conversations that grow too large.
// Providers run once per run, before it starts, and the reminder is attached
// to that run's user input.
//
// Compaction edits earlier history, which invalidates Anthropic thinking
// replayed after the edit on models that check the prefix of thinking
// blocks, such as Claude Opus 5.5. The anthropic provider asks the API to
// drop such blocks, so requests succeed but the model thinks again.
package actx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nostosx/agentx/model"
)

// Placement says where a section is inserted.
type Placement int

const (
	// PlaceSystem appends the section to the system prompt. Use it for stable
	// content so the prompt prefix stays cacheable.
	PlaceSystem Placement = iota
	// PlaceReminder attaches the section to the user input of the current
	// run inside a <system-reminder> block. Use it for content that varies
	// per request, such as recalled memories.
	PlaceReminder
)

// Section is a piece of context contributed by a Provider.
type Section struct {
	Name      string
	Content   string
	Placement Placement
	// Priority orders sections; lower values come first. Sections with equal
	// priority keep provider order.
	Priority int
}

// Input describes the run for which context is assembled.
type Input struct {
	SessionID string
	Agent     string
	// Messages is the conversation history before the new input.
	Messages []*model.Message
	// Query is the text of the new user input.
	Query string
}

// Provider contributes sections to the model context.
type Provider interface {
	Sections(ctx context.Context, in *Input) ([]Section, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, in *Input) ([]Section, error)

// Sections calls f.
func (f ProviderFunc) Sections(ctx context.Context, in *Input) ([]Section, error) { return f(ctx, in) }

// Static returns a provider that always contributes the given section.
func Static(name, content string, placement Placement) Provider {
	return ProviderFunc(func(context.Context, *Input) ([]Section, error) {
		return []Section{{Name: name, Content: content, Placement: placement}}, nil
	})
}

// Config configures a Manager.
type Config struct {
	Window  Window
	Counter TokenCounter
	// Strategy compacts the conversation. Defaults to
	// Chain(ClearToolResults{}, ClearToolArguments{}, Truncate{}).
	Strategy  Strategy
	Providers []Provider
	// DisableAutoCompact turns off automatic compaction; Compact can still be
	// called explicitly.
	DisableAutoCompact bool
}

// Manager assembles context and compacts conversations.
//
// A nil *Manager behaves like NewManager(Config{}).
type Manager struct {
	cfg Config

	mu       sync.Mutex
	factor   float64
	observed bool
}

// NewManager returns a manager.
//
// The default strategy never clears tool results the model has not seen
// yet and always keeps the latest step of a turn, so a single batch of
// unseen tool results larger than the window cannot be compacted; see
// ClearToolResults.
func NewManager(cfg Config) *Manager {
	if cfg.Window.Limit == 0 {
		// Fill only the fields left unset, so a ReserveOutput or Threshold
		// configured without a Limit is kept.
		w := &cfg.Window
		w.Limit = DefaultWindow.Limit
		if w.ReserveOutput == 0 {
			w.ReserveOutput = DefaultWindow.ReserveOutput
		}
		if w.Threshold == 0 {
			w.Threshold = DefaultWindow.Threshold
		}
		if w.TargetRatio == 0 {
			w.TargetRatio = DefaultWindow.TargetRatio
		}
	}
	if cfg.Counter == nil {
		cfg.Counter = EstimateCounter{}
	}
	if cfg.Strategy == nil {
		cfg.Strategy = Chain(ClearToolResults{}, ClearToolArguments{}, Truncate{})
	}
	return &Manager{cfg: cfg, factor: 1}
}

var defaultManager = NewManager(Config{})

func (m *Manager) orDefault() *Manager {
	if m == nil {
		return defaultManager
	}
	return m
}

// AddProvider registers a provider. It is not safe to call concurrently
// with Prepare.
func (m *Manager) AddProvider(p Provider) { m.cfg.Providers = append(m.cfg.Providers, p) }

// Window returns the configured window.
func (m *Manager) Window() Window { return m.orDefault().cfg.Window }

// Prepared is the context assembled for a run.
type Prepared struct {
	// System is the base instructions followed by system sections.
	System string
	// Reminder is the combined reminder sections, already wrapped in a
	// <system-reminder> block, or empty.
	Reminder string
}

// Prepare collects sections from all providers. Provider errors are joined
// and returned along with the sections that succeeded.
func (m *Manager) Prepare(ctx context.Context, instructions string, in *Input) (*Prepared, error) {
	m = m.orDefault()
	var sections []Section
	var errs []error
	for _, p := range m.cfg.Providers {
		s, err := p.Sections(ctx, in)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sections = append(sections, s...)
	}
	slices.SortStableFunc(sections, func(a, b Section) int { return cmp.Compare(a.Priority, b.Priority) })

	var sys, rem []string
	if strings.TrimSpace(instructions) != "" {
		sys = append(sys, strings.TrimSpace(instructions))
	}
	for _, s := range sections {
		content := strings.TrimSpace(s.Content)
		if content == "" {
			continue
		}
		if s.Placement == PlaceReminder {
			rem = append(rem, content)
		} else {
			sys = append(sys, content)
		}
	}
	out := &Prepared{System: strings.Join(sys, "\n\n")}
	if len(rem) > 0 {
		out.Reminder = model.WrapReminder(strings.Join(rem, "\n\n"))
	}
	return out, errors.Join(errs...)
}

// Count estimates the input size of a request.
func (m *Manager) Count(system string, msgs []*model.Message, tools []*model.ToolSpec) int {
	m = m.orDefault()
	n := m.rawCount(system, msgs, tools)
	m.mu.Lock()
	f := m.factor
	m.mu.Unlock()
	return int(float64(n) * f)
}

func (m *Manager) rawCount(system string, msgs []*model.Message, tools []*model.ToolSpec) int {
	c := m.cfg.Counter
	n := c.CountText(system) + c.CountMessages(msgs)
	for _, t := range tools {
		n += c.CountText(t.Name) + c.CountText(t.Description) + c.CountText(string(t.Parameters))
	}
	return n
}

// Observe calibrates estimates against the prompt size reported by the
// provider for a request. The first observation sets the calibration;
// later ones are averaged with it, halving the weight of older requests
// each time.
func (m *Manager) Observe(system string, msgs []*model.Message, tools []*model.ToolSpec, usage model.Usage) {
	m = m.orDefault()
	actual := usage.PromptTokens()
	if actual <= 0 {
		return
	}
	est := m.rawCount(system, msgs, tools)
	if est <= 0 {
		return
	}
	f := float64(actual) / float64(est)
	f = min(max(f, 0.5), 3)
	m.mu.Lock()
	if m.observed {
		// Smooth the factor so one unusual request, such as one dominated
		// by images, does not swing the estimate of the next.
		f = (m.factor + f) / 2
	}
	m.factor, m.observed = f, true
	m.mu.Unlock()
}

// NeedsCompaction reports whether automatic compaction should run, and the
// estimated input size.
func (m *Manager) NeedsCompaction(system string, msgs []*model.Message, tools []*model.ToolSpec) (bool, int) {
	m = m.orDefault()
	n := m.Count(system, msgs, tools)
	if m.cfg.DisableAutoCompact {
		return false, n
	}
	return m.cfg.Window.ShouldCompact(n), n
}

// CompactResult describes a compaction.
type CompactResult struct {
	Messages []*model.Message
	Before   int
	After    int
	Summary  string
}

// Compact shrinks the conversation towards the window target. The system
// prompt and tools are counted against the target.
func (m *Manager) Compact(ctx context.Context, system string, msgs []*model.Message, tools []*model.ToolSpec) (*CompactResult, error) {
	m = m.orDefault()
	before := m.Count(system, msgs, tools)
	overhead := before - m.Count("", msgs, nil)
	target := max(m.cfg.Window.Target()-overhead, 0)
	out, err := m.cfg.Strategy.Compact(ctx, &CompactInput{
		System:   system,
		Messages: msgs,
		Target:   target,
		Counter:  calibrated{m},
	})
	if err != nil {
		return nil, fmt.Errorf("compact context: %w", err)
	}
	return &CompactResult{
		Messages: out.Messages,
		Before:   before,
		After:    m.Count(system, out.Messages, tools),
		Summary:  out.Summary,
	}, nil
}

// calibrated applies the manager's calibration factor to its counter.
type calibrated struct{ m *Manager }

func (c calibrated) CountText(s string) int { return c.m.Count(s, nil, nil) }

func (c calibrated) CountMessages(msgs []*model.Message) int { return c.m.Count("", msgs, nil) }

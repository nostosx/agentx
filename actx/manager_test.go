package actx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/model"
)

type byteCounter struct{}

func (byteCounter) CountText(s string) int { return len(s) }
func (byteCounter) CountMessages(msgs []*model.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text())
	}
	return n
}

func TestManagerPrepare(t *testing.T) {
	t.Parallel()
	errOne, errTwo := errors.New("first provider"), errors.New("second provider")
	in := &actx.Input{SessionID: "session", Agent: "agent", Query: "question"}
	m := actx.NewManager(actx.Config{Providers: []actx.Provider{
		actx.ProviderFunc(func(_ context.Context, got *actx.Input) ([]actx.Section, error) {
			if got != in {
				t.Error("provider did not receive original input")
			}
			return []actx.Section{
				{Content: " second ", Priority: 2},
				{Content: " first ", Priority: 1},
				{Content: " recall A ", Placement: actx.PlaceReminder},
				{Content: " \n\t "},
			}, nil
		}),
		actx.ProviderFunc(func(context.Context, *actx.Input) ([]actx.Section, error) {
			return []actx.Section{{Content: "must be discarded"}}, errOne
		}),
		actx.ProviderFunc(func(context.Context, *actx.Input) ([]actx.Section, error) { return nil, errTwo }),
	}})
	m.AddProvider(actx.Static("recall", "recall B", actx.PlaceReminder))
	m.AddProvider(actx.ProviderFunc(func(context.Context, *actx.Input) ([]actx.Section, error) {
		return []actx.Section{{Content: "third", Priority: 2}}, nil
	}))
	out, err := m.Prepare(t.Context(), " base \n", in)
	if !errors.Is(err, errOne) || !errors.Is(err, errTwo) {
		t.Fatalf("Prepare() error = %v, want both provider errors", err)
	}
	if out.System != "base\n\nfirst\n\nsecond\n\nthird" {
		t.Errorf("System = %q", out.System)
	}
	if want := "<system-reminder>\nrecall A\n\nrecall B\n</system-reminder>"; out.Reminder != want {
		t.Errorf("Reminder = %q, want %q", out.Reminder, want)
	}
}

func TestManagerDefaults(t *testing.T) {
	t.Parallel()
	var m *actx.Manager
	out, err := m.Prepare(t.Context(), " hello ", nil)
	if err != nil || out.System != "hello" || out.Reminder != "" {
		t.Fatalf("nil manager Prepare() = %+v, %v", out, err)
	}
	if m.Window() != actx.DefaultWindow {
		t.Errorf("nil manager Window() = %+v", m.Window())
	}
}

func TestManagerObserve(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		usage model.Usage
		want  int
	}{
		{"calibrated with cache", model.Usage{InputTokens: 10, CacheReadTokens: 5, CacheWriteTokens: 5}, 20},
		{"upper clamp", model.Usage{InputTokens: 1000}, 30},
		{"lower clamp", model.Usage{InputTokens: 1}, 5},
		{"ignore missing usage", model.Usage{}, 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := actx.NewManager(actx.Config{Counter: byteCounter{}})
			m.Observe("1234567890", nil, nil, tt.usage)
			if got := m.Count("1234567890", nil, nil); got != tt.want {
				t.Errorf("Count() = %d, want %d", got, tt.want)
			}
		})
	}
	m := actx.NewManager(actx.Config{Counter: byteCounter{}})
	m.Observe("", nil, nil, model.Usage{InputTokens: 10})
	if got := m.Count("abcd", nil, nil); got != 4 {
		t.Errorf("empty estimate changed calibration: %d", got)
	}
}

func TestManagerCompactBudgetAndErrors(t *testing.T) {
	t.Parallel()
	msgs := []*model.Message{model.UserText("0123456789")}
	tools := []*model.ToolSpec{{Name: "tool", Description: "desc", Parameters: []byte("{}")}}
	for _, tt := range []struct {
		name   string
		system string
		target int
	}{
		{"subtract overhead", "12345", 35},
		{"overhead exhausts target", strings.Repeat("s", 60), 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := actx.NewManager(actx.Config{
				Window: actx.Window{Limit: 100, TargetRatio: 0.5}, Counter: byteCounter{},
				Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
					if in.Target != tt.target || in.System != tt.system || !reflect.DeepEqual(in.Messages, msgs) {
						t.Errorf("strategy input = %+v, want target %d", in, tt.target)
					}
					return &actx.CompactOutput{Messages: []*model.Message{model.UserText("x")}, Summary: "saved"}, nil
				}),
			})
			out, err := m.Compact(t.Context(), tt.system, msgs, tools)
			if err != nil {
				t.Fatal(err)
			}
			if out.Before != len(tt.system)+20 || out.After != len(tt.system)+11 || out.Summary != "saved" {
				t.Errorf("Compact() = %+v", out)
			}
		})
	}
	wantErr := errors.New("strategy failed")
	m := actx.NewManager(actx.Config{Strategy: actx.StrategyFunc(func(context.Context, *actx.CompactInput) (*actx.CompactOutput, error) { return nil, wantErr })})
	if _, err := m.Compact(t.Context(), "", msgs, nil); !errors.Is(err, wantErr) {
		t.Errorf("Compact() error = %v, want wrapped strategy error", err)
	}
}

func TestManagerNeedsCompactionAndConcurrentCalibration(t *testing.T) {
	t.Parallel()
	for _, disabled := range []bool{false, true} {
		m := actx.NewManager(actx.Config{Counter: byteCounter{}, Window: actx.Window{Limit: 10}, DisableAutoCompact: disabled})
		if need, n := m.NeedsCompaction("12345678", nil, nil); need == disabled || n != 8 {
			t.Errorf("NeedsCompaction(disabled=%v) = %v, %d", disabled, need, n)
		}
	}
	m := actx.NewManager(actx.Config{Counter: byteCounter{}})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 30 {
				m.Observe("abcd", nil, nil, model.Usage{InputTokens: 8})
				if got := m.Count("abcd", nil, nil); got != 4 && got != 8 {
					t.Errorf("concurrent Count() = %d", got)
				}
			}
		})
	}
	wg.Wait()
}

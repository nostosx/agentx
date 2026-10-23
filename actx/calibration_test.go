package actx_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/model"
)

func TestWrapReminderAlias(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "recall\nmore"} {
		want := "<system-reminder>\n" + text + "\n</system-reminder>"
		if got := actx.WrapReminder(text); got != want || got != model.WrapReminder(text) {
			t.Errorf("WrapReminder(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestCompactCounterIsCalibrated checks that strategies count with the
// manager's calibration, so their estimates match the target they get.
func TestCompactCounterIsCalibrated(t *testing.T) {
	t.Parallel()
	type counts struct{ text, msgs int }
	var got []counts
	m := actx.NewManager(actx.Config{
		Counter: byteCounter{},
		Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
			got = append(got, counts{in.Counter.CountText("abcd"), in.Counter.CountMessages([]*model.Message{model.UserText("abc")})})
			return &actx.CompactOutput{Messages: in.Messages}, nil
		}),
	})
	msgs := []*model.Message{model.UserText("x")}
	if _, err := m.Compact(t.Context(), "", msgs, nil); err != nil {
		t.Fatal(err)
	}
	// The provider reports twice the estimated prompt size.
	m.Observe("1234567890", nil, nil, model.Usage{InputTokens: 20})
	if _, err := m.Compact(t.Context(), "", msgs, nil); err != nil {
		t.Fatal(err)
	}
	if want := []counts{{4, 3}, {8, 6}}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("strategy counts = %v, want %v", got, want)
	}
}

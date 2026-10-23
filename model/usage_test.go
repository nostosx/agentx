package model_test

import (
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestUsageAccounting(t *testing.T) {
	t.Parallel()
	u := model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2, ReasoningTokens: 4, Requests: 1, ToolCalls: 2}
	u.Add(u)
	if u.InputTokens != 20 || u.OutputTokens != 10 || u.ReasoningTokens != 8 || u.Requests != 2 || u.ToolCalls != 4 || u.PromptTokens() != 30 || u.TotalTokens() != 40 {
		t.Fatalf("usage double-counted reasoning or omitted cache: %+v", u)
	}
}

package model

// Usage reports token consumption for one or more model calls.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadTokens and CacheWriteTokens count prompt-cache activity. They
	// are reported separately from InputTokens by providers that distinguish
	// them.
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens is the portion of OutputTokens spent on reasoning when
	// the provider reports it.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// Requests and ToolCalls count the model calls and executed tool calls
	// of a run, including those of nested runs such as sub-agents. The
	// runtime counts them; providers leave them zero.
	Requests  int `json:"requests,omitempty"`
	ToolCalls int `json:"tool_calls,omitempty"`
}

// TotalTokens returns all input and output tokens, including cache activity.
func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// PromptTokens returns every token the model read as input.
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Add accumulates other into u.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.Requests += other.Requests
	u.ToolCalls += other.ToolCalls
}

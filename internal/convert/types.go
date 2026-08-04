package convert

// OpenAI request models intentionally accept unknown content block shapes.
type ChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []map[string]any `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type OpenAITool struct {
	Type        string         `json:"type,omitempty"`
	Function    *ToolFunction  `json:"function,omitempty"`
	Name        string         `json:"name,omitempty"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

type ChatCompletionRequest struct {
	Model               string             `json:"model"`
	Messages            []ChatMessage      `json:"messages"`
	Stream              bool               `json:"stream,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	N                   *int               `json:"n,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Stop                any                `json:"stop,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
	ReasoningEffort     string             `json:"reasoning_effort,omitempty"`
	Tools               []OpenAITool       `json:"tools,omitempty"`
	ToolChoice          any                `json:"tool_choice,omitempty"`
	StreamOptions       map[string]any     `json:"stream_options,omitempty"`
	LogitBias           map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs            *bool              `json:"logprobs,omitempty"`
	TopLogprobs         *int               `json:"top_logprobs,omitempty"`
	User                string             `json:"user,omitempty"`
	Seed                *int               `json:"seed,omitempty"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicTool struct {
	Type           string         `json:"type,omitempty"`
	Name           string         `json:"name"`
	Description    *string        `json:"description,omitempty"`
	InputSchema    map[string]any `json:"input_schema,omitempty"`
	MaxUses        *int           `json:"max_uses,omitempty"`
	AllowedDomains []string       `json:"allowed_domains,omitempty"`
	BlockedDomains []string       `json:"blocked_domains,omitempty"`
	UserLocation   map[string]any `json:"user_location,omitempty"`
}

type AnthropicMessagesRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	System        any                `json:"system,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Thinking      map[string]any     `json:"thinking,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens *int
}

type UnifiedMessage struct {
	Role        string
	Content     any
	ToolCalls   []map[string]any
	ToolResults []map[string]any
	Images      []map[string]any
}

type UnifiedTool struct {
	Name        string
	Description *string
	InputSchema map[string]any
}

type Options struct {
	ToolDescriptionMaxLength int
	FakeReasoningEnabled     bool
	FakeReasoningMaxTokens   int
	FakeReasoningBudgetCap   int
	TruncationRecovery       bool
	MaxPayloadBytes          int
	AutoTrimPayload          bool
	HiddenModels             map[string]string
}

func DefaultOptions() Options {
	return Options{ToolDescriptionMaxLength: 10000, FakeReasoningEnabled: true, FakeReasoningMaxTokens: 4000, FakeReasoningBudgetCap: 10000, TruncationRecovery: true, MaxPayloadBytes: 600000}
}

type KiroPayloadResult struct {
	Payload           map[string]any
	ToolDocumentation string
}

type PayloadTrimStats struct {
	OriginalBytes   int
	FinalBytes      int
	OriginalEntries int
	FinalEntries    int
	Trimmed         bool
}

// Compatibility aliases for API-specific naming.
type OpenAIChatMessage = ChatMessage
type OpenAIChatCompletionRequest = ChatCompletionRequest
type Tool = OpenAITool
type MessagesRequest = AnthropicMessagesRequest

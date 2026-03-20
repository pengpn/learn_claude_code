package llm

import (
	"context"
	"fmt"
	"os"
)

// Message is a provider-agnostic conversation message.
type Message struct {
	Role        string // "user" or "assistant"
	Content     string
	ToolCalls   []ToolCall   // non-empty when assistant invokes tools
	ToolResults []ToolResult // non-empty for tool-result turn
}

// ToolCall represents a single tool invocation from the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON
}

// ToolResult carries the output of an executed tool.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// Tool declares a callable tool for the model.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // {"properties": {...}, "required": [...]}
}

// ChatParams groups all parameters for a single chat call.
type ChatParams struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Response is the provider-agnostic model reply.
type Response struct {
	Content   string
	ToolCalls []ToolCall
}

// HasToolCalls returns true when the model wants to invoke tools.
func (r *Response) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// Provider abstracts an LLM chat API.
type Provider interface {
	Chat(ctx context.Context, params ChatParams) (*Response, error)
}

// --- Message constructors ---

func UserMessage(content string) Message {
	return Message{Role: "user", Content: content}
}

func AssistantMessage(resp *Response) Message {
	return Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
}

func ToolResultsMessage(results []ToolResult) Message {
	return Message{Role: "user", ToolResults: results}
}

// --- Provider factory ---

// NewProvider creates a Provider and returns the model name.
//
// Resolution order:
//  1. config.yaml (searched from cwd upward), values support ${ENV_VAR}
//  2. Pure environment variables (fallback when no config file)
//
// Env var overrides (always checked):
//
//	PROVIDER  overrides config.provider
//	MODEL_ID  overrides the selected provider's model
func NewProvider() (provider Provider, model string, err error) {
	cfg, cfgErr := LoadConfig()
	if cfgErr != nil {
		return newProviderFromEnv()
	}
	return newProviderFromConfig(cfg)
}

// newProviderFromConfig builds a provider using config.yaml settings.
func newProviderFromConfig(cfg *Config) (Provider, string, error) {
	name := cfg.Provider
	if env := os.Getenv("PROVIDER"); env != "" {
		name = env
	}
	if name == "" {
		return nil, "", fmt.Errorf("config.yaml: provider field is empty")
	}

	pc, ok := cfg.Providers[name]
	if !ok {
		available := make([]string, 0, len(cfg.Providers))
		for k := range cfg.Providers {
			available = append(available, k)
		}
		return nil, "", fmt.Errorf("provider %q not found in config (available: %v)", name, available)
	}

	model := pc.Model
	if env := os.Getenv("MODEL_ID"); env != "" {
		model = env
	}

	// "anthropic" uses the Anthropic SDK; everything else uses OpenAI-compatible HTTP.
	if name == "anthropic" {
		return NewAnthropic(pc), model, nil
	}
	return NewOpenAI(pc), model, nil
}

// newProviderFromEnv is the fallback when no config.yaml is found.
func newProviderFromEnv() (Provider, string, error) {
	name := os.Getenv("PROVIDER")
	model := os.Getenv("MODEL_ID")

	if name == "" {
		switch {
		case os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_BASE_URL") != "":
			name = "anthropic"
		case os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("OPENAI_BASE_URL") != "":
			name = "openai"
		default:
			return nil, "", fmt.Errorf("no config.yaml found; set PROVIDER or API key env vars")
		}
	}

	switch name {
	case "anthropic":
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		return NewAnthropic(ProviderConfig{
			APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
			BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		}), model, nil
	default:
		if model == "" {
			model = "gpt-4o"
		}
		baseURL := os.Getenv("OPENAI_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return NewOpenAI(ProviderConfig{
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			BaseURL: baseURL,
		}), model, nil
	}
}

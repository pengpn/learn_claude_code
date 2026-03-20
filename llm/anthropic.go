package llm

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider wraps the Anthropic Messages API.
type AnthropicProvider struct {
	client anthropic.Client
}

func NewAnthropic(cfg ProviderConfig) *AnthropicProvider {
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &AnthropicProvider{client: anthropic.NewClient(opts...)}
}

func (p *AnthropicProvider) Chat(ctx context.Context, params ChatParams) (*Response, error) {
	maxTokens := int64(params.MaxTokens)
	if maxTokens == 0 {
		maxTokens = 4096
	}

	resp, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(params.Model),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: params.System}},
		Messages:  toAnthropicMessages(params.Messages),
		Tools:     toAnthropicTools(params.Tools),
	})
	if err != nil {
		return nil, err
	}
	return fromAnthropicResponse(resp), nil
}

// --- conversion helpers ---

func toAnthropicMessages(msgs []Message) []anthropic.MessageParam {
	var out []anthropic.MessageParam
	for _, m := range msgs {
		switch {
		case len(m.ToolResults) > 0:
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropic.NewToolResultBlock(tr.ToolCallID, tr.Content, tr.IsError))
			}
			out = append(out, anthropic.NewUserMessage(blocks...))

		case m.Role == "assistant":
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any
				_ = json.Unmarshal([]byte(tc.Arguments), &input)
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))

		default: // user
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	return out
}

func toAnthropicTools(tools []Tool) []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	for i := range tools {
		t := &tools[i]
		schema := anthropic.ToolInputSchemaParam{
			Properties: t.InputSchema["properties"],
		}
		if req := toStringSlice(t.InputSchema["required"]); len(req) > 0 {
			schema.Required = req
		}
		tp := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out
}

func fromAnthropicResponse(msg *anthropic.Message) *Response {
	resp := &Response{}
	var texts []string

	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			texts = append(texts, b.Text)
		case anthropic.ToolUseBlock:
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: string(b.Input),
			})
		}
	}
	resp.Content = strings.Join(texts, "\n")
	return resp
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []string:
		return val
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

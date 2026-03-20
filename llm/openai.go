package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIProvider implements the OpenAI-compatible chat completions API.
// Works with OpenAI, DeepSeek, Qwen, Kimi, Zhipu, MiniMax, and any other compatible service.
//
// base_url should include the version path, matching the OpenAI SDK convention:
//
//	OpenAI:   https://api.openai.com/v1
//	DeepSeek: https://api.deepseek.com/v1
//	Zhipu:    https://open.bigmodel.cn/api/paas/v4
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewOpenAI(cfg ProviderConfig) *OpenAIProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  &http.Client{},
	}
}

func (p *OpenAIProvider) Chat(ctx context.Context, params ChatParams) (*Response, error) {
	maxTokens := params.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	var msgs []oaiMessage
	if params.System != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: strPtr(params.System)})
	}
	msgs = append(msgs, toOpenAIMessages(params.Messages)...)

	body, err := json.Marshal(oaiRequest{
		Model:     params.Model,
		Messages:  msgs,
		Tools:     toOpenAITools(params.Tools),
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w\nbody: %s", err, truncate(string(respBody), 500))
	}
	if oaiResp.Error != nil {
		return nil, fmt.Errorf("API error [%s]: %s", oaiResp.Error.Type, oaiResp.Error.Message)
	}
	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("empty response (no choices)")
	}

	choice := oaiResp.Choices[0]
	resp := &Response{}
	if choice.Message.Content != nil {
		resp.Content = *choice.Message.Content
	}
	for _, tc := range choice.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return resp, nil
}

// --- conversion helpers ---

func toOpenAIMessages(msgs []Message) []oaiMessage {
	var out []oaiMessage
	for _, m := range msgs {
		switch {
		case len(m.ToolResults) > 0:
			for _, tr := range m.ToolResults {
				out = append(out, oaiMessage{
					Role:       "tool",
					Content:    strPtr(tr.Content),
					ToolCallID: tr.ToolCallID,
				})
			}
			if m.Content != "" {
				out = append(out, oaiMessage{Role: "user", Content: strPtr(m.Content)})
			}
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			msg := oaiMessage{Role: "assistant"}
			if m.Content != "" {
				msg.Content = strPtr(m.Content)
			}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: oaiToolCallFunc{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
			out = append(out, msg)
		default:
			out = append(out, oaiMessage{Role: m.Role, Content: strPtr(m.Content)})
		}
	}
	return out
}

func toOpenAITools(tools []Tool) []oaiTool {
	if len(tools) == 0 {
		return nil
	}
	var out []oaiTool
	for _, t := range tools {
		params := map[string]any{"type": "object"}
		for k, v := range t.InputSchema {
			params[k] = v
		}
		out = append(out, oaiTool{
			Type: "function",
			Function: oaiToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func strPtr(s string) *string { return &s }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// --- OpenAI API wire types ---

type oaiRequest struct {
	Model     string       `json:"model"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiToolCallFunc `json:"function"`
}

type oaiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiToolFunc `json:"function"`
}

type oaiToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaiResponse struct {
	Choices []oaiChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type oaiChoice struct {
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

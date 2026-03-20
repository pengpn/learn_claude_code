package main

/*
s06_context_compact - Compact

Three-layer compression pipeline so the agent can work forever:

    Every turn:
    +------------------+
    | Tool call result |
    +------------------+
            |
            v
    [Layer 1: microCompact]         (silent, every turn)
      Replace tool_result content older than last 3
      with "[Previous: used {tool_name}]"
            |
            v
    [Check: tokens > 50000?]
       |               |
       no              yes
       |               |
       v               v
    continue    [Layer 2: autoCompact]
                  Save full transcript to .transcripts/
                  Ask LLM to summarize conversation.
                  Replace all messages with [summary].
                        |
                        v
                [Layer 3: compact tool]
                  Model calls compact -> immediate summarization.
                  Same as auto, triggered manually.

Key insight: "The agent can forget strategically and keep working forever."
*/

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

const (
	threshold    = 50000 // estimated token limit before auto_compact
	keepRecent   = 3     // how many recent tool results to preserve
	maxOutputLen = 50000
)

var transcriptDir string

// estimateTokens gives a rough token count (~4 chars per token).
func estimateTokens(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Arguments) + len(tc.Name)
		}
		for _, tr := range m.ToolResults {
			total += len(tr.Content)
		}
	}
	return total / 4
}

// -- Layer 1: microCompact - replace old tool results with placeholders --

type toolResultRef struct {
	msgIdx    int
	resultIdx int
}

func microCompact(messages []llm.Message) {
	// Build tool_call_id -> tool_name map from assistant messages
	nameMap := map[string]string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}

	// Collect all tool result locations
	var refs []toolResultRef
	for i, m := range messages {
		if len(m.ToolResults) > 0 {
			for j := range m.ToolResults {
				refs = append(refs, toolResultRef{msgIdx: i, resultIdx: j})
			}
		}
	}

	if len(refs) <= keepRecent {
		return
	}

	// Replace old results (everything except the last keepRecent)
	toClear := refs[:len(refs)-keepRecent]
	for _, ref := range toClear {
		tr := &messages[ref.msgIdx].ToolResults[ref.resultIdx]
		if len(tr.Content) > 100 {
			name := nameMap[tr.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			tr.Content = fmt.Sprintf("[Previous: used %s]", name)
		}
	}
}

// -- Layer 2: autoCompact - save transcript, summarize, replace messages --

func autoCompact(ctx context.Context, provider llm.Provider, model string,
	messages []llm.Message,
) []llm.Message {
	// Save full transcript to disk
	_ = os.MkdirAll(transcriptDir, 0o755)
	transcriptPath := filepath.Join(transcriptDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	if f, err := os.Create(transcriptPath); err == nil {
		enc := json.NewEncoder(f)
		for _, m := range messages {
			_ = enc.Encode(m)
		}
		f.Close()
		fmt.Printf("[transcript saved: %s]\n", transcriptPath)
	}

	// Serialize conversation for summarization (truncate to ~80k chars)
	raw, _ := json.Marshal(messages)
	conversationText := string(raw)
	if len(conversationText) > 80000 {
		conversationText = conversationText[:80000]
	}

	// Ask LLM to summarize
	summaryResp, err := provider.Chat(ctx, llm.ChatParams{
		Model: model,
		Messages: []llm.Message{
			llm.UserMessage(
				"Summarize this conversation for continuity. Include: " +
					"1) What was accomplished, 2) Current state, 3) Key decisions made. " +
					"Be concise but preserve critical details.\n\n" + conversationText,
			),
		},
		MaxTokens: 2000,
	})
	summary := "(summary failed)"
	if err == nil && summaryResp.Content != "" {
		summary = summaryResp.Content
	}

	// Replace all messages with compressed summary
	return []llm.Message{
		llm.UserMessage(fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary)),
		{Role: "assistant", Content: "Understood. I have the context from the summary. Continuing."},
	}
}

// -- Tool implementations --

func safePath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Join(workdir, p))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, workdir) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}
	return abs, nil
}

var dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}

func runBash(command string) string {
	for _, d := range dangerousPatterns {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workdir
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}
	result := strings.TrimSpace(string(output))
	if err != nil && result == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	if result == "" {
		return "(no output)"
	}
	if len(result) > maxOutputLen {
		return result[:maxOutputLen]
	}
	return result
}

func runRead(path string, limit int) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	if limit > 0 && limit < len(lines) {
		lines = append(lines[:limit], fmt.Sprintf("... (%d more)", len(lines)-limit))
	}
	result := strings.Join(lines, "\n")
	if len(result) > maxOutputLen {
		return result[:maxOutputLen]
	}
	return result
}

func runWrite(path, content string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Wrote %d bytes", len(content))
}

func runEdit(path, oldText, newText string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}
	if err := os.WriteFile(fp, []byte(strings.Replace(content, oldText, newText, 1)), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Edited %s", path)
}

// -- Dispatch + tools --

type toolHandler func(args map[string]any) string

var toolHandlers = map[string]toolHandler{
	"bash": func(args map[string]any) string {
		return runBash(args["command"].(string))
	},
	"read_file": func(args map[string]any) string {
		limit := 0
		if v, ok := args["limit"]; ok {
			if f, ok := v.(float64); ok {
				limit = int(f)
			}
		}
		return runRead(args["path"].(string), limit)
	},
	"write_file": func(args map[string]any) string {
		return runWrite(args["path"].(string), args["content"].(string))
	},
	"edit_file": func(args map[string]any) string {
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	},
	"compact": func(args map[string]any) string {
		return "Manual compression requested."
	},
}

var tools = []llm.Tool{
	{
		Name: "bash", Description: "Run a shell command.",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	},
	{
		Name: "read_file", Description: "Read file contents.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []string{"path"},
		},
	},
	{
		Name: "write_file", Description: "Write content to file.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	},
	{
		Name: "edit_file", Description: "Replace exact text in file.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":     map[string]any{"type": "string"},
				"old_text": map[string]any{"type": "string"},
				"new_text": map[string]any{"type": "string"},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
	},
	{
		Name: "compact", Description: "Trigger manual conversation compression.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"focus": map[string]any{"type": "string", "description": "What to preserve in the summary"},
			},
		},
	},
}

// -- Agent loop --

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		// Layer 1: silently replace old tool results
		microCompact(*messages)

		// Layer 2: auto-compact when token estimate exceeds threshold
		if estimateTokens(*messages) > threshold {
			fmt.Println("[auto_compact triggered]")
			*messages = autoCompact(ctx, provider, model, *messages)
		}

		resp, err := provider.Chat(ctx, llm.ChatParams{
			Model:     model,
			System:    system,
			Messages:  *messages,
			Tools:     tools,
			MaxTokens: 8000,
		})
		if err != nil {
			return err
		}

		*messages = append(*messages, llm.AssistantMessage(resp))

		if !resp.HasToolCalls() {
			if resp.Content != "" {
				fmt.Println(resp.Content)
			}
			return nil
		}

		var results []llm.ToolResult
		manualCompact := false

		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			var output string
			if tc.Name == "compact" {
				manualCompact = true
				output = "Compressing..."
			} else if handler, ok := toolHandlers[tc.Name]; ok {
				output = handler(args)
			} else {
				output = fmt.Sprintf("Unknown tool: %s", tc.Name)
			}

			preview := output
			if len(preview) > 200 {
				preview = preview[:200]
			}
			fmt.Printf("> %s: %s\n", tc.Name, preview)

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}

		*messages = append(*messages, llm.ToolResultsMessage(results))

		// Layer 3: manual compact triggered by the compact tool
		if manualCompact {
			fmt.Println("[manual compact]")
			*messages = autoCompact(ctx, provider, model, *messages)
		}
	}
}

func main() {
	transcriptDir = filepath.Join(workdir, ".transcripts")

	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms06 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		messages = append(messages, llm.UserMessage(query))
		if err := agentLoop(context.Background(), provider, model, system, &messages); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
}

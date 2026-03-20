package main

/*
s04_subagent - Subagents

Spawn a child agent with fresh messages=[]. The child works in its own
context, sharing the filesystem, then returns only a summary to the parent.

    Parent agent                     Subagent
    +------------------+             +------------------+
    | messages=[...]   |             | messages=[]      |  <-- fresh
    |                  |  dispatch   |                  |
    | tool: task       | ---------->| while tool_use:  |
    |   prompt="..."   |            |   call tools     |
    |   description="" |            |   append results |
    |                  |  summary   |                  |
    |   result = "..." | <--------- | return last text |
    +------------------+             +------------------+
              |
    Parent context stays clean.
    Subagent context is discarded.

Key insight: "Process isolation gives context isolation for free."
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

// -- Tool implementations shared by parent and child --

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
	if len(result) > 50000 {
		return result[:50000]
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
	if len(result) > 50000 {
		return result[:50000]
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

// -- Dispatch map (shared by parent and child) --

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
}

// Child gets all base tools except task (no recursive spawning)
var childTools = []llm.Tool{
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
}

// Parent tools: base tools + task dispatcher
var parentTools = append(childTools, llm.Tool{
	Name: "task", Description: "Spawn a subagent with fresh context. It shares the filesystem but not conversation history.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"prompt":      map[string]any{"type": "string"},
			"description": map[string]any{"type": "string", "description": "Short description of the task"},
		},
		"required": []string{"prompt"},
	},
})

// -- Subagent: fresh context, filtered tools, summary-only return --

func runSubagent(ctx context.Context, provider llm.Provider, model string, prompt string) string {
	subSystem := fmt.Sprintf("You are a coding subagent at %s. Complete the given task, then summarize your findings.", workdir)
	messages := []llm.Message{llm.UserMessage(prompt)} // fresh context

	for range 30 { // safety limit
		resp, err := provider.Chat(ctx, llm.ChatParams{
			Model:     model,
			System:    subSystem,
			Messages:  messages,
			Tools:     childTools,
			MaxTokens: 8000,
		})
		if err != nil {
			return fmt.Sprintf("Subagent error: %v", err)
		}

		messages = append(messages, llm.AssistantMessage(resp))

		if !resp.HasToolCalls() {
			if resp.Content != "" {
				return resp.Content
			}
			return "(no summary)"
		}

		var results []llm.ToolResult
		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			handler, ok := toolHandlers[tc.Name]
			var output string
			if ok {
				output = handler(args)
			} else {
				output = fmt.Sprintf("Unknown tool: %s", tc.Name)
			}
			if len(output) > 50000 {
				output = output[:50000]
			}
			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		messages = append(messages, llm.ToolResultsMessage(results))
	}
	// Only the final text returns to the parent -- child context is discarded
	return "(subagent hit iteration limit)"
}

// -- Parent agent loop --

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		resp, err := provider.Chat(ctx, llm.ChatParams{
			Model:     model,
			System:    system,
			Messages:  *messages,
			Tools:     parentTools,
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
		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			var output string
			if tc.Name == "task" {
				desc := "subtask"
				if d, ok := args["description"].(string); ok && d != "" {
					desc = d
				}
				prompt := args["prompt"].(string)
				preview := prompt
				if len(preview) > 80 {
					preview = preview[:80]
				}
				fmt.Printf("> task (%s): %s\n", desc, preview)
				output = runSubagent(ctx, provider, model, prompt)
			} else {
				handler, ok := toolHandlers[tc.Name]
				if ok {
					output = handler(args)
				} else {
					output = fmt.Sprintf("Unknown tool: %s", tc.Name)
				}
			}

			preview := output
			if len(preview) > 200 {
				preview = preview[:200]
			}
			fmt.Printf("  %s\n", preview)

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		*messages = append(*messages, llm.ToolResultsMessage(results))
	}
}

func main() {
	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use the task tool to delegate exploration or subtasks.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms04 >> \033[0m")
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

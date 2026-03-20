//go:build ignore

/*
Minimal Agent Template - Copy and customize this.

This is the simplest possible working agent (~80 lines).
It has everything you need: 3 tools + loop.

Usage:

 1. Set up config.yaml or env vars
 2. go run minimal-agent.go
 3. Type commands, 'q' to quit
*/
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"learn_claude_code/llm"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var workdir string

func init() { workdir, _ = os.Getwd() }

var system = fmt.Sprintf(`You are a coding agent at %s.

Rules:
- Use tools to complete tasks
- Prefer action over explanation
- Summarize what you did when done`, workdir)

var tools = []llm.Tool{
	{
		Name: "bash", Description: "Run shell command",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	},
	{
		Name: "read_file", Description: "Read file contents",
		InputSchema: map[string]any{
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
	},
	{
		Name: "write_file", Description: "Write content to file",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	},
}

func executeTool(name string, args map[string]any) string {
	switch name {
	case "bash":
		cmd := exec.Command("bash", "-c", args["command"].(string))
		cmd.Dir = workdir
		out, err := cmd.CombinedOutput()
		result := strings.TrimSpace(string(out))
		if err != nil && result == "" {
			return fmt.Sprintf("Error: %v", err)
		}
		if result == "" {
			return "(empty)"
		}
		return result[:min(len(result), 50000)]
	case "read_file":
		data, err := os.ReadFile(filepath.Join(workdir, args["path"].(string)))
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		s := string(data)
		return s[:min(len(s), 50000)]
	case "write_file":
		p := filepath.Join(workdir, args["path"].(string))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		content := args["content"].(string)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Wrote %d bytes to %s", len(content), args["path"])
	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func agent(ctx context.Context, provider llm.Provider, model string,
	prompt string, history *[]llm.Message,
) string {
	*history = append(*history, llm.UserMessage(prompt))
	for {
		resp, err := provider.Chat(ctx, llm.ChatParams{
			Model: model, System: system, Messages: *history,
			Tools: tools, MaxTokens: 8000,
		})
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		*history = append(*history, llm.AssistantMessage(resp))
		if !resp.HasToolCalls() {
			return resp.Content
		}
		var results []llm.ToolResult
		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
			fmt.Printf("> %s: %v\n", tc.Name, args)
			output := executeTool(tc.Name, args)
			fmt.Printf("  %s...\n", output[:min(len(output), 100)])
			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		*history = append(*history, llm.ToolResultsMessage(results))
	}
}

func main() {
	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Minimal Agent - %s\nType 'q' to quit.\n\n", workdir)
	var history []llm.Message
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(">> ")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || query == "q" || query == "quit" || query == "exit" {
			break
		}
		fmt.Println(agent(context.Background(), provider, model, query, &history))
		fmt.Println()
	}
}

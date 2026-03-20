package main

/*
s01_agent_loop - The Agent Loop

The entire secret of an AI coding agent in one pattern:

    for resp.HasToolCalls() {
        response := provider.Chat(messages, tools)
        execute tools
        append results
    }

    +----------+      +-------+      +---------+
    |   User   | ---> |  LLM  | ---> |  Tool   |
    |  prompt  |      |       |      | execute |
    +----------+      +---+---+      +----+----+
                          ^               |
                          |   tool_result |
                          +---------------+
                          (loop continues)

This is the core loop: feed tool results back to the model
until the model decides to stop. Production agents layer
policy, hooks, and lifecycle controls on top.
*/

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"learn_claude_code/llm"
)

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

// -- The core pattern: a loop that calls tools until the model stops --

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message, tools []llm.Tool,
) error {
	for {
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

		// Append assistant turn
		*messages = append(*messages, llm.AssistantMessage(resp))

		// If the model didn't call a tool, we're done
		if !resp.HasToolCalls() {
			if resp.Content != "" {
				fmt.Println(resp.Content)
			}
			return nil
		}

		// Execute each tool call, collect results
		var results []llm.ToolResult
		for _, tc := range resp.ToolCalls {
			var input struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal([]byte(tc.Arguments), &input)

			fmt.Printf("\033[33m$ %s\033[0m\n", input.Command)
			output := runBash(input.Command)
			if len(output) > 200 {
				fmt.Println(output[:200])
			} else {
				fmt.Println(output)
			}
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

	cwd, _ := os.Getwd()
	system := fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", cwd)

	tools := []llm.Tool{{
		Name:        "bash",
		Description: "Run a shell command.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to run.",
				},
			},
			"required": []string{"command"},
		},
	}}

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms01 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		messages = append(messages, llm.UserMessage(query))
		if err := agentLoop(context.Background(), provider, model, system, &messages, tools); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
}

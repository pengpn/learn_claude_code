//go:build ignore

/*
Subagent Pattern - How to implement Task tool for context isolation.

The key insight: spawn child agents with ISOLATED context to prevent
"context pollution" where exploration details fill up the main conversation.
*/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"learn_claude_code/llm"
	"strings"
	"time"
)

// =============================================================================
// AGENT TYPE REGISTRY
// =============================================================================

type AgentTypeConfig struct {
	Description string
	Tools       []string // tool names, or ["*"] for all
	Prompt      string
}

var agentTypes = map[string]AgentTypeConfig{
	// Explore: Read-only, for searching and analyzing
	"explore": {
		Description: "Read-only agent for exploring code, finding files, searching",
		Tools:       []string{"bash", "read_file"}, // No write access!
		Prompt:      "You are an exploration agent. Search and analyze, but NEVER modify files. Return a concise summary of what you found.",
	},
	// Code: Full-powered, for implementation
	"code": {
		Description: "Full agent for implementing features and fixing bugs",
		Tools:       []string{"*"}, // All tools
		Prompt:      "You are a coding agent. Implement the requested changes efficiently. Return a summary of what you changed.",
	},
	// Plan: Read-only, for design work
	"plan": {
		Description: "Planning agent for designing implementation strategies",
		Tools:       []string{"bash", "read_file"}, // Read-only
		Prompt:      "You are a planning agent. Analyze the codebase and output a numbered implementation plan. Do NOT make any changes.",
	},
	// Add your own types here...
	// "test": {
	//     Description: "Testing agent for running and analyzing tests",
	//     Tools:       []string{"bash", "read_file"},
	//     Prompt:      "Run tests and report results. Don't modify code.",
	// },
}

func getAgentDescriptions() string {
	var lines []string
	for name, cfg := range agentTypes {
		lines = append(lines, fmt.Sprintf("- %s: %s", name, cfg.Description))
	}
	return strings.Join(lines, "\n")
}

// getToolsForAgent filters tools based on agent type.
// "*" means all base tools. Otherwise, whitelist specific tool names.
// Note: Subagents don't get Task tool to prevent infinite recursion.
func getToolsForAgent(agentType string, baseTools []llm.Tool) []llm.Tool {
	cfg, ok := agentTypes[agentType]
	if !ok {
		return baseTools
	}
	if len(cfg.Tools) == 1 && cfg.Tools[0] == "*" {
		return baseTools // All base tools, but NOT Task
	}
	allowed := make(map[string]bool)
	for _, t := range cfg.Tools {
		allowed[t] = true
	}
	var filtered []llm.Tool
	for _, t := range baseTools {
		if allowed[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// =============================================================================
// TASK TOOL DEFINITION
// =============================================================================

func makeTaskTool() llm.Tool {
	var typeNames []string
	for name := range agentTypes {
		typeNames = append(typeNames, name)
	}
	return llm.Tool{
		Name: "Task",
		Description: fmt.Sprintf(`Spawn a subagent for a focused subtask.

Subagents run in ISOLATED context - they don't see parent's history.
Use this to keep the main conversation clean.

Agent types:
%s

Example uses:
- Task(explore): "Find all files using the auth module"
- Task(plan): "Design a migration strategy for the database"
- Task(code): "Implement the user registration form"`, getAgentDescriptions()),
		InputSchema: map[string]any{
			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "Short task name (3-5 words) for progress display"},
				"prompt":      map[string]any{"type": "string", "description": "Detailed instructions for the subagent"},
				"agent_type":  map[string]any{"type": "string", "enum": typeNames, "description": "Type of agent to spawn"},
			},
			"required": []string{"description", "prompt", "agent_type"},
		},
	}
}

// =============================================================================
// SUBAGENT EXECUTION
// =============================================================================

// RunTask executes a subagent task with isolated context.
//
// Key concepts:
//  1. ISOLATED HISTORY - subagent starts fresh, no parent context
//  2. FILTERED TOOLS - based on agent type permissions
//  3. AGENT-SPECIFIC PROMPT - specialized behavior
//  4. RETURNS SUMMARY ONLY - parent sees just the final result
func RunTask(
	ctx context.Context,
	provider llm.Provider,
	model string,
	workdir string,
	baseTools []llm.Tool,
	executeTool func(string, map[string]any) string,
	description, prompt, agentType string,
) string {
	cfg, ok := agentTypes[agentType]
	if !ok {
		return fmt.Sprintf("Error: Unknown agent type '%s'", agentType)
	}

	subSystem := fmt.Sprintf("You are a %s subagent at %s.\n\n%s\n\nComplete the task and return a clear, concise summary.",
		agentType, workdir, cfg.Prompt)
	subTools := getToolsForAgent(agentType, baseTools)

	// KEY: ISOLATED message history!
	// The subagent starts fresh, doesn't see parent's conversation.
	messages := []llm.Message{llm.UserMessage(prompt)}

	// Progress display
	fmt.Printf("  [%s] %s\n", agentType, description)
	start := time.Now()
	toolCount := 0

	for range 30 {
		resp, err := provider.Chat(ctx, llm.ChatParams{
			Model: model, System: subSystem, Messages: messages,
			Tools: subTools, MaxTokens: 8000,
		})
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}

		if !resp.HasToolCalls() {
			elapsed := time.Since(start)
			fmt.Printf("\r  [%s] %s - done (%d tools, %.1fs)\n",
				agentType, description, toolCount, elapsed.Seconds())
			if resp.Content != "" {
				return resp.Content
			}
			return "(subagent returned no text)"
		}

		var results []llm.ToolResult
		for _, tc := range resp.ToolCalls {
			toolCount++
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
			output := executeTool(tc.Name, args)
			if len(output) > 50000 {
				output = output[:50000]
			}
			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})

			// Update progress (in-place on same line)
			elapsed := time.Since(start)
			fmt.Printf("\r  [%s] %s ... %d tools, %.1fs",
				agentType, description, toolCount, elapsed.Seconds())
		}

		messages = append(messages, llm.AssistantMessage(resp))
		messages = append(messages, llm.ToolResultsMessage(results))
	}

	return "(subagent hit iteration limit)"
}

// =============================================================================
// USAGE EXAMPLE
// =============================================================================

/*
In your main agent's executeTool function:

	func executeTool(name string, args map[string]any) string {
	    if name == "Task" {
	        return RunTask(
	            ctx, provider, model, workdir, baseTools, executeTool,
	            args["description"].(string),
	            args["prompt"].(string),
	            args["agent_type"].(string),
	        )
	    }
	    // ... other tools ...
	}

In your tools slice:

	tools := append(baseTools, makeTaskTool())
*/

func main() {} // Reference only — not meant to be run directly

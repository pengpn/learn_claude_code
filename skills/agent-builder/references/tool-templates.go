//go:build ignore

/*
Tool Templates - Copy and customize these for your agent.

Each tool needs:
1. Definition (JSON schema for the model)
2. Implementation (Go function)
*/
package main

import (
	"context"
	"fmt"
	"learn_claude_code/llm"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var WORKDIR string

func init() { WORKDIR, _ = os.Getwd() }

// =============================================================================
// TOOL DEFINITIONS (for tools slice)
// =============================================================================

var BashTool = llm.Tool{
	Name:        "bash",
	Description: "Run a shell command. Use for: ls, find, grep, git, npm, go, etc.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to execute"},
		},
		"required": []string{"command"},
	},
}

var ReadFileTool = llm.Tool{
	Name:        "read_file",
	Description: "Read file contents. Returns UTF-8 text.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "Relative path to the file"},
			"limit": map[string]any{"type": "integer", "description": "Max lines to read (default: all)"},
		},
		"required": []string{"path"},
	},
}

var WriteFileTool = llm.Tool{
	Name:        "write_file",
	Description: "Write content to a file. Creates parent directories if needed.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Relative path for the file"},
			"content": map[string]any{"type": "string", "description": "Content to write"},
		},
		"required": []string{"path", "content"},
	},
}

var EditFileTool = llm.Tool{
	Name:        "edit_file",
	Description: "Replace exact text in a file. Use for surgical edits.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"path":     map[string]any{"type": "string", "description": "Relative path to the file"},
			"old_text": map[string]any{"type": "string", "description": "Exact text to find (must match precisely)"},
			"new_text": map[string]any{"type": "string", "description": "Replacement text"},
		},
		"required": []string{"path", "old_text", "new_text"},
	},
}

var TodoWriteTool = llm.Tool{
	Name:        "TodoWrite",
	Description: "Update the task list. Use to plan and track progress.",
	InputSchema: map[string]any{
		"properties": map[string]any{
			"items": map[string]any{
				"type":        "array",
				"description": "Complete list of tasks",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":    map[string]any{"type": "string", "description": "Task description"},
						"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						"activeForm": map[string]any{"type": "string", "description": "Present tense, e.g. 'Reading files'"},
					},
					"required": []string{"content", "status", "activeForm"},
				},
			},
		},
		"required": []string{"items"},
	},
}

// Task tool is generated dynamically with agent types — see subagent-pattern.go

// =============================================================================
// TOOL IMPLEMENTATIONS
// =============================================================================

// safePath ensures the path stays within the workspace.
// Prevents ../../../etc/passwd attacks.
func safePath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Join(WORKDIR, p))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, WORKDIR) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}
	return abs, nil
}

// runBash executes a shell command with safety checks.
// Safety features: blocks dangerous commands, 60s timeout, output truncated to 50KB.
func runBash(command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = WORKDIR
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Command timed out (60s)"
	}
	result := strings.TrimSpace(string(out))
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

// runReadFile reads file contents with optional line limit.
func runReadFile(path string, limit int) string {
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
		lines = append(lines[:limit], fmt.Sprintf("... (%d more lines)", len(lines)-limit))
	}
	result := strings.Join(lines, "\n")
	if len(result) > 50000 {
		return result[:50000]
	}
	return result
}

// runWriteFile writes content to file, creating parent directories if needed.
func runWriteFile(path, content string) string {
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
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// runEditFile replaces exact text in a file (surgical edit).
// Only replaces first occurrence for safety.
func runEditFile(path, oldText, newText string) string {
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

// =============================================================================
// DISPATCHER PATTERN
// =============================================================================

// executeTool dispatches a tool call to its implementation.
//
// This pattern makes it easy to add new tools:
// 1. Add definition to tools slice
// 2. Add implementation function
// 3. Add case to this dispatcher
func executeTool(name string, args map[string]any) string {
	switch name {
	case "bash":
		return runBash(args["command"].(string))
	case "read_file":
		limit := 0
		if v, ok := args["limit"]; ok {
			if f, ok := v.(float64); ok {
				limit = int(f)
			}
		}
		return runReadFile(args["path"].(string), limit)
	case "write_file":
		return runWriteFile(args["path"].(string), args["content"].(string))
	case "edit_file":
		return runEditFile(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func main() {} // Reference only — not meant to be run directly

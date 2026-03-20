//go:build ignore

/*
Agent Scaffold Script - Create a new agent project with best practices.

Usage:

	go run init_agent.go <agent-name> [-level 0|1] [-path <output-dir>]

Examples:

	go run init_agent.go my-agent                # Level 1 (4 tools)
	go run init_agent.go my-agent -level 0       # Minimal (bash only)
	go run init_agent.go my-agent -path ./bots   # Custom output directory
*/
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Agent templates for each level
var templates = map[int]string{
	0: `package main

/*
Level 0 Agent - Bash is All You Need (~50 lines)

Core insight: One tool (bash) can do everything.
*/

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"learn_claude_code/llm"
)

var system = "You are a coding agent. Use bash for everything:\n- Read: cat, grep, find, ls\n- Write: echo 'content' > file"

var tools = []llm.Tool{{
	Name: "bash", Description: "Execute shell command",
	InputSchema: map[string]any{
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []string{"command"},
	},
}}

func run(ctx context.Context, provider llm.Provider, model string,
	prompt string, history *[]llm.Message) string {
	*history = append(*history, llm.UserMessage(prompt))
	for {
		r, err := provider.Chat(ctx, llm.ChatParams{
			Model: model, System: system, Messages: *history,
			Tools: tools, MaxTokens: 8000,
		})
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		*history = append(*history, llm.AssistantMessage(r))
		if !r.HasToolCalls() {
			return r.Content
		}
		var results []llm.ToolResult
		for _, tc := range r.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
			fmt.Printf("> %s\n", args["command"])
			cmd := exec.Command("bash", "-c", args["command"].(string))
			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))
			if err != nil && output == "" {
				output = fmt.Sprintf("Error: %v", err)
			}
			if output == "" {
				output = "(empty)"
			}
			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output[:min(len(output), 50000)]})
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
	fmt.Println("AGENT_NAME - Level 0 Agent\nType 'q' to quit.\n")
	var history []llm.Message
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(">> ")
		if !scanner.Scan() {
			break
		}
		q := strings.TrimSpace(scanner.Text())
		if q == "" || q == "q" || q == "quit" {
			break
		}
		fmt.Println(run(context.Background(), provider, model, q, &history))
		fmt.Println()
	}
}
`,
	1: `package main

/*
Level 1 Agent - Model as Agent (~200 lines)

Core insight: 4 tools cover 90% of coding tasks.
The model IS the agent. Code just runs the loop.
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

func init() { workdir, _ = os.Getwd() }

var system = fmt.Sprintf("You are a coding agent at %s.\n\nRules:\n- Prefer tools over prose. Act, don't just explain.\n- Never invent file paths. Use ls/find first if unsure.\n- Make minimal changes. Don't over-engineer.\n- After finishing, summarize what changed.", workdir)

var tools = []llm.Tool{
	{Name: "bash", Description: "Run shell command",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		}},
	{Name: "read_file", Description: "Read file contents",
		InputSchema: map[string]any{
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		}},
	{Name: "write_file", Description: "Write content to file",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		}},
	{Name: "edit_file", Description: "Replace exact text in file",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path":     map[string]any{"type": "string"},
				"old_text": map[string]any{"type": "string"},
				"new_text": map[string]any{"type": "string"},
			},
			"required": []string{"path", "old_text", "new_text"},
		}},
}

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

func execute(name string, args map[string]any) string {
	switch name {
	case "bash":
		dangerous := []string{"rm -rf /", "sudo", "shutdown", "> /dev/"}
		cmd := args["command"].(string)
		for _, d := range dangerous {
			if strings.Contains(cmd, d) {
				return "Error: Dangerous command blocked"
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "bash", "-c", cmd)
		c.Dir = workdir
		out, err := c.CombinedOutput()
		if ctx.Err() != nil {
			return "Error: Timeout (60s)"
		}
		result := strings.TrimSpace(string(out))
		if err != nil && result == "" {
			return fmt.Sprintf("Error: %v", err)
		}
		if result == "" {
			return "(empty)"
		}
		return result[:min(len(result), 50000)]
	case "read_file":
		fp, err := safePath(args["path"].(string))
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		s := string(data)
		return s[:min(len(s), 50000)]
	case "write_file":
		fp, err := safePath(args["path"].(string))
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		_ = os.MkdirAll(filepath.Dir(fp), 0o755)
		content := args["content"].(string)
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return fmt.Sprintf("Wrote %d bytes to %s", len(content), args["path"])
	case "edit_file":
		fp, err := safePath(args["path"].(string))
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		content := string(data)
		old := args["old_text"].(string)
		if !strings.Contains(content, old) {
			return fmt.Sprintf("Error: Text not found in %s", args["path"])
		}
		_ = os.WriteFile(fp, []byte(strings.Replace(content, old, args["new_text"].(string), 1)), 0o644)
		return fmt.Sprintf("Edited %s", args["path"])
	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func agent(ctx context.Context, provider llm.Provider, model string,
	prompt string, history *[]llm.Message) string {
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
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &a)
			fmt.Printf("> %s: %v\n", tc.Name, a)
			output := execute(tc.Name, a)
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
	fmt.Printf("AGENT_NAME - Level 1 Agent at %s\nType 'q' to quit.\n\n", workdir)
	var history []llm.Message
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(">> ")
		if !scanner.Scan() {
			break
		}
		q := strings.TrimSpace(scanner.Text())
		if q == "" || q == "q" || q == "quit" || q == "exit" {
			break
		}
		fmt.Println(agent(context.Background(), provider, model, q, &history))
		fmt.Println()
	}
}
`,
}

var envTemplate = `# API Configuration — copy to .env and fill in
# For Anthropic:
#   ANTHROPIC_API_KEY=sk-xxx
# For OpenAI-compatible (Qwen, DeepSeek, etc.):
#   OPENAI_API_KEY=sk-xxx
#   OPENAI_BASE_URL=https://api.deepseek.com
# Model:
#   MODEL_ID=claude-sonnet-4-20250514
`

func createAgent(name string, level int, outputDir string) {
	tmpl, ok := templates[level]
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: Level %d not yet implemented in scaffold.\n", level)
		fmt.Fprintln(os.Stderr, "Available levels: 0 (minimal), 1 (4 tools)")
		fmt.Fprintln(os.Stderr, "For levels 2-4, refer to s02-s05 in the main project.")
		os.Exit(1)
	}

	agentDir := filepath.Join(outputDir, name)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		os.Exit(1)
	}

	// Replace placeholder with agent name
	content := strings.ReplaceAll(tmpl, "AGENT_NAME", name)

	agentFile := filepath.Join(agentDir, "main.go")
	if err := os.WriteFile(agentFile, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created: %s\n", agentFile)

	// go.mod
	goMod := fmt.Sprintf("module %s\n\ngo 1.23\n\nrequire learn_claude_code v0.0.0\n", name)
	goModFile := filepath.Join(agentDir, "go.mod")
	_ = os.WriteFile(goModFile, []byte(goMod), 0o644)
	fmt.Printf("Created: %s\n", goModFile)

	// .env.example
	envFile := filepath.Join(agentDir, ".env.example")
	_ = os.WriteFile(envFile, []byte(envTemplate), 0o644)
	fmt.Printf("Created: %s\n", envFile)

	// .gitignore
	gitignore := filepath.Join(agentDir, ".gitignore")
	_ = os.WriteFile(gitignore, []byte(".env\n"), 0o644)
	fmt.Printf("Created: %s\n", gitignore)

	fmt.Printf("\nAgent '%s' created at %s\n", name, agentDir)
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. cd %s\n", agentDir)
	fmt.Println("  2. cp .env.example .env")
	fmt.Println("  3. Edit .env with your API key")
	fmt.Println("  4. Set up config.yaml or env vars")
	fmt.Printf("  5. go run main.go\n")
}

func main() {
	level := flag.Int("level", 1, "Complexity level: 0 (minimal), 1 (4 tools)")
	path := flag.String("path", ".", "Output directory (default: current directory)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: go run init_agent.go <agent-name> [-level 0|1] [-path <output-dir>]")
		fmt.Fprintln(os.Stderr, "\nLevels:")
		fmt.Fprintln(os.Stderr, "  0  Minimal (~50 lines) - Single bash tool")
		fmt.Fprintln(os.Stderr, "  1  Basic (~200 lines)  - 4 core tools: bash, read, write, edit")
		fmt.Fprintln(os.Stderr, "  2-4  See s02-s05 in the main project for TodoWrite, Subagent, Skills")
		os.Exit(1)
	}

	name := flag.Arg(0)
	createAgent(name, *level, *path)
}

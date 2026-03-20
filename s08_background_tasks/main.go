package main

/*
s08_background_tasks - Background Tasks

Run commands in background goroutines. A notification queue is drained
before each LLM call to deliver results.

    Main goroutine             Background goroutine
    +-----------------+        +-----------------+
    | agent loop      |        | task executes   |
    | ...             |        | ...             |
    | [LLM call] <---+------- | enqueue(result) |
    |  ^drain queue   |        +-----------------+
    +-----------------+

    Timeline:
    Agent ----[spawn A]----[spawn B]----[other work]----
                 |              |
                 v              v
              [A runs]      [B runs]        (parallel)
                 |              |
                 +-- notification queue --> [results injected]

Key insight: "Fire and forget -- the agent doesn't block while the command runs."
*/

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

// =============================================================================
// BackgroundManager: goroutine execution + notification queue
// =============================================================================

type bgTask struct {
	Status  string
	Result  string
	Command string
}

type bgNotification struct {
	TaskID  string
	Status  string
	Command string
	Result  string
}

type BackgroundManager struct {
	mu            sync.Mutex
	tasks         map[string]*bgTask
	notifications []bgNotification
}

func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{tasks: make(map[string]*bgTask)}
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run starts a background goroutine, returns task_id immediately.
func (bm *BackgroundManager) Run(command string) string {
	taskID := shortID()
	bm.mu.Lock()
	bm.tasks[taskID] = &bgTask{Status: "running", Command: command}
	bm.mu.Unlock()

	go bm.execute(taskID, command)

	preview := command
	if len(preview) > 80 {
		preview = preview[:80]
	}
	return fmt.Sprintf("Background task %s started: %s", taskID, preview)
}

func (bm *BackgroundManager) execute(taskID, command string) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()

	var status, output string
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		status, output = "timeout", "Error: Timeout (300s)"
	case err != nil:
		result := strings.TrimSpace(string(out))
		if result == "" {
			status, output = "error", fmt.Sprintf("Error: %v", err)
		} else {
			status, output = "completed", result
		}
	default:
		status = "completed"
		output = strings.TrimSpace(string(out))
		if output == "" {
			output = "(no output)"
		}
	}
	if len(output) > 50000 {
		output = output[:50000]
	}

	preview := command
	if len(preview) > 80 {
		preview = preview[:80]
	}
	resultPreview := output
	if len(resultPreview) > 500 {
		resultPreview = resultPreview[:500]
	}

	bm.mu.Lock()
	bm.tasks[taskID].Status = status
	bm.tasks[taskID].Result = output
	bm.notifications = append(bm.notifications, bgNotification{
		TaskID:  taskID,
		Status:  status,
		Command: preview,
		Result:  resultPreview,
	})
	bm.mu.Unlock()
}

// Check returns status of one task or lists all.
func (bm *BackgroundManager) Check(taskID string) string {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if taskID != "" {
		t, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Error: Unknown task %s", taskID)
		}
		result := t.Result
		if result == "" {
			result = "(running)"
		}
		cmd := t.Command
		if len(cmd) > 60 {
			cmd = cmd[:60]
		}
		return fmt.Sprintf("[%s] %s\n%s", t.Status, cmd, result)
	}

	if len(bm.tasks) == 0 {
		return "No background tasks."
	}
	var lines []string
	for tid, t := range bm.tasks {
		cmd := t.Command
		if len(cmd) > 60 {
			cmd = cmd[:60]
		}
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", tid, t.Status, cmd))
	}
	return strings.Join(lines, "\n")
}

// DrainNotifications returns and clears all pending completion notifications.
func (bm *BackgroundManager) DrainNotifications() []bgNotification {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	notifs := bm.notifications
	bm.notifications = nil
	return notifs
}

// =============================================================================
// Tool implementations
// =============================================================================

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

// =============================================================================
// Dispatch + tool definitions
// =============================================================================

var bg *BackgroundManager

type toolHandler func(args map[string]any) string

var toolHandlers map[string]toolHandler

func initHandlers() {
	bg = NewBackgroundManager()
	toolHandlers = map[string]toolHandler{
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
		"background_run": func(args map[string]any) string {
			return bg.Run(args["command"].(string))
		},
		"check_background": func(args map[string]any) string {
			taskID, _ := args["task_id"].(string)
			return bg.Check(taskID)
		},
	}
}

var tools = []llm.Tool{
	{
		Name: "bash", Description: "Run a shell command (blocking).",
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
		Name: "background_run", Description: "Run command in background goroutine. Returns task_id immediately.",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	},
	{
		Name: "check_background", Description: "Check background task status. Omit task_id to list all.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string"},
			},
		},
	},
}

// =============================================================================
// Agent loop
// =============================================================================

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		// Drain background notifications and inject before LLM call
		if notifs := bg.DrainNotifications(); len(notifs) > 0 {
			var lines []string
			for _, n := range notifs {
				lines = append(lines, fmt.Sprintf("[bg:%s] %s: %s", n.TaskID, n.Status, n.Result))
			}
			notifText := strings.Join(lines, "\n")
			*messages = append(*messages,
				llm.UserMessage(fmt.Sprintf("<background-results>\n%s\n</background-results>", notifText)),
				llm.Message{Role: "assistant", Content: "Noted background results."},
			)
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

			preview := output
			if len(preview) > 200 {
				preview = preview[:200]
			}
			fmt.Printf("> %s: %s\n", tc.Name, preview)

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		*messages = append(*messages, llm.ToolResultsMessage(results))
	}
}

func main() {
	initHandlers()

	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use background_run for long-running commands.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms08 >> \033[0m")
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

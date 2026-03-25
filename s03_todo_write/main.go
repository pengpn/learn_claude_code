package main

/*
s03_todo_write - TodoWrite

The model tracks its own progress via a TodoManager. A nag reminder
forces it to keep updating when it forgets.

    +----------+      +-------+      +---------+
    |   User   | ---> |  LLM  | ---> | Tools   |
    |  prompt  |      |       |      | + todo  |
    +----------+      +---+---+      +----+----+
                          ^               |
                          |   tool_result |
                          +---------------+
                                |
                    +-----------+-----------+
                    | TodoManager state     |
                    | [ ] task A            |
                    | [>] task B <- doing   |
                    | [x] task C            |
                    +-----------------------+
                                |
                    if rounds_since_todo >= 3:
                      inject <reminder>

Key insight: "The agent can track its own progress -- and I can see it."

自测方式：用自然语言描述一个大于3步的任务，然后让模型执行，观察是否能够正确地使用 todo 工具来规划和跟踪进度。
例如输入：1 . 创建demo目录  2 .写main.go 打印hello world 3 .再写readme.md说明用法 4，最后验证编译
每次 LLM 开始新步骤前，都会调用 todo 更新状态。如果它连续 3 轮忘了更新，你会看到系统注入的 <reminder> 让它想起来
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

// -- TodoManager: structured state the LLM writes to --

type TodoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

type TodoManager struct {
	items []TodoItem
}

func normalizeStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "pending", "待办":
		return "pending", true
	case "in_progress", "进行中":
		return "in_progress", true
	case "completed", "已完成":
		return "completed", true
	default:
		return "", false
	}
}

func (t *TodoManager) Update(raw []any) (string, error) {
	if len(raw) > 20 {
		return "", fmt.Errorf("max 20 todos allowed")
	}

	var validated []TodoItem
	inProgressCount := 0

	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			return "", fmt.Errorf("item %d: expected object", i)
		}

		id := fmt.Sprintf("%d", i+1)
		if v, ok := m["id"].(string); ok && v != "" {
			id = v
		}
		text := ""
		if v, ok := m["text"].(string); ok {
			text = strings.TrimSpace(v)
		}
		if text == "" {
			return "", fmt.Errorf("item %s: text required", id)
		}
		rawStatus := ""
		if v, ok := m["status"].(string); ok {
			rawStatus = v
		}
		status, ok := normalizeStatus(rawStatus)
		if !ok {
			return "", fmt.Errorf("item %s: invalid status %q", id, rawStatus)
		}
		if status == "in_progress" {
			inProgressCount++
		}
		validated = append(validated, TodoItem{ID: id, Text: text, Status: status})
	}

	if inProgressCount > 1 {
		return "", fmt.Errorf("only one task can be in_progress at a time")
	}

	t.items = validated
	return t.Render(), nil
}

func (t *TodoManager) Render() string {
	if len(t.items) == 0 {
		return "暂无待办。"
	}
	markers := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}
	labels := map[string]string{"pending": "待办", "in_progress": "进行中", "completed": "已完成"}
	var lines []string
	done := 0
	for _, item := range t.items {
		lines = append(lines, fmt.Sprintf("%s #%s [%s]: %s", markers[item.Status], item.ID, labels[item.Status], item.Text))
		if item.Status == "completed" {
			done++
		}
	}
	lines = append(lines, fmt.Sprintf("\n（已完成 %d/%d）", done, len(t.items)))
	return strings.Join(lines, "\n")
}

var todo = &TodoManager{}

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

// -- The dispatch map --

type toolHandler func(args map[string]any) (string, error)

var toolHandlers = map[string]toolHandler{
	"bash": func(args map[string]any) (string, error) {
		return runBash(args["command"].(string)), nil
	},
	"read_file": func(args map[string]any) (string, error) {
		limit := 0
		if v, ok := args["limit"]; ok {
			if f, ok := v.(float64); ok {
				limit = int(f)
			}
		}
		return runRead(args["path"].(string), limit), nil
	},
	"write_file": func(args map[string]any) (string, error) {
		return runWrite(args["path"].(string), args["content"].(string)), nil
	},
	"edit_file": func(args map[string]any) (string, error) {
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string)), nil
	},
	"todo": func(args map[string]any) (string, error) {
		items, ok := args["items"].([]any)
		if !ok {
			return "", fmt.Errorf("items must be an array")
		}
		return todo.Update(items)
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
		Name: "todo", Description: "Update task list. Track progress on multi-step tasks.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":     map[string]any{"type": "string"},
							"text":   map[string]any{"type": "string"},
							"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "待办", "进行中", "已完成"}},
						},
						"required": []string{"id", "text", "status"},
					},
				},
			},
			"required": []string{"items"},
		},
	},
}

// -- Agent loop with nag reminder injection --

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	roundsSinceTodo := 0

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

		*messages = append(*messages, llm.AssistantMessage(resp))

		if !resp.HasToolCalls() {
			if resp.Content != "" {
				fmt.Println(resp.Content)
			}
			return nil
		}

		var results []llm.ToolResult
		usedTodo := false

		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			handler, ok := toolHandlers[tc.Name]
			var output string
			if !ok {
				output = fmt.Sprintf("Unknown tool: %s", tc.Name)
			} else {
				out, err := handler(args)
				if err != nil {
					output = fmt.Sprintf("Error: %v", err)
				} else {
					output = out
				}
			}

			preview := output
			if len(preview) > 200 {
				preview = preview[:200]
			}
			fmt.Printf("> %s: %s\n", tc.Name, preview)

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
			if tc.Name == "todo" {
				usedTodo = true
			}
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}

		// Nag reminder: embed in the tool results message when the model
		// hasn't updated todos in 3+ rounds. Putting it in the same message
		// keeps the "assistant(tool_calls) → tool results" adjacency that
		// OpenAI-compatible APIs require.
		msg := llm.ToolResultsMessage(results)
		// 如果它连续 3 轮忘了更新，你会看到系统注入的 <reminder> 让它想起来
		if roundsSinceTodo >= 3 {
			msg.Content = "<reminder>Update your todos.</reminder>"
			fmt.Println("[SYSTEM] 注入 reminder：3 轮没更新 todo")  // 加这行显示看看
		}
		*messages = append(*messages, msg)
	}
}

func main() {
	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	system := fmt.Sprintf(`你是一个位于 %s 的编码代理。
使用 todo 工具来规划多步骤任务。开始前将任务标记为进行中（或 in_progress），完成后标记为已完成（或 completed）。
优先使用工具，而不是只用文字说明。`, workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms03 >> \033[0m")
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

package main

/*
s07_task_system - Tasks

Tasks persist as JSON files in .tasks/ so they survive context compression.
Each task has a dependency graph (blockedBy/blocks).

    .tasks/
      task_1.json  {"id":1, "subject":"...", "status":"completed", ...}
      task_2.json  {"id":2, "blockedBy":[1], "status":"pending", ...}
      task_3.json  {"id":3, "blockedBy":[2], "blocks":[], ...}

    Dependency resolution:
    +----------+     +----------+     +----------+
    | task 1   | --> | task 2   | --> | task 3   |
    | complete |     | blocked  |     | blocked  |
    +----------+     +----------+     +----------+
         |                ^
         +--- completing task 1 removes it from task 2's blockedBy

Key insight: "State that survives compression -- because it's outside the conversation."
*/

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

// =============================================================================
// TaskManager: CRUD with dependency graph, persisted as JSON files
// =============================================================================

type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	BlockedBy   []int  `json:"blockedBy"`
	Blocks      []int  `json:"blocks"`
	Owner       string `json:"owner"`
}

type TaskManager struct {
	dir    string
	nextID int
}

func NewTaskManager(dir string) *TaskManager {
	_ = os.MkdirAll(dir, 0o755)
	tm := &TaskManager{dir: dir}
	tm.nextID = tm.maxID() + 1
	return tm
}

func (tm *TaskManager) maxID() int {
	entries, _ := os.ReadDir(tm.dir)
	maxVal := 0
	for _, e := range entries {
		var id int
		if _, err := fmt.Sscanf(e.Name(), "task_%d.json", &id); err == nil {
			if id > maxVal {
				maxVal = id
			}
		}
	}
	return maxVal
}

func (tm *TaskManager) taskPath(id int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", id))
}

func (tm *TaskManager) load(id int) (*Task, error) {
	data, err := os.ReadFile(tm.taskPath(id))
	if err != nil {
		return nil, fmt.Errorf("task %d not found", id)
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (tm *TaskManager) save(t *Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tm.taskPath(t.ID), data, 0o644)
}

func (tm *TaskManager) Create(subject, description string) string {
	t := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Blocks:      []int{},
	}
	tm.nextID++
	if err := tm.save(t); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (tm *TaskManager) Get(taskID int) string {
	t, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (tm *TaskManager) Update(taskID int, status string,
	addBlockedBy, addBlocks []int,
) string {
	t, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	if status != "" {
		switch status {
		case "pending", "in_progress", "completed":
			t.Status = status
			if status == "completed" {
				tm.clearDependency(taskID)
			}
		default:
			return fmt.Sprintf("Error: Invalid status: %s", status)
		}
	}

	if len(addBlockedBy) > 0 {
		t.BlockedBy = uniqueAppend(t.BlockedBy, addBlockedBy...)
	}

	if len(addBlocks) > 0 {
		t.Blocks = uniqueAppend(t.Blocks, addBlocks...)
		// Bidirectional: also update the blocked tasks' blockedBy lists
		for _, blockedID := range addBlocks {
			blocked, err := tm.load(blockedID)
			if err != nil {
				continue
			}
			if !containsInt(blocked.BlockedBy, taskID) {
				blocked.BlockedBy = append(blocked.BlockedBy, taskID)
				_ = tm.save(blocked)
			}
		}
	}

	if err := tm.save(t); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

// clearDependency removes completedID from all other tasks' blockedBy lists.
func (tm *TaskManager) clearDependency(completedID int) {
	entries, _ := os.ReadDir(tm.dir)
	for _, e := range entries {
		var id int
		if _, err := fmt.Sscanf(e.Name(), "task_%d.json", &id); err != nil {
			continue
		}
		t, err := tm.load(id)
		if err != nil {
			continue
		}
		if removeInt(&t.BlockedBy, completedID) {
			_ = tm.save(t)
		}
	}
}

func (tm *TaskManager) ListAll() string {
	entries, _ := os.ReadDir(tm.dir)
	type taskEntry struct {
		id   int
		task *Task
	}
	var tasks []taskEntry
	for _, e := range entries {
		var id int
		if _, err := fmt.Sscanf(e.Name(), "task_%d.json", &id); err != nil {
			continue
		}
		t, err := tm.load(id)
		if err != nil {
			continue
		}
		tasks = append(tasks, taskEntry{id: id, task: t})
	}
	if len(tasks) == 0 {
		return "No tasks."
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].id < tasks[j].id })

	var lines []string
	for _, te := range tasks {
		t := te.task
		marker := map[string]string{
			"pending":     "[ ]",
			"in_progress": "[>]",
			"completed":   "[x]",
		}[t.Status]
		if marker == "" {
			marker = "[?]"
		}
		line := fmt.Sprintf("%s #%d: %s", marker, t.ID, t.Subject)
		if len(t.BlockedBy) > 0 {
			line += fmt.Sprintf(" (blocked by: %v)", t.BlockedBy)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// -- int slice helpers --

func uniqueAppend(slice []int, vals ...int) []int {
	set := map[int]bool{}
	for _, v := range slice {
		set[v] = true
	}
	for _, v := range vals {
		if !set[v] {
			slice = append(slice, v)
			set[v] = true
		}
	}
	return slice
}

func containsInt(slice []int, val int) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
	}
	return false
}

// removeInt removes val from *slice, returns true if found.
func removeInt(slice *[]int, val int) bool {
	for i, v := range *slice {
		if v == val {
			*slice = append((*slice)[:i], (*slice)[i+1:]...)
			return true
		}
	}
	return false
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

var tasks *TaskManager

type toolHandler func(args map[string]any) string

var toolHandlers map[string]toolHandler

func initHandlers() {
	tasks = NewTaskManager(filepath.Join(workdir, ".tasks"))
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
		"task_create": func(args map[string]any) string {
			desc, _ := args["description"].(string)
			return tasks.Create(args["subject"].(string), desc)
		},
		"task_update": func(args map[string]any) string {
			taskID := int(args["task_id"].(float64))
			status, _ := args["status"].(string)
			addBlockedBy := toIntSlice(args["addBlockedBy"])
			addBlocks := toIntSlice(args["addBlocks"])
			return tasks.Update(taskID, status, addBlockedBy, addBlocks)
		},
		"task_list": func(args map[string]any) string {
			return tasks.ListAll()
		},
		"task_get": func(args map[string]any) string {
			return tasks.Get(int(args["task_id"].(float64)))
		},
	}
}

// toIntSlice converts a JSON-decoded []any to []int.
func toIntSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	result := make([]int, 0, len(arr))
	for _, item := range arr {
		if f, ok := item.(float64); ok {
			result = append(result, int(f))
		}
	}
	return result
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
		Name: "task_create", Description: "Create a new task.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"subject":     map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"subject"},
		},
	},
	{
		Name: "task_update", Description: "Update a task's status or dependencies.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"task_id":      map[string]any{"type": "integer"},
				"status":       map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
				"addBlockedBy": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"addBlocks":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			},
			"required": []string{"task_id"},
		},
	},
	{
		Name: "task_list", Description: "List all tasks with status summary.",
		InputSchema: map[string]any{
			"properties": map[string]any{},
		},
	},
	{
		Name: "task_get", Description: "Get full details of a task by ID.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"task_id": map[string]any{"type": "integer"},
			},
			"required": []string{"task_id"},
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

	system := fmt.Sprintf("You are a coding agent at %s. Use task tools to plan and track work.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms07 >> \033[0m")
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

package main

/*
s12_worktree_task_isolation - Worktree + Task Isolation

Directory-level isolation for parallel task execution.
Tasks are the control plane and worktrees are the execution plane.

    .tasks/task_12.json
      {
        "id": 12,
        "subject": "Implement auth refactor",
        "status": "in_progress",
        "worktree": "auth-refactor"
      }

    .worktrees/index.json
      {
        "worktrees": [
          {
            "name": "auth-refactor",
            "path": ".../.worktrees/auth-refactor",
            "branch": "wt/auth-refactor",
            "task_id": 12,
            "status": "active"
          }
        ]
      }

Key insight: "Isolate by directory, coordinate by task ID."
*/

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

// =============================================================================
// Detect git repo root
// =============================================================================

func detectRepoRoot(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return root
	}
	return ""
}

var repoRoot string

// =============================================================================
// EventBus: append-only lifecycle events for observability
// =============================================================================

type EventBus struct {
	path string
}

func NewEventBus(path string) *EventBus {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = os.WriteFile(path, nil, 0o644)
	}
	return &EventBus{path: path}
}

func (eb *EventBus) Emit(event string, task, wt map[string]any, errMsg string) {
	payload := map[string]any{
		"event":    event,
		"ts":       float64(time.Now().UnixMilli()) / 1000.0,
		"task":     task,
		"worktree": wt,
	}
	if payload["task"] == nil {
		payload["task"] = map[string]any{}
	}
	if payload["worktree"] == nil {
		payload["worktree"] = map[string]any{}
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	data, _ := json.Marshal(payload)
	f, err := os.OpenFile(eb.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

func (eb *EventBus) ListRecent(limit int) string {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	data, err := os.ReadFile(eb.path)
	if err != nil {
		return "[]"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	var items []any
	for _, line := range lines {
		if line == "" {
			continue
		}
		var obj any
		if json.Unmarshal([]byte(line), &obj) == nil {
			items = append(items, obj)
		} else {
			items = append(items, map[string]any{"event": "parse_error", "raw": line})
		}
	}
	out, _ := json.MarshalIndent(items, "", "  ")
	return string(out)
}

// =============================================================================
// TaskManager: persistent task board with optional worktree binding
// =============================================================================

type taskData struct {
	ID          int     `json:"id"`
	Subject     string  `json:"subject"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Owner       string  `json:"owner"`
	Worktree    string  `json:"worktree"`
	BlockedBy   []int   `json:"blockedBy"`
	CreatedAt   float64 `json:"created_at"`
	UpdatedAt   float64 `json:"updated_at"`
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
		name := e.Name()
		if strings.HasPrefix(name, "task_") && strings.HasSuffix(name, ".json") {
			idStr := strings.TrimSuffix(strings.TrimPrefix(name, "task_"), ".json")
			if v, err := strconv.Atoi(idStr); err == nil && v > maxVal {
				maxVal = v
			}
		}
	}
	return maxVal
}

func (tm *TaskManager) taskPath(id int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", id))
}

func (tm *TaskManager) load(id int) (*taskData, error) {
	data, err := os.ReadFile(tm.taskPath(id))
	if err != nil {
		return nil, fmt.Errorf("Task %d not found", id)
	}
	var t taskData
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("Task %d corrupt", id)
	}
	return &t, nil
}

func (tm *TaskManager) save(t *taskData) {
	data, _ := json.MarshalIndent(t, "", "  ")
	_ = os.WriteFile(tm.taskPath(t.ID), data, 0o644)
}

func nowTS() float64 {
	return float64(time.Now().UnixMilli()) / 1000.0
}

func (tm *TaskManager) Create(subject, description string) string {
	t := &taskData{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		CreatedAt:   nowTS(),
		UpdatedAt:   nowTS(),
	}
	tm.save(t)
	tm.nextID++
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out)
}

func (tm *TaskManager) Get(id int) string {
	t, err := tm.load(id)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out)
}

func (tm *TaskManager) Exists(id int) bool {
	_, err := os.Stat(tm.taskPath(id))
	return err == nil
}

func (tm *TaskManager) Update(id int, status, owner string) string {
	t, err := tm.load(id)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if status != "" {
		switch status {
		case "pending", "in_progress", "completed":
			t.Status = status
		default:
			return fmt.Sprintf("Error: Invalid status: %s", status)
		}
	}
	if owner != "" {
		t.Owner = owner
	}
	t.UpdatedAt = nowTS()
	tm.save(t)
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out)
}

func (tm *TaskManager) BindWorktree(id int, worktree, owner string) string {
	t, err := tm.load(id)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	t.Worktree = worktree
	if owner != "" {
		t.Owner = owner
	}
	if t.Status == "pending" {
		t.Status = "in_progress"
	}
	t.UpdatedAt = nowTS()
	tm.save(t)
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out)
}

func (tm *TaskManager) UnbindWorktree(id int) string {
	t, err := tm.load(id)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	t.Worktree = ""
	t.UpdatedAt = nowTS()
	tm.save(t)
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out)
}

func (tm *TaskManager) ListAll() string {
	entries, _ := os.ReadDir(tm.dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "task_") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	if len(names) == 0 {
		return "No tasks."
	}

	var lines []string
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(tm.dir, name))
		if err != nil {
			continue
		}
		var t taskData
		if json.Unmarshal(data, &t) != nil {
			continue
		}
		marker := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}[t.Status]
		if marker == "" {
			marker = "[?]"
		}
		owner := ""
		if t.Owner != "" {
			owner = " owner=" + t.Owner
		}
		wt := ""
		if t.Worktree != "" {
			wt = " wt=" + t.Worktree
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", marker, t.ID, t.Subject, owner, wt))
	}
	return strings.Join(lines, "\n")
}

// =============================================================================
// WorktreeManager: create/list/run/remove git worktrees + lifecycle index
// =============================================================================

type worktreeEntry struct {
	Name      string  `json:"name"`
	Path      string  `json:"path"`
	Branch    string  `json:"branch"`
	TaskID    *int    `json:"task_id"`
	Status    string  `json:"status"`
	CreatedAt float64 `json:"created_at,omitempty"`
	RemovedAt float64 `json:"removed_at,omitempty"`
	KeptAt    float64 `json:"kept_at,omitempty"`
}

type worktreeIndex struct {
	Worktrees []worktreeEntry `json:"worktrees"`
}

type WorktreeManager struct {
	repoRoot     string
	tasks        *TaskManager
	events       *EventBus
	dir          string
	indexPath    string
	gitAvailable bool
}

var worktreeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)

func NewWorktreeManager(root string, tasks *TaskManager, events *EventBus) *WorktreeManager {
	dir := filepath.Join(root, ".worktrees")
	_ = os.MkdirAll(dir, 0o755)
	indexPath := filepath.Join(dir, "index.json")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		data, _ := json.MarshalIndent(worktreeIndex{Worktrees: []worktreeEntry{}}, "", "  ")
		_ = os.WriteFile(indexPath, data, 0o644)
	}
	wm := &WorktreeManager{
		repoRoot:  root,
		tasks:     tasks,
		events:    events,
		dir:       dir,
		indexPath: indexPath,
	}
	wm.gitAvailable = wm.isGitRepo()
	return wm
}

func (wm *WorktreeManager) isGitRepo() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = wm.repoRoot
	err := cmd.Run()
	return err == nil
}

func (wm *WorktreeManager) runGit(args ...string) (string, error) {
	if !wm.gitAvailable {
		return "", fmt.Errorf("not in a git repository; worktree tools require git")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wm.repoRoot
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result != "" {
			return "", fmt.Errorf("%s", result)
		}
		return "", fmt.Errorf("git %s failed: %v", strings.Join(args, " "), err)
	}
	if result == "" {
		return "(no output)", nil
	}
	return result, nil
}

func (wm *WorktreeManager) loadIndex() worktreeIndex {
	data, err := os.ReadFile(wm.indexPath)
	if err != nil {
		return worktreeIndex{}
	}
	var idx worktreeIndex
	_ = json.Unmarshal(data, &idx)
	return idx
}

func (wm *WorktreeManager) saveIndex(idx worktreeIndex) {
	data, _ := json.MarshalIndent(idx, "", "  ")
	_ = os.WriteFile(wm.indexPath, data, 0o644)
}

func (wm *WorktreeManager) find(name string) *worktreeEntry {
	idx := wm.loadIndex()
	for i := range idx.Worktrees {
		if idx.Worktrees[i].Name == name {
			return &idx.Worktrees[i]
		}
	}
	return nil
}

func (wm *WorktreeManager) validateName(name string) error {
	if !worktreeNameRe.MatchString(name) {
		return fmt.Errorf("invalid worktree name; use 1-40 chars: letters, numbers, ., _, -")
	}
	return nil
}

func taskRef(id *int) map[string]any {
	if id != nil {
		return map[string]any{"id": *id}
	}
	return map[string]any{}
}

func intPtr(v int) *int { return &v }

func (wm *WorktreeManager) Create(name string, taskID *int, baseRef string) string {
	if err := wm.validateName(name); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if wm.find(name) != nil {
		return fmt.Sprintf("Error: Worktree '%s' already exists in index", name)
	}
	if taskID != nil && !wm.tasks.Exists(*taskID) {
		return fmt.Sprintf("Error: Task %d not found", *taskID)
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}

	path := filepath.Join(wm.dir, name)
	branch := "wt/" + name

	wm.events.Emit("worktree.create.before",
		taskRef(taskID),
		map[string]any{"name": name, "base_ref": baseRef}, "")

	_, err := wm.runGit("worktree", "add", "-b", branch, path, baseRef)
	if err != nil {
		wm.events.Emit("worktree.create.failed",
			taskRef(taskID),
			map[string]any{"name": name, "base_ref": baseRef}, err.Error())
		return fmt.Sprintf("Error: %v", err)
	}

	entry := worktreeEntry{
		Name:      name,
		Path:      path,
		Branch:    branch,
		TaskID:    taskID,
		Status:    "active",
		CreatedAt: nowTS(),
	}

	idx := wm.loadIndex()
	idx.Worktrees = append(idx.Worktrees, entry)
	wm.saveIndex(idx)

	if taskID != nil {
		wm.tasks.BindWorktree(*taskID, name, "")
	}

	wm.events.Emit("worktree.create.after",
		taskRef(taskID),
		map[string]any{"name": name, "path": path, "branch": branch, "status": "active"}, "")

	out, _ := json.MarshalIndent(entry, "", "  ")
	return string(out)
}

func (wm *WorktreeManager) ListAll() string {
	idx := wm.loadIndex()
	if len(idx.Worktrees) == 0 {
		return "No worktrees in index."
	}
	var lines []string
	for _, wt := range idx.Worktrees {
		suffix := ""
		if wt.TaskID != nil {
			suffix = fmt.Sprintf(" task=%d", *wt.TaskID)
		}
		branch := wt.Branch
		if branch == "" {
			branch = "-"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s -> %s (%s)%s",
			wt.Status, wt.Name, wt.Path, branch, suffix))
	}
	return strings.Join(lines, "\n")
}

func (wm *WorktreeManager) Status(name string) string {
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}
	if info, err := os.Stat(wt.Path); err != nil || !info.IsDir() {
		return fmt.Sprintf("Error: Worktree path missing: %s", wt.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--short", "--branch")
	cmd.Dir = wt.Path
	out, _ := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if result == "" {
		return "Clean worktree"
	}
	return result
}

var dangerousPatterns = []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}

func (wm *WorktreeManager) Run(name, command string) string {
	for _, d := range dangerousPatterns {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}
	if info, err := os.Stat(wt.Path); err != nil || !info.IsDir() {
		return fmt.Sprintf("Error: Worktree path missing: %s", wt.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = wt.Path
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (300s)"
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

func (wm *WorktreeManager) Remove(name string, force, completeTask bool) string {
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}

	wm.events.Emit("worktree.remove.before",
		taskRef(wt.TaskID),
		map[string]any{"name": name, "path": wt.Path}, "")

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)

	if _, err := wm.runGit(args...); err != nil {
		wm.events.Emit("worktree.remove.failed",
			taskRef(wt.TaskID),
			map[string]any{"name": name, "path": wt.Path}, err.Error())
		return fmt.Sprintf("Error: %v", err)
	}

	if completeTask && wt.TaskID != nil {
		taskID := *wt.TaskID
		taskJSON := wm.tasks.Get(taskID)
		var before taskData
		_ = json.Unmarshal([]byte(taskJSON), &before)
		wm.tasks.Update(taskID, "completed", "")
		wm.tasks.UnbindWorktree(taskID)
		wm.events.Emit("task.completed",
			map[string]any{"id": taskID, "subject": before.Subject, "status": "completed"},
			map[string]any{"name": name}, "")
	}

	idx := wm.loadIndex()
	for i := range idx.Worktrees {
		if idx.Worktrees[i].Name == name {
			idx.Worktrees[i].Status = "removed"
			idx.Worktrees[i].RemovedAt = nowTS()
		}
	}
	wm.saveIndex(idx)

	wm.events.Emit("worktree.remove.after",
		taskRef(wt.TaskID),
		map[string]any{"name": name, "path": wt.Path, "status": "removed"}, "")

	return fmt.Sprintf("Removed worktree '%s'", name)
}

func (wm *WorktreeManager) Keep(name string) string {
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}

	idx := wm.loadIndex()
	var kept *worktreeEntry
	for i := range idx.Worktrees {
		if idx.Worktrees[i].Name == name {
			idx.Worktrees[i].Status = "kept"
			idx.Worktrees[i].KeptAt = nowTS()
			kept = &idx.Worktrees[i]
		}
	}
	wm.saveIndex(idx)

	wm.events.Emit("worktree.keep",
		taskRef(wt.TaskID),
		map[string]any{"name": name, "path": wt.Path, "status": "kept"}, "")

	if kept != nil {
		out, _ := json.MarshalIndent(kept, "", "  ")
		return string(out)
	}
	return fmt.Sprintf("Error: Unknown worktree '%s'", name)
}

// =============================================================================
// Base tool implementations
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
// Tool dispatch + definitions (16 tools)
// =============================================================================

var (
	tasks     *TaskManager
	events    *EventBus
	worktrees *WorktreeManager
)

type toolHandler func(args map[string]any) string

var toolHandlers map[string]toolHandler

func initHandlers() {
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
		"task_list": func(args map[string]any) string {
			return tasks.ListAll()
		},
		"task_get": func(args map[string]any) string {
			return tasks.Get(int(args["task_id"].(float64)))
		},
		"task_update": func(args map[string]any) string {
			status, _ := args["status"].(string)
			owner, _ := args["owner"].(string)
			return tasks.Update(int(args["task_id"].(float64)), status, owner)
		},
		"task_bind_worktree": func(args map[string]any) string {
			owner, _ := args["owner"].(string)
			return tasks.BindWorktree(int(args["task_id"].(float64)), args["worktree"].(string), owner)
		},
		"worktree_create": func(args map[string]any) string {
			var taskID *int
			if v, ok := args["task_id"]; ok {
				if f, ok := v.(float64); ok {
					taskID = intPtr(int(f))
				}
			}
			baseRef, _ := args["base_ref"].(string)
			return worktrees.Create(args["name"].(string), taskID, baseRef)
		},
		"worktree_list": func(args map[string]any) string {
			return worktrees.ListAll()
		},
		"worktree_status": func(args map[string]any) string {
			return worktrees.Status(args["name"].(string))
		},
		"worktree_run": func(args map[string]any) string {
			return worktrees.Run(args["name"].(string), args["command"].(string))
		},
		"worktree_keep": func(args map[string]any) string {
			return worktrees.Keep(args["name"].(string))
		},
		"worktree_remove": func(args map[string]any) string {
			force, _ := args["force"].(bool)
			completeTask, _ := args["complete_task"].(bool)
			return worktrees.Remove(args["name"].(string), force, completeTask)
		},
		"worktree_events": func(args map[string]any) string {
			limit := 20
			if v, ok := args["limit"]; ok {
				if f, ok := v.(float64); ok {
					limit = int(f)
				}
			}
			return events.ListRecent(limit)
		},
	}
}

var tools = []llm.Tool{
	{
		Name: "bash", Description: "Run a shell command in the current workspace (blocking).",
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
		Name: "task_create", Description: "Create a new task on the shared task board.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"subject":     map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"subject"},
		},
	},
	{
		Name: "task_list", Description: "List all tasks with status, owner, and worktree binding.",
		InputSchema: map[string]any{"properties": map[string]any{}},
	},
	{
		Name: "task_get", Description: "Get task details by ID.",
		InputSchema: map[string]any{
			"properties": map[string]any{"task_id": map[string]any{"type": "integer"}},
			"required":   []string{"task_id"},
		},
	},
	{
		Name: "task_update", Description: "Update task status or owner.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"task_id": map[string]any{"type": "integer"},
				"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
				"owner":   map[string]any{"type": "string"},
			},
			"required": []string{"task_id"},
		},
	},
	{
		Name: "task_bind_worktree", Description: "Bind a task to a worktree name.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"task_id":  map[string]any{"type": "integer"},
				"worktree": map[string]any{"type": "string"},
				"owner":    map[string]any{"type": "string"},
			},
			"required": []string{"task_id", "worktree"},
		},
	},
	{
		Name: "worktree_create", Description: "Create a git worktree and optionally bind it to a task.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"name":     map[string]any{"type": "string"},
				"task_id":  map[string]any{"type": "integer"},
				"base_ref": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name: "worktree_list", Description: "List worktrees tracked in .worktrees/index.json.",
		InputSchema: map[string]any{"properties": map[string]any{}},
	},
	{
		Name: "worktree_status", Description: "Show git status for one worktree.",
		InputSchema: map[string]any{
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
	},
	{
		Name: "worktree_run", Description: "Run a shell command in a named worktree directory.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"name":    map[string]any{"type": "string"},
				"command": map[string]any{"type": "string"},
			},
			"required": []string{"name", "command"},
		},
	},
	{
		Name: "worktree_remove", Description: "Remove a worktree and optionally mark its bound task completed.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"name":          map[string]any{"type": "string"},
				"force":         map[string]any{"type": "boolean"},
				"complete_task": map[string]any{"type": "boolean"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name: "worktree_keep", Description: "Mark a worktree as kept in lifecycle state without removing it.",
		InputSchema: map[string]any{
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
	},
	{
		Name: "worktree_events", Description: "List recent worktree/task lifecycle events from .worktrees/events.jsonl.",
		InputSchema: map[string]any{
			"properties": map[string]any{"limit": map[string]any{"type": "integer"}},
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
			Model: model, System: system, Messages: *messages,
			Tools: tools, MaxTokens: 8000,
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
				func() {
					defer func() {
						if r := recover(); r != nil {
							output = fmt.Sprintf("Error: %v", r)
						}
					}()
					output = handler(args)
				}()
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
	repoRoot = detectRepoRoot(workdir)
	if repoRoot == "" {
		repoRoot = workdir
	}

	tasks = NewTaskManager(filepath.Join(repoRoot, ".tasks"))
	events = NewEventBus(filepath.Join(repoRoot, ".worktrees", "events.jsonl"))
	worktrees = NewWorktreeManager(repoRoot, tasks, events)

	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	initHandlers()

	system := fmt.Sprintf(
		"You are a coding agent at %s. "+
			"Use task + worktree tools for multi-task work. "+
			"For parallel or risky changes: create tasks, allocate worktree lanes, "+
			"run commands in those lanes, then choose keep/remove for closeout. "+
			"Use worktree_events when you need lifecycle visibility.",
		workdir)

	fmt.Printf("Repo root for s12: %s\n", repoRoot)
	if !worktrees.gitAvailable {
		fmt.Println("Note: Not in a git repo. worktree_* tools will return errors.")
	}

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms12 >> \033[0m")
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

package main

/*
s11_autonomous_agents - Autonomous Agents

Idle cycle with task board polling, auto-claiming unclaimed tasks, and
identity re-injection after context compression. Builds on s10's protocols.

    Teammate lifecycle:
    +-------+
    | spawn |
    +---+---+
        |
        v
    +-------+  tool_use    +-------+
    | WORK  | <----------- |  LLM  |
    +---+---+              +-------+
        |
        | stop_reason != tool_use
        v
    +--------+
    | IDLE   | poll every 5s for up to 60s
    +---+----+
        |
        +---> check inbox -> message? -> resume WORK
        |
        +---> scan .tasks/ -> unclaimed? -> claim -> resume WORK
        |
        +---> timeout (60s) -> shutdown

    Identity re-injection after compression:
    messages = [identity_block, ...remaining...]
    "You are 'coder', role: backend, team: my-team"

Key insight: "The agent finds work itself."
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
	"sort"
	"strings"
	"sync"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

const (
	pollInterval = 5 * time.Second
	idleTimeout  = 60 * time.Second
)

var validMsgTypes = map[string]bool{
	"message":                true,
	"broadcast":              true,
	"shutdown_request":       true,
	"shutdown_response":      true,
	"plan_approval_response": true,
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// =============================================================================
// Request trackers
// =============================================================================

type shutdownEntry struct {
	Target string `json:"target"`
	Status string `json:"status"`
}

type planEntry struct {
	From   string `json:"from"`
	Plan   string `json:"plan"`
	Status string `json:"status"`
}

var (
	shutdownRequests = map[string]*shutdownEntry{}
	planRequests     = map[string]*planEntry{}
	trackerMu        sync.Mutex
)

// =============================================================================
// MessageBus
// =============================================================================

type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewMessageBus(dir string) *MessageBus {
	_ = os.MkdirAll(dir, 0o755)
	return &MessageBus{dir: dir}
}

func (mb *MessageBus) Send(sender, to, content, msgType string, extra map[string]any) string {
	if msgType == "" {
		msgType = "message"
	}
	if !validMsgTypes[msgType] {
		return fmt.Sprintf("Error: Invalid type '%s'", msgType)
	}
	msg := map[string]any{
		"type":      msgType,
		"from":      sender,
		"content":   content,
		"timestamp": float64(time.Now().UnixMilli()) / 1000.0,
	}
	for k, v := range extra {
		msg[k] = v
	}
	data, _ := json.Marshal(msg)

	mb.mu.Lock()
	defer mb.mu.Unlock()

	f, err := os.OpenFile(filepath.Join(mb.dir, to+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer f.Close()
	f.Write(append(data, '\n'))
	return fmt.Sprintf("Sent %s to %s", msgType, to)
}

func (mb *MessageBus) ReadInbox(name string) []map[string]any {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	path := filepath.Join(mb.dir, name+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) == nil {
			messages = append(messages, msg)
		}
	}
	_ = os.WriteFile(path, nil, 0o644)
	return messages
}

func (mb *MessageBus) Broadcast(sender, content string, teammates []string) string {
	count := 0
	for _, name := range teammates {
		if name != sender {
			mb.Send(sender, name, content, "broadcast", nil)
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count)
}

// =============================================================================
// Task board scanning
// =============================================================================

var (
	tasksDir string
	claimMu  sync.Mutex
)

type taskFile struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Owner       string `json:"owner"`
	BlockedBy   []int  `json:"blockedBy"`
}

func scanUnclaimedTasks() []taskFile {
	_ = os.MkdirAll(tasksDir, 0o755)
	entries, _ := os.ReadDir(tasksDir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "task_") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var unclaimed []taskFile
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(tasksDir, name))
		if err != nil {
			continue
		}
		var t taskFile
		if json.Unmarshal(data, &t) != nil {
			continue
		}
		if t.Status == "pending" && t.Owner == "" && len(t.BlockedBy) == 0 {
			unclaimed = append(unclaimed, t)
		}
	}
	return unclaimed
}

func claimTask(taskID int, owner string) string {
	claimMu.Lock()
	defer claimMu.Unlock()

	path := filepath.Join(tasksDir, fmt.Sprintf("task_%d.json", taskID))
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error: Task %d not found", taskID)
	}
	var t taskFile
	if json.Unmarshal(data, &t) != nil {
		return fmt.Sprintf("Error: Task %d corrupt", taskID)
	}
	t.Owner = owner
	t.Status = "in_progress"
	out, _ := json.MarshalIndent(t, "", "  ")
	_ = os.WriteFile(path, out, 0o644)
	return fmt.Sprintf("Claimed task #%d for %s", taskID, owner)
}

// =============================================================================
// Autonomous TeammateManager
// =============================================================================

type memberConfig struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type teamConfig struct {
	TeamName string         `json:"team_name"`
	Members  []memberConfig `json:"members"`
}

type TeammateManager struct {
	dir        string
	configPath string
	config     teamConfig
	mu         sync.Mutex
	provider   llm.Provider
	model      string
}

func NewTeammateManager(dir string, provider llm.Provider, model string) *TeammateManager {
	_ = os.MkdirAll(dir, 0o755)
	tm := &TeammateManager{
		dir:        dir,
		configPath: filepath.Join(dir, "config.json"),
		provider:   provider,
		model:      model,
	}
	tm.config = tm.loadConfig()
	return tm
}

func (tm *TeammateManager) loadConfig() teamConfig {
	data, err := os.ReadFile(tm.configPath)
	if err != nil {
		return teamConfig{TeamName: "default"}
	}
	var cfg teamConfig
	if json.Unmarshal(data, &cfg) != nil {
		return teamConfig{TeamName: "default"}
	}
	return cfg
}

func (tm *TeammateManager) saveConfig() {
	data, _ := json.MarshalIndent(tm.config, "", "  ")
	_ = os.WriteFile(tm.configPath, data, 0o644)
}

func (tm *TeammateManager) findMember(name string) *memberConfig {
	for i := range tm.config.Members {
		if tm.config.Members[i].Name == name {
			return &tm.config.Members[i]
		}
	}
	return nil
}

func (tm *TeammateManager) setStatus(name, status string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if m := tm.findMember(name); m != nil {
		m.Status = status
		tm.saveConfig()
	}
}

func (tm *TeammateManager) Spawn(name, role, prompt string) string {
	tm.mu.Lock()
	member := tm.findMember(name)
	if member != nil {
		if member.Status != "idle" && member.Status != "shutdown" {
			tm.mu.Unlock()
			return fmt.Sprintf("Error: '%s' is currently %s", name, member.Status)
		}
		member.Status = "working"
		member.Role = role
	} else {
		tm.config.Members = append(tm.config.Members, memberConfig{
			Name: name, Role: role, Status: "working",
		})
	}
	tm.saveConfig()
	tm.mu.Unlock()

	go tm.loop(name, role, prompt)
	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

func (tm *TeammateManager) loop(name, role, prompt string) {
	tm.mu.Lock()
	teamName := tm.config.TeamName
	tm.mu.Unlock()

	sysPrompt := fmt.Sprintf(
		"You are '%s', role: %s, team: %s, at %s. "+
			"Use idle tool when you have no more work. You will auto-claim new tasks.",
		name, role, teamName, workdir,
	)

	messages := []llm.Message{llm.UserMessage(prompt)}
	tools := teammateTools()

	for {
		// -- WORK PHASE --
		idleRequested := false
		for range 50 {
			// Check inbox
			inbox := bus.ReadInbox(name)
			for _, msg := range inbox {
				if msgType, _ := msg["type"].(string); msgType == "shutdown_request" {
					tm.setStatus(name, "shutdown")
					return
				}
				data, _ := json.Marshal(msg)
				messages = append(messages, llm.UserMessage(string(data)))
			}

			resp, err := tm.provider.Chat(context.Background(), llm.ChatParams{
				Model: tm.model, System: sysPrompt, Messages: messages,
				Tools: tools, MaxTokens: 8000,
			})
			if err != nil {
				tm.setStatus(name, "idle")
				return
			}

			messages = append(messages, llm.AssistantMessage(resp))
			if !resp.HasToolCalls() {
				break
			}

			var results []llm.ToolResult
			for _, tc := range resp.ToolCalls {
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Arguments), &args)

				var output string
				if tc.Name == "idle" {
					idleRequested = true
					output = "Entering idle phase. Will poll for new tasks."
				} else {
					output = execTeammateTool(name, tc.Name, args)
				}

				preview := output
				if len(preview) > 120 {
					preview = preview[:120]
				}
				fmt.Printf("  [%s] %s: %s\n", name, tc.Name, preview)
				results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
			}
			messages = append(messages, llm.ToolResultsMessage(results))
			if idleRequested {
				break
			}
		}

		// -- IDLE PHASE: poll for inbox messages and unclaimed tasks --
		tm.setStatus(name, "idle")
		resumed := false
		polls := int(idleTimeout / pollInterval)

		for range polls {
			time.Sleep(pollInterval)

			// Check inbox
			inbox := bus.ReadInbox(name)
			if len(inbox) > 0 {
				for _, msg := range inbox {
					if msgType, _ := msg["type"].(string); msgType == "shutdown_request" {
						tm.setStatus(name, "shutdown")
						return
					}
					data, _ := json.Marshal(msg)
					messages = append(messages, llm.UserMessage(string(data)))
				}
				resumed = true
				break
			}

			// Scan task board for unclaimed tasks
			unclaimed := scanUnclaimedTasks()
			if len(unclaimed) > 0 {
				task := unclaimed[0]
				claimTask(task.ID, name)
				taskPrompt := fmt.Sprintf("<auto-claimed>Task #%d: %s\n%s</auto-claimed>",
					task.ID, task.Subject, task.Description)

				// Identity re-injection if context was compressed
				if len(messages) <= 3 {
					identity := llm.UserMessage(fmt.Sprintf(
						"<identity>You are '%s', role: %s, team: %s. Continue your work.</identity>",
						name, role, teamName))
					ack := llm.Message{Role: "assistant", Content: fmt.Sprintf("I am %s. Continuing.", name)}
					messages = append([]llm.Message{identity, ack}, messages...)
				}

				messages = append(messages,
					llm.UserMessage(taskPrompt),
					llm.Message{Role: "assistant", Content: fmt.Sprintf("Claimed task #%d. Working on it.", task.ID)},
				)
				resumed = true
				break
			}
		}

		if !resumed {
			tm.setStatus(name, "shutdown")
			return
		}
		tm.setStatus(name, "working")
	}
}

func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.config.Members) == 0 {
		return "No teammates."
	}
	lines := []string{fmt.Sprintf("Team: %s", tm.config.TeamName)}
	for _, m := range tm.config.Members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}
	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var names []string
	for _, m := range tm.config.Members {
		names = append(names, m.Name)
	}
	return names
}

// =============================================================================
// Teammate tool execution
// =============================================================================

func execTeammateTool(sender, toolName string, args map[string]any) string {
	switch toolName {
	case "bash":
		return runBash(args["command"].(string))
	case "read_file":
		return runRead(args["path"].(string), 0)
	case "write_file":
		return runWrite(args["path"].(string), args["content"].(string))
	case "edit_file":
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	case "send_message":
		msgType, _ := args["msg_type"].(string)
		return bus.Send(sender, args["to"].(string), args["content"].(string), msgType, nil)
	case "read_inbox":
		msgs := bus.ReadInbox(sender)
		data, _ := json.MarshalIndent(msgs, "", "  ")
		return string(data)
	case "shutdown_response":
		reqID, _ := args["request_id"].(string)
		approve, _ := args["approve"].(bool)
		reason, _ := args["reason"].(string)
		trackerMu.Lock()
		if e, ok := shutdownRequests[reqID]; ok {
			if approve {
				e.Status = "approved"
			} else {
				e.Status = "rejected"
			}
		}
		trackerMu.Unlock()
		bus.Send(sender, "lead", reason, "shutdown_response",
			map[string]any{"request_id": reqID, "approve": approve})
		if approve {
			return "Shutdown approved"
		}
		return "Shutdown rejected"
	case "plan_approval":
		planText, _ := args["plan"].(string)
		reqID := shortID()
		trackerMu.Lock()
		planRequests[reqID] = &planEntry{From: sender, Plan: planText, Status: "pending"}
		trackerMu.Unlock()
		bus.Send(sender, "lead", planText, "plan_approval_response",
			map[string]any{"request_id": reqID, "plan": planText})
		return fmt.Sprintf("Plan submitted (request_id=%s). Waiting for approval.", reqID)
	case "claim_task":
		return claimTask(int(args["task_id"].(float64)), sender)
	default:
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}
}

func teammateTools() []llm.Tool {
	msgTypes := make([]string, 0, len(validMsgTypes))
	for t := range validMsgTypes {
		msgTypes = append(msgTypes, t)
	}
	return []llm.Tool{
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
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
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
			Name: "send_message", Description: "Send message to a teammate.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"to":       map[string]any{"type": "string"},
					"content":  map[string]any{"type": "string"},
					"msg_type": map[string]any{"type": "string", "enum": msgTypes},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name: "read_inbox", Description: "Read and drain your inbox.",
			InputSchema: map[string]any{"properties": map[string]any{}},
		},
		{
			Name: "shutdown_response", Description: "Respond to a shutdown request.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"approve":    map[string]any{"type": "boolean"},
					"reason":     map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "approve"},
			},
		},
		{
			Name: "plan_approval", Description: "Submit a plan for lead approval.",
			InputSchema: map[string]any{
				"properties": map[string]any{"plan": map[string]any{"type": "string"}},
				"required":   []string{"plan"},
			},
		},
		{
			Name: "idle", Description: "Signal that you have no more work. Enters idle polling phase.",
			InputSchema: map[string]any{"properties": map[string]any{}},
		},
		{
			Name: "claim_task", Description: "Claim a task from the task board by ID.",
			InputSchema: map[string]any{
				"properties": map[string]any{"task_id": map[string]any{"type": "integer"}},
				"required":   []string{"task_id"},
			},
		},
	}
}

// =============================================================================
// Lead-specific protocol handlers
// =============================================================================

func handleShutdownRequest(teammate string) string {
	reqID := shortID()
	trackerMu.Lock()
	shutdownRequests[reqID] = &shutdownEntry{Target: teammate, Status: "pending"}
	trackerMu.Unlock()
	bus.Send("lead", teammate, "Please shut down gracefully.", "shutdown_request",
		map[string]any{"request_id": reqID})
	return fmt.Sprintf("Shutdown request %s sent to '%s'", reqID, teammate)
}

func checkShutdownStatus(requestID string) string {
	trackerMu.Lock()
	entry, ok := shutdownRequests[requestID]
	trackerMu.Unlock()
	if !ok {
		return `{"error": "not found"}`
	}
	data, _ := json.Marshal(entry)
	return string(data)
}

func handlePlanReview(requestID string, approve bool, feedback string) string {
	trackerMu.Lock()
	req, ok := planRequests[requestID]
	if !ok {
		trackerMu.Unlock()
		return fmt.Sprintf("Error: Unknown plan request_id '%s'", requestID)
	}
	if approve {
		req.Status = "approved"
	} else {
		req.Status = "rejected"
	}
	from := req.From
	trackerMu.Unlock()

	bus.Send("lead", from, feedback, "plan_approval_response",
		map[string]any{"request_id": requestID, "approve": approve, "feedback": feedback})
	return fmt.Sprintf("Plan %s for '%s'", req.Status, from)
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
// Lead agent dispatch + tool definitions (14 tools)
// =============================================================================

var (
	bus  *MessageBus
	team *TeammateManager
)

type toolHandler func(args map[string]any) string

var toolHandlers map[string]toolHandler

func initHandlers(provider llm.Provider, model string) {
	bus = NewMessageBus(filepath.Join(workdir, ".team", "inbox"))
	team = NewTeammateManager(filepath.Join(workdir, ".team"), provider, model)

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
		"spawn_teammate": func(args map[string]any) string {
			return team.Spawn(args["name"].(string), args["role"].(string), args["prompt"].(string))
		},
		"list_teammates": func(args map[string]any) string {
			return team.ListAll()
		},
		"send_message": func(args map[string]any) string {
			msgType, _ := args["msg_type"].(string)
			return bus.Send("lead", args["to"].(string), args["content"].(string), msgType, nil)
		},
		"read_inbox": func(args map[string]any) string {
			msgs := bus.ReadInbox("lead")
			data, _ := json.MarshalIndent(msgs, "", "  ")
			return string(data)
		},
		"broadcast": func(args map[string]any) string {
			return bus.Broadcast("lead", args["content"].(string), team.MemberNames())
		},
		"shutdown_request": func(args map[string]any) string {
			return handleShutdownRequest(args["teammate"].(string))
		},
		"shutdown_status": func(args map[string]any) string {
			return checkShutdownStatus(args["request_id"].(string))
		},
		"plan_approval": func(args map[string]any) string {
			approve, _ := args["approve"].(bool)
			feedback, _ := args["feedback"].(string)
			return handlePlanReview(args["request_id"].(string), approve, feedback)
		},
		"idle": func(args map[string]any) string {
			return "Lead does not idle."
		},
		"claim_task": func(args map[string]any) string {
			return claimTask(int(args["task_id"].(float64)), "lead")
		},
	}
}

var tools []llm.Tool

func initTools() {
	msgTypes := make([]string, 0, len(validMsgTypes))
	for t := range validMsgTypes {
		msgTypes = append(msgTypes, t)
	}
	tools = []llm.Tool{
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
			Name: "spawn_teammate", Description: "Spawn an autonomous teammate.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"name":   map[string]any{"type": "string"},
					"role":   map[string]any{"type": "string"},
					"prompt": map[string]any{"type": "string"},
				},
				"required": []string{"name", "role", "prompt"},
			},
		},
		{
			Name: "list_teammates", Description: "List all teammates.",
			InputSchema: map[string]any{"properties": map[string]any{}},
		},
		{
			Name: "send_message", Description: "Send a message to a teammate.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"to":       map[string]any{"type": "string"},
					"content":  map[string]any{"type": "string"},
					"msg_type": map[string]any{"type": "string", "enum": msgTypes},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name: "read_inbox", Description: "Read and drain the lead's inbox.",
			InputSchema: map[string]any{"properties": map[string]any{}},
		},
		{
			Name: "broadcast", Description: "Send a message to all teammates.",
			InputSchema: map[string]any{
				"properties": map[string]any{"content": map[string]any{"type": "string"}},
				"required":   []string{"content"},
			},
		},
		{
			Name: "shutdown_request", Description: "Request a teammate to shut down.",
			InputSchema: map[string]any{
				"properties": map[string]any{"teammate": map[string]any{"type": "string"}},
				"required":   []string{"teammate"},
			},
		},
		{
			Name: "shutdown_status", Description: "Check shutdown request status.",
			InputSchema: map[string]any{
				"properties": map[string]any{"request_id": map[string]any{"type": "string"}},
				"required":   []string{"request_id"},
			},
		},
		{
			Name: "plan_approval", Description: "Approve or reject a teammate's plan.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"approve":    map[string]any{"type": "boolean"},
					"feedback":   map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "approve"},
			},
		},
		{
			Name: "idle", Description: "Enter idle state (for lead -- rarely used).",
			InputSchema: map[string]any{"properties": map[string]any{}},
		},
		{
			Name: "claim_task", Description: "Claim a task from the board by ID.",
			InputSchema: map[string]any{
				"properties": map[string]any{"task_id": map[string]any{"type": "integer"}},
				"required":   []string{"task_id"},
			},
		},
	}
}

// =============================================================================
// Agent loop
// =============================================================================

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		if inbox := bus.ReadInbox("lead"); len(inbox) > 0 {
			data, _ := json.MarshalIndent(inbox, "", "  ")
			*messages = append(*messages,
				llm.UserMessage(fmt.Sprintf("<inbox>%s</inbox>", string(data))),
				llm.Message{Role: "assistant", Content: "Noted inbox messages."},
			)
		}

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
	tasksDir = filepath.Join(workdir, ".tasks")

	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	initHandlers(provider, model)
	initTools()

	system := fmt.Sprintf(
		"You are a team lead at %s. Teammates are autonomous -- they find work themselves.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms11 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}
		if query == "/team" {
			fmt.Println(team.ListAll())
			continue
		}
		if query == "/inbox" {
			msgs := bus.ReadInbox("lead")
			data, _ := json.MarshalIndent(msgs, "", "  ")
			fmt.Println(string(data))
			continue
		}
		if query == "/tasks" {
			_ = os.MkdirAll(tasksDir, 0o755)
			entries, _ := os.ReadDir(tasksDir)
			var names []string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "task_") && strings.HasSuffix(e.Name(), ".json") {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			for _, n := range names {
				data, _ := os.ReadFile(filepath.Join(tasksDir, n))
				var t taskFile
				if json.Unmarshal(data, &t) != nil {
					continue
				}
				marker := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}[t.Status]
				if marker == "" {
					marker = "[?]"
				}
				owner := ""
				if t.Owner != "" {
					owner = " @" + t.Owner
				}
				fmt.Printf("  %s #%d: %s%s\n", marker, t.ID, t.Subject, owner)
			}
			continue
		}

		messages = append(messages, llm.UserMessage(query))
		if err := agentLoop(context.Background(), provider, model, system, &messages); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
}

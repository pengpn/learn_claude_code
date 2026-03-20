package main

/*
s10_team_protocols - Team Protocols

Shutdown protocol and plan approval protocol, both using the same
request_id correlation pattern. Builds on s09's team messaging.

    Shutdown FSM: pending -> approved | rejected

    Lead                              Teammate
    +---------------------+          +---------------------+
    | shutdown_request     |          |                     |
    | {request_id: abc}    | -------> | receives request    |
    +---------------------+          | decides: approve?   |
                                     +---------------------+
    +---------------------+                  |
    | shutdown_response    | <---------------+
    | {request_id: abc,    |
    |  approve: true}      |
    +---------------------+
            |
            v
    status -> "shutdown", goroutine stops

    Plan approval FSM: pending -> approved | rejected

    Teammate                          Lead
    +---------------------+          +---------------------+
    | plan_approval        |          |                     |
    | submit: {plan:"..."}| -------> | reviews plan text   |
    +---------------------+          | approve/reject?     |
                                     +---------------------+
    +---------------------+                  |
    | plan_approval_resp   | <---------------+
    | {approve: true}      |
    +---------------------+

    Trackers: {request_id: {"target|from": name, "status": "pending|..."}}

Key insight: "Same request_id correlation pattern, two domains."
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
// Request trackers: correlate by request_id
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
// MessageBus: JSONL inbox per teammate
// =============================================================================

type inboxMessage struct {
	Type      string  `json:"type"`
	From      string  `json:"from"`
	Content   string  `json:"content"`
	Timestamp float64 `json:"timestamp"`
	// extra fields for protocol messages
	RequestID string `json:"request_id,omitempty"`
	Approve   *bool  `json:"approve,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Feedback  string `json:"feedback,omitempty"`
}

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
// TeammateManager with shutdown + plan approval
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

	go tm.teammateLoop(name, role, prompt)
	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

func (tm *TeammateManager) teammateLoop(name, role, prompt string) {
	sysPrompt := fmt.Sprintf(
		"You are '%s', role: %s, at %s. "+
			"Submit plans via plan_approval before major work. "+
			"Respond to shutdown_request with shutdown_response.",
		name, role, workdir,
	)

	messages := []llm.Message{llm.UserMessage(prompt)}
	tools := teammateTools()
	shouldExit := false

	for range 50 {
		inbox := bus.ReadInbox(name)
		for _, msg := range inbox {
			data, _ := json.Marshal(msg)
			messages = append(messages, llm.UserMessage(string(data)))
		}
		if shouldExit {
			break
		}

		resp, err := tm.provider.Chat(context.Background(), llm.ChatParams{
			Model: tm.model, System: sysPrompt, Messages: messages,
			Tools: tools, MaxTokens: 8000,
		})
		if err != nil {
			break
		}

		messages = append(messages, llm.AssistantMessage(resp))
		if !resp.HasToolCalls() {
			break
		}

		var results []llm.ToolResult
		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			output := execTeammateTool(name, tc.Name, args)
			preview := output
			if len(preview) > 120 {
				preview = preview[:120]
			}
			fmt.Printf("  [%s] %s: %s\n", name, tc.Name, preview)

			// If teammate approves shutdown, exit after this iteration
			if tc.Name == "shutdown_response" {
				if approve, ok := args["approve"].(bool); ok && approve {
					shouldExit = true
				}
			}

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		messages = append(messages, llm.ToolResultsMessage(results))
	}

	tm.mu.Lock()
	if m := tm.findMember(name); m != nil {
		if shouldExit {
			m.Status = "shutdown"
		} else {
			m.Status = "idle"
		}
		tm.saveConfig()
	}
	tm.mu.Unlock()
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
		if entry, ok := shutdownRequests[reqID]; ok {
			if approve {
				entry.Status = "approved"
			} else {
				entry.Status = "rejected"
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
		return fmt.Sprintf("Plan submitted (request_id=%s). Waiting for lead approval.", reqID)
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
			InputSchema: map[string]any{
				"properties": map[string]any{},
			},
		},
		{
			Name: "shutdown_response", Description: "Respond to a shutdown request. Approve to shut down, reject to keep working.",
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
			Name: "plan_approval", Description: "Submit a plan for lead approval. Provide plan text.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"plan": map[string]any{"type": "string"},
				},
				"required": []string{"plan"},
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
	return fmt.Sprintf("Shutdown request %s sent to '%s' (status: pending)", reqID, teammate)
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
// Lead agent dispatch + tool definitions (12 tools)
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
			Name: "spawn_teammate", Description: "Spawn a persistent teammate.",
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
			InputSchema: map[string]any{
				"properties": map[string]any{},
			},
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
			InputSchema: map[string]any{
				"properties": map[string]any{},
			},
		},
		{
			Name: "broadcast", Description: "Send a message to all teammates.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"content"},
			},
		},
		{
			Name: "shutdown_request", Description: "Request a teammate to shut down gracefully. Returns a request_id.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"teammate": map[string]any{"type": "string"},
				},
				"required": []string{"teammate"},
			},
		},
		{
			Name: "shutdown_status", Description: "Check the status of a shutdown request by request_id.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
				},
				"required": []string{"request_id"},
			},
		},
		{
			Name: "plan_approval", Description: "Approve or reject a teammate's plan. Provide request_id + approve + optional feedback.",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"approve":    map[string]any{"type": "boolean"},
					"feedback":   map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "approve"},
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
	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	initHandlers(provider, model)
	initTools()

	system := fmt.Sprintf(
		"You are a team lead at %s. Manage teammates with shutdown and plan approval protocols.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms10 >> \033[0m")
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

		messages = append(messages, llm.UserMessage(query))
		if err := agentLoop(context.Background(), provider, model, system, &messages); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
}

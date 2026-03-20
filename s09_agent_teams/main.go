package main

/*
s09_agent_teams - Agent Teams

Persistent named agents with file-based JSONL inboxes. Each teammate runs
its own agent loop in a separate goroutine. Communication via append-only inboxes.

    Subagent (s04):  spawn -> execute -> return summary -> destroyed
    Teammate (s09):  spawn -> work -> idle -> work -> ... -> shutdown

    .team/config.json                   .team/inbox/
    +----------------------------+      +------------------+
    | {"team_name": "default",   |      | alice.jsonl      |
    |  "members": [              |      | bob.jsonl        |
    |    {"name":"alice",        |      | lead.jsonl       |
    |     "role":"coder",        |      +------------------+
    |     "status":"idle"}       |
    |  ]}                        |      send_message("alice", "fix bug"):
    +----------------------------+        append to alice.jsonl

                                        read_inbox("alice"):
    spawn_teammate("alice","coder",...)   drain alice.jsonl -> return msgs
         |
         v
    Goroutine: alice          Goroutine: bob
    +------------------+      +------------------+
    | agent_loop       |      | agent_loop       |
    | status: working  |      | status: idle     |
    | ... runs tools   |      | ... waits ...    |
    | status -> idle   |      |                  |
    +------------------+      +------------------+

Key insight: "Teammates that can talk to each other."
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

// =============================================================================
// MessageBus: JSONL inbox per teammate
// =============================================================================

type inboxMessage struct {
	Type      string  `json:"type"`
	From      string  `json:"from"`
	Content   string  `json:"content"`
	Timestamp float64 `json:"timestamp"`
}

type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewMessageBus(dir string) *MessageBus {
	_ = os.MkdirAll(dir, 0o755)
	return &MessageBus{dir: dir}
}

func (mb *MessageBus) Send(sender, to, content, msgType string) string {
	if msgType == "" {
		msgType = "message"
	}
	if !validMsgTypes[msgType] {
		return fmt.Sprintf("Error: Invalid type '%s'", msgType)
	}
	msg := inboxMessage{
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(time.Now().UnixMilli()) / 1000.0,
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

func (mb *MessageBus) ReadInbox(name string) []inboxMessage {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	path := filepath.Join(mb.dir, name+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var messages []inboxMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var msg inboxMessage
		if json.Unmarshal([]byte(line), &msg) == nil {
			messages = append(messages, msg)
		}
	}

	// Drain: clear the inbox
	_ = os.WriteFile(path, nil, 0o644)
	return messages
}

func (mb *MessageBus) Broadcast(sender, content string, teammates []string) string {
	count := 0
	for _, name := range teammates {
		if name != sender {
			mb.Send(sender, name, content, "broadcast")
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count)
}

// =============================================================================
// TeammateManager: persistent named agents with config.json
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
		"You are '%s', role: %s, at %s. Use send_message to communicate. Complete your task.",
		name, role, workdir,
	)

	messages := []llm.Message{llm.UserMessage(prompt)}
	tools := teammateTools()

	for range 50 {
		// Check inbox
		inbox := bus.ReadInbox(name)
		for _, msg := range inbox {
			data, _ := json.Marshal(msg)
			messages = append(messages, llm.UserMessage(string(data)))
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

			results = append(results, llm.ToolResult{ToolCallID: tc.ID, Content: output})
		}
		messages = append(messages, llm.ToolResultsMessage(results))
	}

	// Mark idle when done
	tm.mu.Lock()
	if m := tm.findMember(name); m != nil && m.Status != "shutdown" {
		m.Status = "idle"
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
		return bus.Send(sender, args["to"].(string), args["content"].(string), msgType)
	case "read_inbox":
		msgs := bus.ReadInbox(sender)
		data, _ := json.MarshalIndent(msgs, "", "  ")
		return string(data)
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
	}
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
// Lead agent dispatch + tool definitions
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

	msgTypes := make([]string, 0, len(validMsgTypes))
	for t := range validMsgTypes {
		msgTypes = append(msgTypes, t)
	}

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
			return bus.Send("lead", args["to"].(string), args["content"].(string), msgType)
		},
		"read_inbox": func(args map[string]any) string {
			msgs := bus.ReadInbox("lead")
			data, _ := json.MarshalIndent(msgs, "", "  ")
			return string(data)
		},
		"broadcast": func(args map[string]any) string {
			return bus.Broadcast("lead", args["content"].(string), team.MemberNames())
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
			Name: "spawn_teammate", Description: "Spawn a persistent teammate that runs in its own goroutine.",
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
			Name: "list_teammates", Description: "List all teammates with name, role, status.",
			InputSchema: map[string]any{
				"properties": map[string]any{},
			},
		},
		{
			Name: "send_message", Description: "Send a message to a teammate's inbox.",
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
	}
}

// =============================================================================
// Agent loop
// =============================================================================

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		// Drain lead's inbox before each LLM call
		if inbox := bus.ReadInbox("lead"); len(inbox) > 0 {
			data, _ := json.MarshalIndent(inbox, "", "  ")
			*messages = append(*messages,
				llm.UserMessage(fmt.Sprintf("<inbox>%s</inbox>", string(data))),
				llm.Message{Role: "assistant", Content: "Noted inbox messages."},
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
	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	initHandlers(provider, model)
	initTools()

	system := fmt.Sprintf("You are a team lead at %s. Spawn teammates and communicate via inboxes.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms09 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" || strings.EqualFold(query, "q") || strings.EqualFold(query, "exit") {
			break
		}

		// Local commands
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

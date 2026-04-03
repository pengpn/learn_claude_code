package main

/*
s06_context_compact - 上下文压缩

这个示例展示一个可长期运行的 agent 如何分层压缩上下文，
避免随着对话、工具调用和工具输出不断累积，最终撑爆上下文窗口。

整体分为三层压缩：

    每一轮开始：
    +------------------+
    | 工具调用结果累积 |
    +------------------+
            |
            v
    [第 1 层: microCompact]        （静默执行，每轮都做）
      把较早的 tool_result 内容替换成简短占位符
      例如 "[Previous: used bash]"
      只保留最近 3 个工具结果的详细内容
            |
            v
    [检查: token 是否超过 50000]
       |               |
       否              是
       |               |
       v               v
     继续对话     [第 2 层: autoCompact]
                  先把完整 transcript 保存到 .transcripts/
                  再让 LLM 生成一份连续性摘要
                  最后用摘要消息替换原始长历史
                        |
                        v
                [第 3 层: compact 工具]
                  模型主动调用 compact
                  立即触发一次手动压缩

核心思想：
不是死记所有细节，而是有策略地遗忘低价值历史，
把有限上下文留给当前最重要的推理与操作。


最推荐的一组完整测试步骤

启动程序
连续输入 4 到 6 次“读取某个 main.go 全文”
观察是否出现 [auto_compact triggered]
再输入“请先压缩当前上下文，再总结”
观察是否出现 > compact: Compressing... 和 [manual compact]
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

const (
	threshold    = 50000 // 估算 token 超过这个阈值后，触发自动压缩
	keepRecent   = 3     // 保留最近多少个工具结果的完整内容
	maxOutputLen = 50000
)

var transcriptDir string

// estimateTokens 用非常粗略的方式估算当前消息大约占用多少 token。
// 这里采用“约 4 个字符 ~= 1 个 token”的经验规则，
// 目的不是精确计数，而是判断是否需要触发 autoCompact。
func estimateTokens(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Arguments) + len(tc.Name)
		}
		for _, tr := range m.ToolResults {
			total += len(tr.Content)
		}
	}
	return total / 4
}

// -- 第 1 层：microCompact --
// 这一层是“轻量瘦身”：
// 1. 每轮都执行，成本低
// 2. 主要压缩历史工具输出，因为它们通常最占上下文
// 3. 不完全抹掉历史，而是保留“之前调用过哪个工具”的痕迹
//
// 效果：
// 模型依然知道过去做过哪些操作，
// 但不用一直背着大段命令输出或整份文件内容前进。

type toolResultRef struct {
	msgIdx    int
	resultIdx int
}

func microCompact(messages []llm.Message) {
	// 先从 assistant 消息里建立 tool_call_id -> tool_name 的映射，
	// 后面替换 tool_result 时，需要知道它原本来自哪个工具。
	nameMap := map[string]string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}

	// 收集所有 tool_result 在消息数组中的位置。
	var refs []toolResultRef
	for i, m := range messages {
		if len(m.ToolResults) > 0 {
			for j := range m.ToolResults {
				refs = append(refs, toolResultRef{msgIdx: i, resultIdx: j})
			}
		}
	}

	if len(refs) <= keepRecent {
		return
	}

	// 只保留最近 keepRecent 个完整结果。
	// 更早的长结果会被替换成占位符，减少上下文体积。
	toClear := refs[:len(refs)-keepRecent]
	for _, ref := range toClear {
		tr := &messages[ref.msgIdx].ToolResults[ref.resultIdx]
		if len(tr.Content) > 100 {
			name := nameMap[tr.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			tr.Content = fmt.Sprintf("[Previous: used %s]", name)
		}
	}
}

// -- 第 2 层：autoCompact --
// 当整体上下文已经过大时，仅靠替换旧 tool_result 不够，
// 这时就要把“整段历史”压缩成一份可续接的摘要。
//
// 处理顺序：
// 1. 把完整 transcript 落盘，保留原始记录
// 2. 把当前对话序列化后交给 LLM 总结
// 3. 用新的“摘要消息”替换掉旧 messages

func autoCompact(ctx context.Context, provider llm.Provider, model string,
	messages []llm.Message,
) []llm.Message {
	// 第一步：先保存完整历史到磁盘。
	// 这样即使内存里的 messages 被替换，原始上下文仍然可以追溯。
	_ = os.MkdirAll(transcriptDir, 0o755)
	transcriptPath := filepath.Join(transcriptDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	if f, err := os.Create(transcriptPath); err == nil {
		enc := json.NewEncoder(f)
		for _, m := range messages {
			_ = enc.Encode(m)
		}
		f.Close()
		fmt.Printf("[transcript saved: %s]\n", transcriptPath)
	}

	// 第二步：把对话序列化成文本，提供给 LLM 做总结。
	// 这里会限制长度，避免“为了压缩上下文，反而又构造出过大的总结输入”。
	raw, _ := json.Marshal(messages)
	conversationText := string(raw)
	if len(conversationText) > 80000 {
		conversationText = conversationText[:80000]
	}

	// 第三步：让 LLM 输出一份连续性摘要。
	// 这个摘要不是面向用户的润色回答，而是面向后续推理的“接班笔记”。
	summaryResp, err := provider.Chat(ctx, llm.ChatParams{
		Model: model,
		Messages: []llm.Message{
			llm.UserMessage(
				"Summarize this conversation for continuity. Include: " +
					"1) What was accomplished, 2) Current state, 3) Key decisions made. " +
					"Be concise but preserve critical details.\n\n" + conversationText,
			),
		},
		MaxTokens: 2000,
	})
	summary := "(summary failed)"
	if err == nil && summaryResp.Content != "" {
		summary = summaryResp.Content
	}

	// 最后：直接用极小的一组消息替换原始长历史。
	// 第一条消息告诉模型“历史已压缩，并给出摘要内容”；
	// 第二条消息让上下文看起来像一次正常续接，便于后续继续工作。
	return []llm.Message{
		llm.UserMessage(fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary)),
		{Role: "assistant", Content: "Understood. I have the context from the summary. Continuing."},
	}
}

// -- 工具实现 --

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

// 这里只做非常基础的危险命令拦截，用来防止明显不安全的命令。
// 这是示例级防护，不是完备的安全方案。
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
	if len(result) > maxOutputLen {
		return result[:maxOutputLen]
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
	if len(result) > maxOutputLen {
		return result[:maxOutputLen]
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

// -- 工具分发与工具定义 --

type toolHandler func(args map[string]any) string

var toolHandlers = map[string]toolHandler{
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
	"compact": func(args map[string]any) string {
		return "Manual compression requested."
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
		Name: "compact", Description: "Trigger manual conversation compression.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"focus": map[string]any{"type": "string", "description": "What to preserve in the summary"},
			},
		},
	},
}

// -- Agent 主循环 --
// 执行顺序体现了三层压缩的接入位置：
// 1. 先做第 1 层微压缩
// 2. 再按阈值判断是否需要第 2 层自动压缩
// 3. 然后正常调用模型
// 4. 如果模型主动调用 compact，再执行第 3 层手动压缩

func agentLoop(ctx context.Context, provider llm.Provider, model, system string,
	messages *[]llm.Message,
) error {
	for {
		// 第 1 层：静默压缩旧工具结果。
		// 这一步每轮都会做，尽量先把“低价值但高体积”的历史输出瘦身。
		microCompact(*messages)

		// 第 2 层：如果上下文估算已经太大，就触发自动摘要压缩。
		if estimateTokens(*messages) > threshold {
			fmt.Println("[auto_compact triggered]")
			fmt.Println("[自动压缩触发]")
			*messages = autoCompact(ctx, provider, model, *messages)
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
		manualCompact := false

		for _, tc := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)

			var output string
			if tc.Name == "compact" {
				// 第 3 层入口：
				// 模型显式要求压缩时，先记录标志，等这一轮 tool_result
				// 写回消息历史后，再统一执行 autoCompact。
				manualCompact = true
				output = "Compressing..."
			} else if handler, ok := toolHandlers[tc.Name]; ok {
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

		// 第 3 层：模型主动触发的手动压缩。
		// 逻辑上和第 2 层类似，只是触发条件从“超过阈值”变成“模型主动要求”。
		if manualCompact {
			fmt.Println("[manual compact]")
			fmt.Println("[手动压缩触发]")
			*messages = autoCompact(ctx, provider, model, *messages)
		}
	}
}

func main() {
	transcriptDir = filepath.Join(workdir, ".transcripts")

	provider, model, err := llm.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	system := fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks.", workdir)

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms06 >> \033[0m")
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

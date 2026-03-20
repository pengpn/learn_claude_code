package main

/*
s05_skill_loading - Skills

Two-layer skill injection that avoids bloating the system prompt:

    Layer 1 (cheap): skill names in system prompt (~100 tokens/skill)
    Layer 2 (on demand): full skill body in tool_result

    skills/
      pdf/
        SKILL.md          <-- frontmatter (name, description) + body
      code-review/
        SKILL.md

    System prompt:
    +--------------------------------------+
    | You are a coding agent.              |
    | Skills available:                    |
    |   - pdf: Process PDF files...        |  <-- Layer 1: metadata only
    |   - code-review: Review code...      |
    +--------------------------------------+

    When model calls load_skill("pdf"):
    +--------------------------------------+
    | tool_result:                         |
    | <skill>                              |
    |   Full PDF processing instructions   |  <-- Layer 2: full body
    |   Step 1: ...                        |
    |   Step 2: ...                        |
    | </skill>                             |
    +--------------------------------------+

Key insight: "Don't put everything in the system prompt. Load on demand."
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
	"strings"
	"time"

	"learn_claude_code/llm"
)

var workdir string

func init() {
	workdir, _ = os.Getwd()
}

// -- SkillLoader: scan skills/<name>/SKILL.md with YAML frontmatter --

type skill struct {
	meta map[string]string
	body string
	path string
}

type SkillLoader struct {
	skillsDir string
	skills    map[string]*skill
}

func NewSkillLoader(dir string) *SkillLoader {
	sl := &SkillLoader{skillsDir: dir, skills: make(map[string]*skill)}
	sl.loadAll()
	return sl
}

var frontmatterRE = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n(.*)`)

func (sl *SkillLoader) loadAll() {
	var files []string
	_ = filepath.Walk(sl.skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Name() == "SKILL.md" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		meta, body := parseFrontmatter(string(data))
		name := meta["name"]
		if name == "" {
			name = filepath.Base(filepath.Dir(f))
		}
		sl.skills[name] = &skill{meta: meta, body: body, path: f}
	}
}

// parseFrontmatter extracts YAML frontmatter between --- delimiters.
func parseFrontmatter(text string) (map[string]string, string) {
	m := frontmatterRE.FindStringSubmatch(text)
	if m == nil {
		return map[string]string{}, text
	}
	meta := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(m[1]), "\n") {
		if idx := strings.Index(line, ":"); idx >= 0 {
			meta[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
		}
	}
	return meta, strings.TrimSpace(m[2])
}

// GetDescriptions returns Layer 1: short descriptions for the system prompt.
func (sl *SkillLoader) GetDescriptions() string {
	if len(sl.skills) == 0 {
		return "(no skills available)"
	}
	names := make([]string, 0, len(sl.skills))
	for name := range sl.skills {
		names = append(names, name)
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		s := sl.skills[name]
		desc := s.meta["description"]
		if desc == "" {
			desc = "No description"
		}
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if tags := s.meta["tags"]; tags != "" {
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// GetContent returns Layer 2: full skill body in tool_result.
func (sl *SkillLoader) GetContent(name string) string {
	s, ok := sl.skills[name]
	if !ok {
		available := make([]string, 0, len(sl.skills))
		for k := range sl.skills {
			available = append(available, k)
		}
		return fmt.Sprintf("Error: Unknown skill %q. Available: %s", name, strings.Join(available, ", "))
	}
	return fmt.Sprintf("<skill name=%q>\n%s\n</skill>", name, s.body)
}

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

// -- Dispatch + tools --

var skillLoader *SkillLoader

type toolHandler func(args map[string]any) string

var toolHandlers map[string]toolHandler

func initHandlers() {
	skillLoader = NewSkillLoader(filepath.Join(workdir, "skills"))
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
		"load_skill": func(args map[string]any) string {
			return skillLoader.GetContent(args["name"].(string))
		},
	}
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
		Name: "load_skill", Description: "Load specialized knowledge by name.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name to load"},
			},
			"required": []string{"name"},
		},
	},
}

// -- Agent loop --

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

	// Layer 1: skill metadata injected into system prompt
	system := fmt.Sprintf(`You are a coding agent at %s.
Use load_skill to access specialized knowledge before tackling unfamiliar topics.

Skills available:
%s`, workdir, skillLoader.GetDescriptions())

	var messages []llm.Message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\033[36ms05 >> \033[0m")
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

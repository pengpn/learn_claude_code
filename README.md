# Learn Claude Code

[中文版](README_zh.md)

A progressive tutorial for building an AI Coding Agent from scratch, implemented in Go.

**This project is inspired by [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)** (official learning site: [learn.shareai.run](https://learn.shareai.run/en/)). It rewrites the original Python reference implementation in Go while preserving the same lesson structure and core concepts.

Each lesson is a standalone runnable program. Starting from the simplest Agent Loop, it progressively adds tool calling, subagents, context compaction, task systems, team collaboration, and more — ultimately building a complete multi-agent system.

## Lessons

| Lesson | Topic | Core Concept |
|--------|-------|--------------|
| `s01_agent_loop` | Agent Loop | `LLM(messages, tools) → execute → append → loop` core loop |
| `s02_tool_use` | Tools | Tool definitions + dispatch map routing |
| `s03_todo_write` | TodoWrite | Model self-tracks progress with nag reminder mechanism |
| `s04_subagent` | Subagents | Subagent context isolation, returns summary only |
| `s05_skill_loading` | Skills | Two-layer injection: system prompt index + on-demand full skill loading |
| `s06_context_compact` | Context Compact | Three-layer compaction: micro-compact / auto-compact / manual compact |
| `s07_task_system` | Task System | Persistent task board + dependency graph (blockedBy/blocks) |
| `s08_background_tasks` | Background Tasks | Goroutine background execution + notification queue |
| `s09_agent_teams` | Agent Teams | Persistent teammates + JSONL inbox messaging |
| `s10_team_protocols` | Team Protocols | Shutdown protocol + plan approval protocol (request_id correlation) |
| `s11_autonomous_agents` | Autonomous Agents | Idle polling + auto task claiming + identity re-injection |
| `s12_worktree_task_isolation` | Worktree Isolation | Git worktree directory isolation + task binding |

## Quick Start

### 1. Prerequisites

- Go 1.23+
- Any LLM API key (supports OpenAI, Anthropic, Qwen, DeepSeek, Kimi, Zhipu GLM, MiniMax)

### 2. Clone & Install

```bash
git clone https://github.com/<your-username>/learn_claude_code.git
cd learn_claude_code
go mod download
```

### 3. Configure LLM

Copy the example config and fill in your API key:

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` to set `provider` and the corresponding `api_key`. Two approaches are supported:

- **Environment variable reference** (recommended): `api_key: ${OPENAI_API_KEY}`, then `export OPENAI_API_KEY=sk-xxx`
- **Direct value**: `api_key: sk-xxx`

### 4. Run Any Lesson

```bash
# Run the basic Agent Loop
go run ./s01_agent_loop/

# Run the version with tools
go run ./s02_tool_use/

# Run the autonomous Agent team
go run ./s11_autonomous_agents/

# Run worktree isolation
go run ./s12_worktree_task_isolation/
```

Each program launches an interactive REPL. Type natural language instructions to interact with the Agent. Type `q` to quit.

## Project Structure

```
learn_claude_code/
├── llm/                          # LLM abstraction layer (Provider interface)
│   ├── provider.go               #   Provider interface + factory function
│   ├── config.go                 #   config.yaml parsing
│   ├── openai.go                 #   OpenAI-compatible implementation (works with most Chinese providers)
│   └── anthropic.go              #   Anthropic native SDK implementation
├── s01_agent_loop/main.go        # Lessons 01–12, each independently runnable
├── s02_tool_use/main.go
├── ...
├── s12_worktree_task_isolation/main.go
├── skills/                       # Agent skill definitions (used by s05)
│   ├── agent-builder/
│   ├── code-review/
│   ├── mcp-builder/
│   └── pdf/
├── config.example.yaml           # Config template (no keys)
├── config.yaml                   # Actual config (.gitignore'd)
└── go.mod
```

## LLM Provider Support

All OpenAI-compatible API providers are supported through a single HTTP client adapter — just change `base_url`:

| Provider | base_url | Default Model |
|----------|----------|---------------|
| OpenAI | `https://api.openai.com/v1` | gpt-4o |
| Anthropic | Via native SDK | claude-sonnet-4-20250514 |
| Qwen | `https://dashscope.aliyuncs.com/compatible-mode/v1` | qwen-plus |
| DeepSeek | `https://api.deepseek.com/v1` | deepseek-chat |
| Kimi | `https://api.moonshot.cn/v1` | moonshot-v1-32k |
| Zhipu GLM | `https://open.bigmodel.cn/api/paas/v4` | glm-4-flash |
| MiniMax | `https://api.minimax.chat/v1` | MiniMax-Text-01 |

You can also override the config via environment variables:

```bash
PROVIDER=deepseek MODEL_ID=deepseek-chat go run ./s01_agent_loop/
```

## License

[MIT](LICENSE)

# Agent Learn

[English](README.md)

从零构建 AI Coding Agent 的渐进式教程，使用 Go 实现。

**本项目源于 [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)**（官方学习网站：[learn.shareai.run](https://learn.shareai.run/en/)），将原版 Python 参考实现转写为 Go，并保持相同的课程结构与核心概念。

每一课都是一个独立可运行的程序，从最简单的 Agent Loop 开始，逐步添加工具调用、子代理、上下文压缩、任务系统、团队协作等能力，最终构建出一个完整的多智能体系统。

## 课程目录

| 课程 | 主题 | 核心概念 |
|------|------|----------|
| `s01_agent_loop` | Agent Loop | `LLM(messages, tools) → execute → append → loop` 核心循环 |
| `s02_tool_use` | Tools | 工具定义 + dispatch map 路由 |
| `s03_todo_write` | TodoWrite | 模型自我追踪进度，nag reminder 机制 |
| `s04_subagent` | Subagents | 子代理隔离上下文，仅返回摘要 |
| `s05_skill_loading` | Skills | 两层注入：system prompt 索引 + 按需加载完整 skill |
| `s06_context_compact` | Context Compact | 三层压缩：micro-compact / auto-compact / manual compact |
| `s07_task_system` | Task System | 持久化任务看板 + 依赖图（blockedBy/blocks） |
| `s08_background_tasks` | Background Tasks | goroutine 后台执行 + 通知队列 |
| `s09_agent_teams` | Agent Teams | 持久化 teammates + JSONL 收件箱通信 |
| `s10_team_protocols` | Team Protocols | 关停协议 + 计划审批协议（request_id 关联） |
| `s11_autonomous_agents` | Autonomous Agents | 空闲轮询 + 自动认领任务 + 身份重注入 |
| `s12_worktree_task_isolation` | Worktree Isolation | git worktree 目录隔离 + 任务绑定 |

## 快速开始

### 1. 环境要求

- Go 1.23+
- 任意 LLM API key（支持 OpenAI、Anthropic、通义千问、DeepSeek、Kimi、智谱、MiniMax）

### 2. 克隆与安装

```bash
git clone https://github.com/<your-username>/learn_claude_code.git
cd learn_claude_code
go mod download
```

### 3. 配置 LLM

复制示例配置并填入你的 API key：

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，设置 `provider` 和对应的 `api_key`。支持两种方式：

- **环境变量引用**（推荐）：`api_key: ${OPENAI_API_KEY}`，然后 `export OPENAI_API_KEY=sk-xxx`
- **直接填写**：`api_key: sk-xxx`

### 4. 运行任意一课

```bash
# 运行最基础的 Agent Loop
go run ./s01_agent_loop/

# 运行带工具的版本
go run ./s02_tool_use/

# 运行自治 Agent 团队
go run ./s11_autonomous_agents/

# 运行 worktree 隔离
go run ./s12_worktree_task_isolation/
```

每个程序启动后进入交互式 REPL，输入自然语言指令即可与 Agent 对话。输入 `q` 退出。

## 项目结构

```
learn_claude_code/
├── llm/                          # LLM 抽象层（Provider 接口）
│   ├── provider.go               #   Provider 接口 + 工厂函数
│   ├── config.go                 #   config.yaml 解析
│   ├── openai.go                 #   OpenAI 兼容实现（适用大多数国内厂商）
│   └── anthropic.go              #   Anthropic 原生 SDK 实现
├── s01_agent_loop/main.go        # 课程 01-12，每课独立可运行
├── s02_tool_use/main.go
├── ...
├── s12_worktree_task_isolation/main.go
├── skills/                       # Agent skill 定义（供 s05 使用）
│   ├── agent-builder/
│   ├── code-review/
│   ├── mcp-builder/
│   └── pdf/
├── config.example.yaml           # 配置模板（不含 key）
├── config.yaml                   # 实际配置（.gitignore 已忽略）
└── go.mod
```

## LLM Provider 支持

所有 OpenAI 兼容接口的厂商均通过同一个 HTTP 客户端适配，只需修改 `base_url`：

| Provider | base_url | 默认模型 |
|----------|----------|----------|
| OpenAI | `https://api.openai.com/v1` | gpt-4o |
| Anthropic | 通过原生 SDK | claude-sonnet-4-20250514 |
| 通义千问 | `https://dashscope.aliyuncs.com/compatible-mode/v1` | qwen-plus |
| DeepSeek | `https://api.deepseek.com/v1` | deepseek-chat |
| Kimi | `https://api.moonshot.cn/v1` | moonshot-v1-32k |
| 智谱 GLM | `https://open.bigmodel.cn/api/paas/v4` | glm-4-flash |
| MiniMax | `https://api.minimax.chat/v1` | MiniMax-Text-01 |

也可以通过环境变量覆盖 config：

```bash
PROVIDER=deepseek MODEL_ID=deepseek-chat go run ./s01_agent_loop/
```

## License

[MIT](LICENSE)

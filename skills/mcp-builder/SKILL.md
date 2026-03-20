---
name: mcp-builder
description: Build MCP (Model Context Protocol) servers that give Claude new capabilities. Use when user wants to create an MCP server, add tools to Claude, or integrate external services.
---

# MCP Server Building Skill

You now have expertise in building MCP (Model Context Protocol) servers. MCP enables Claude to interact with external services through a standardized protocol.

## What is MCP?

MCP servers expose:
- **Tools**: Functions Claude can call (like API endpoints)
- **Resources**: Data Claude can read (like files or database records)
- **Prompts**: Pre-built prompt templates

## Quick Start: Go MCP Server

### 1. Project Setup

```bash
# Create project
mkdir my-mcp-server && cd my-mcp-server
go mod init my-mcp-server

# Install MCP SDK
go get github.com/mark3labs/mcp-go
```

### 2. Basic Server Template

```go
package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("my-server", "1.0.0")

	// Define a tool
	helloTool := mcp.NewTool("hello",
		mcp.WithDescription("Say hello to someone"),
		mcp.WithString("name", mcp.Required(), mcp.Description("The name to greet")),
	)

	s.AddTool(helloTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.Params.Arguments["name"].(string)
		return mcp.NewToolResultText(fmt.Sprintf("Hello, %s!", name)), nil
	})

	// Define another tool
	addTool := mcp.NewTool("add_numbers",
		mcp.WithDescription("Add two numbers together"),
		mcp.WithNumber("a", mcp.Required(), mcp.Description("First number")),
		mcp.WithNumber("b", mcp.Required(), mcp.Description("Second number")),
	)

	s.AddTool(addTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := req.Params.Arguments["a"].(float64)
		b := req.Params.Arguments["b"].(float64)
		return mcp.NewToolResultText(fmt.Sprintf("%.0f", a+b)), nil
	})

	// Run server via stdio
	if err := server.ServeStdio(s); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}
```

### 3. Register with Claude/Cursor

Add to `~/.claude/mcp.json` (Claude Code):
```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/my-mcp-server"
    }
  }
}
```

Or in Cursor's `.cursor/mcp.json`:
```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/my-mcp-server"
    }
  }
}
```

Build first: `go build -o my-mcp-server .`

## Advanced Patterns

### External API Integration

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("weather-server", "1.0.0")

	weatherTool := mcp.NewTool("get_weather",
		mcp.WithDescription("Get current weather for a city"),
		mcp.WithString("city", mcp.Required(), mcp.Description("City name")),
	)

	s.AddTool(weatherTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		city := req.Params.Arguments["city"].(string)

		resp, err := http.Get(fmt.Sprintf(
			"https://api.weatherapi.com/v1/current.json?key=YOUR_API_KEY&q=%s", city))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer resp.Body.Close()

		var data map[string]any
		json.NewDecoder(resp.Body).Decode(&data)
		current := data["current"].(map[string]any)

		return mcp.NewToolResultText(fmt.Sprintf("%s: %.1f°C, %s",
			city,
			current["temp_c"].(float64),
			current["condition"].(map[string]any)["text"].(string),
		)), nil
	})

	server.ServeStdio(s)
}
```

### Database Access

```go
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	db, _ := sql.Open("sqlite3", "data.db")
	defer db.Close()

	s := server.NewMCPServer("db-server", "1.0.0")

	queryTool := mcp.NewTool("query_db",
		mcp.WithDescription("Execute a read-only SQL query"),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL SELECT query")),
	)

	s.AddTool(queryTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.Params.Arguments["sql"].(string)
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
			return mcp.NewToolResultError("Only SELECT queries allowed"), nil
		}

		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		defer rows.Close()

		cols, _ := rows.Columns()
		var results []map[string]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			row := map[string]any{}
			for i, col := range cols {
				row[col] = vals[i]
			}
			results = append(results, row)
		}

		out, _ := json.MarshalIndent(results, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	})

	server.ServeStdio(s)
}
```

### Resources (Read-only Data)

```go
s.AddResource(mcp.NewResource("config://settings", "Application settings"),
	func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		data, err := os.ReadFile("settings.json")
		if err != nil {
			return nil, err
		}
		return []mcp.ResourceContents{
			mcp.NewTextResourceContents(req.Params.URI, "application/json", string(data)),
		}, nil
	},
)
```

## SSE Transport (HTTP Server)

```go
import "github.com/mark3labs/mcp-go/server"

// Instead of stdio, serve over HTTP with SSE
sseServer := server.NewSSEServer(s, server.WithBaseURL("http://localhost:8080"))
if err := sseServer.Start(":8080"); err != nil {
	log.Fatal(err)
}
```

## Testing

```bash
# Build the server
go build -o my-mcp-server .

# Test with MCP Inspector
npx @modelcontextprotocol/inspector ./my-mcp-server

# Or send test messages via stdin
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./my-mcp-server
```

## Best Practices

1. **Clear tool descriptions**: Claude uses these to decide when to call tools
2. **Input validation**: Always validate and sanitize inputs
3. **Error handling**: Return `mcp.NewToolResultError()` with meaningful messages
4. **Context propagation**: Use `ctx` from handler for cancellation and timeouts
5. **Security**: Never expose sensitive operations without auth
6. **Idempotency**: Tools should be safe to retry
7. **Build as binary**: Distribute compiled binaries for easy deployment

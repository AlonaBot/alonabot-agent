// mcptest 命令提供 Agent 集成测试使用的 stdio MCP 服务。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type arguments struct {
	Text string `json:"text"`
	Fail bool   `json:"fail,omitempty"`
}

func main() {
	pidFile := flag.String("pid-file", "", "write process ID")
	ignoreEOF := flag.Bool("ignore-eof", false, "wait after stdin closes")
	flag.Parse()
	if *pidFile != "" {
		if err := os.WriteFile(*pidFile, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			panic(err)
		}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-test", Version: "1.0.0"}, nil)
	var calls atomic.Int32
	mcp.AddTool(server, &mcp.Tool{Name: "echo/tool", Description: "MCP echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in arguments) (*mcp.CallToolResult, any, error) {
		result := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: in.Text}, &mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"}},
			StructuredContent: map[string]any{
				"pid": os.Getpid(), "text": in.Text, "calls": calls.Add(1),
				"credential": os.Getenv("MCP_TEST_SECRET"), "inherited": os.Getenv("ALONABOT_ADMIN_TOKEN"),
			},
			IsError: in.Fail,
		}
		return result, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		raw, _ := json.Marshal(err.Error())
		fmt.Fprintln(os.Stderr, string(raw))
		os.Exit(1)
	}
	if *ignoreEOF {
		for {
			time.Sleep(time.Hour)
		}
	}
}

package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gin-gonic/gin"
)

// IntegrationHandler 对外接入的配置生成（MCP / OpenAI 兼容）
//
// 为什么放在后端生成：客户端配置里要填 exe 的**绝对路径**与端口，
// 这些只有运行中的引擎自己知道。让用户手工去拼，十个人里九个会填错。
type IntegrationHandler struct {
	baseDir string
	port    string
}

func NewIntegrationHandler(baseDir, port string) *IntegrationHandler {
	return &IntegrationHandler{baseDir: baseDir, port: port}
}

// MCPConfig GET /api/mcp/config
//
// 返回可直接粘进 Claude Desktop / Cursor 的配置片段。
// command 用当前 exe 的绝对路径——用户把项目挪了位置也只是重新点一次复制。
func (h *IntegrationHandler) MCPConfig(c *gin.Context) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = filepath.Join(h.baseDir, "FlowEdge.exe")
	}
	env := map[string]string{"PORT": h.port}
	if token := os.Getenv("FLOWEDGE_API_TOKEN"); token != "" {
		// 引擎开了鉴权时 MCP 也必须带同一个 token
		env["FLOWEDGE_API_TOKEN"] = "<与引擎相同的 token>"
	}
	block := gin.H{
		"command": exe,
		"args":    []string{"--mcp"},
		"env":     env,
	}
	c.JSON(http.StatusOK, gin.H{
		"mcpServers":      gin.H{"flowedge": block},
		"exe":             exe,
		"os":              runtime.GOOS,
		"openai_base_url": "http://127.0.0.1:" + h.port + "/v1",
		"notes": []string{
			"把 mcpServers 整段合并进客户端的配置文件（Claude Desktop 是 claude_desktop_config.json）",
			"FlowEdge 必须先启动：MCP 是瘦客户端，转发到本机引擎，不会自己起一套",
			"OpenAI 兼容客户端（Open WebUI / SillyTavern）Base URL 填上面的 openai_base_url",
		},
	})
}

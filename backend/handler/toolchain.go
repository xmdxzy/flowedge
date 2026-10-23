package handler

// 工具链条目的 HTTP 出口。
//
// 分组：
//   /api/toolchain*        统一清单 + 重扫 + 读正文（四类工具共用）
//   /api/mcp/servers*      MCP 服务器的增删改启停 + 探活 + 工具清单 + 试跑
//   /api/plugins/:name     引擎自有插件的启停（改 plugin.json 的 enabled）
//
// 为什么增删改只允许作用于引擎自有的东西：别人的配置文件（WorkBuddy / Cursor /
// Claude）是那套工具的资产，引擎越界去改，坏的是整台机器的智能体工具链。
// 需要"关掉"时写覆盖层（data/mcp-overrides.json），原文件一个字节都不动。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// ToolchainHandler 工具链
type ToolchainHandler struct {
	tc *service.Toolchain
}

func NewToolchainHandler(tc *service.Toolchain) *ToolchainHandler {
	return &ToolchainHandler{tc: tc}
}

// List GET /api/toolchain
//
// 默认不探活（列表要快）；?probe=1 时对未探活过的 MCP 服务器并行探一遍。
func (h *ToolchainHandler) List(c *gin.Context) {
	probe := c.Query("probe") == "1"
	ctx := c.Request.Context()
	view := h.tc.Snapshot(ctx, probe)
	c.JSON(http.StatusOK, view)
}

// Reload POST /api/toolchain/reload
//
// 重扫插件目录并热注册节点类型 + 重读智能体名册 + 清 MCP 探活缓存。
// 不重启引擎：跑着的图不受影响，新加的插件/智能体下一次运行即可用。
func (h *ToolchainHandler) Reload(c *gin.Context) {
	c.JSON(http.StatusOK, h.tc.Reload())
}

// Doc GET /api/toolchain/doc?kind=skill|agent&name=xxx
//
// 读正文：技能包读 SKILL.md，插件里的智能体定义读 .md。两者都是只读资产，
// 面板里展开看内容，不需要也不允许编辑（编辑会与源头漂移）。
func (h *ToolchainHandler) Doc(c *gin.Context) {
	kind := c.Query("kind")
	name := c.Query("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺 name"})
		return
	}
	switch kind {
	case "skill":
		base := h.tc.BaseDir()
		packs := service.ScanSkillPacks(service.SkillRoots(base), service.LoadSkillPolicy(filepath.Join(base, "data", "skill-policy.json")))
		packs = append(packs, service.ScanPluginPacks(service.PluginSkillRoots(base), service.LoadSkillPolicy(filepath.Join(base, "data", "skill-policy.json")))...)
		doc, err := service.ReadSkillDoc(skillPackSource{packs}, name)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "读不到这个技能包正文", "detail": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"kind": kind, "name": name, "doc": doc})
	case "agent":
		// name 形如 "def:<plugin>/<file>" → 只允许读已安装插件缓存下的 agents/*.md
		doc, path, err := service.ReadPluginAgentDef(name)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "读不到这个智能体定义", "detail": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"kind": kind, "name": name, "path": path, "doc": doc})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind 只能是 skill 或 agent"})
	}
}

// ===== MCP =====

// ProbeMCP POST /api/mcp/servers/:name/probe
func (h *ToolchainHandler) ProbeMCP(c *gin.Context) {
	info, err := h.tc.ProbeMCP(c.Request.Context(), c.Param("name"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// Tools GET /api/mcp/servers/:name/tools
func (h *ToolchainHandler) Tools(c *gin.Context) {
	tools, err := h.tc.MCPTools(c.Request.Context(), c.Param("name"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"server": c.Param("name"), "count": len(tools), "tools": tools})
}

// CallMCP POST /api/mcp/servers/:name/call  {tool, args, timeout_ms}
//
// 试跑：与 mcp_call 节点走同一条调用路径（service.Toolchain.CallMCP），
// 面板里跑通的结果就是节点里会拿到的结果，不存在两套实现两种行为。
func (h *ToolchainHandler) CallMCP(c *gin.Context) {
	var body struct {
		Tool      string         `json:"tool"`
		Args      map[string]any `json:"args"`
		TimeoutMS int            `json:"timeout_ms"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return
	}
	timeout := 120 * time.Second
	if body.TimeoutMS > 0 {
		timeout = time.Duration(body.TimeoutMS) * time.Millisecond
	}
	res, err := h.tc.CallMCP(c.Request.Context(), c.Param("name"), body.Tool, body.Args, timeout)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// UpsertMCP PUT /api/mcp/servers
func (h *ToolchainHandler) UpsertMCP(c *gin.Context) {
	var spec service.MCPServerSpec
	if err := c.ShouldBindJSON(&spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := service.SaveMCPServer(h.tc.BaseDir(), spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.tc.InvalidateMCP(spec.Name)
	c.JSON(http.StatusOK, gin.H{"ok": true, "name": spec.Name, "path": filepath.Join(h.tc.BaseDir(), "data", "mcp.json")})
}

// DeleteMCP DELETE /api/mcp/servers/:name
func (h *ToolchainHandler) DeleteMCP(c *gin.Context) {
	name := c.Param("name")
	if err := service.DeleteMCPServer(h.tc.BaseDir(), name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.tc.InvalidateMCP(name)
	c.JSON(http.StatusOK, gin.H{"ok": true, "name": name})
}

// SetMCPEnabled POST /api/mcp/servers/:name/enabled  {enabled}
func (h *ToolchainHandler) SetMCPEnabled(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 {enabled: true|false}"})
		return
	}
	name := c.Param("name")
	if err := service.SetMCPEnabled(h.tc.BaseDir(), name, *body.Enabled); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.tc.InvalidateMCP(name)
	c.JSON(http.StatusOK, gin.H{"ok": true, "name": name, "enabled": *body.Enabled})
}

// ===== 插件 =====

// SetPluginEnabled POST /api/plugins/:name/enabled  {enabled}
//
// 只改引擎自有插件（data/plugins/<name>/plugin.json）。改完自动热重载：
// 停用后节点类型立刻消失，启用后立刻出现——「改了要重启」是最容易被忘掉的一步。
func (h *ToolchainHandler) SetPluginEnabled(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 {enabled: true|false}"})
		return
	}
	name := c.Param("name")
	dir := filepath.Join(h.tc.BaseDir(), "data", "plugins", name)
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "没有这个插件目录：" + dir})
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin.json 解析失败：" + err.Error()})
		return
	}
	m["enabled"] = *body.Enabled
	if err := service.WriteJSONFile(filepath.Join(dir, "plugin.json"), m); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写回失败：" + err.Error()})
		return
	}
	res := h.tc.Reload()
	c.JSON(http.StatusOK, gin.H{"ok": true, "name": name, "enabled": *body.Enabled, "reload": res})
}

// ===== 智能体纳管 =====

// AdoptAgent POST /api/toolchain/adopt {command}
func (h *ToolchainHandler) AdoptAgent(c *gin.Context) {
	var body struct {
		Command string `json:"command"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Command) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 {command: \"claude\"}"})
		return
	}
	if err := h.tc.AdoptAgentCLI(strings.TrimSpace(body.Command)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.tc.Reload()
	c.JSON(http.StatusOK, gin.H{"ok": true, "command": body.Command})
}

// SetAgentEnabled POST /api/toolchain/agents/:id/enabled {enabled}
//
// 与 /api/agents 的 Upsert 分开：启停是最常用的动作，不该要求前端
// 先把整条定义读回来再整条写回去（一次竞态就会把用户手写的字段抹掉）。
func (h *ToolchainHandler) SetAgentEnabled(c *gin.Context) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 {enabled: true|false}"})
		return
	}
	if err := h.tc.SetAgentEnabled(c.Param("id"), *body.Enabled); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id"), "enabled": *body.Enabled})
}

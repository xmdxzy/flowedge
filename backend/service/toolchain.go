package service

// 工具链：把「机器上有哪些工具」做成一份统一清单。
//
// 为什么需要这一层
//
//	FlowEdge 的四类外部能力过去各有各的发现方式，而且彼此不知道对方存在：
//	  技能包  /api/skills        扫 5 处根 + 专家插件市场（2026-10-23 才补齐）
//	  插件    data/plugins       只在**启动时**扫一次，加一个插件要重启
//	  MCP     没有               ← 只把自己包成 MCP 服务器给别人用
//	  智能体  data/agents.json   只认手写的名册，PATH 里装着的 CLI 视而不见
//
//	结果就是用户那句「我明明装了很多啊，怎么这里一个都没有」。根因不是少了某个
//	功能，而是**没有一张把它们放在一起、并且会自动发现新东西的表**。
//
// 设计要点
//
//  1. 每次请求都重扫，不缓存目录树。一次扫描 ≈ 几十毫秒，换来的是
//     「刚装的东西马上出现」，不用重启引擎、不用点刷新。
//     （MCP 探活会起子进程，那个才需要缓存，见 mcpCache。）
//  2. 「能改」与「只能看」必须分开（Managed）。别人的配置文件只读，
//     启停写覆盖层；引擎自己的目录才允许增删改。
//  3. 每条都带「怎么用」的一句话。列出能力却不说怎么用，等于没列。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"flowedge/backend/model"
)

// ToolEntry 统一条目。四类工具用同一个形状，前端才能用一套卡片渲染。
type ToolEntry struct {
	Kind   string `json:"kind"` // skill | mcp | plugin | agent
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Path   string `json:"path,omitempty"`
	// Enabled 现在的启用状态（配置层）
	Enabled bool `json:"enabled"`
	// Managed true = 引擎可写（改配置/删条目）。false = 别人的资产，只读 + 覆盖层启停。
	Managed bool `json:"managed"`
	// Usable 现在真的能用。与 Enabled 分开：
	// 启用了但命令不在 PATH、端口连不上，都是「配置了但用不了」。
	Usable  bool           `json:"usable"`
	Detail  string         `json:"detail,omitempty"`
	Use     string         `json:"use,omitempty"`     // 怎么用它（一句话）
	Actions []string       `json:"actions,omitempty"` // read/enable/disable/delete/probe/tools/call/adopt/open
	Meta    map[string]any `json:"meta,omitempty"`
	Probed  bool           `json:"probed,omitempty"` // Usable 是否为实测结果（否则来自缓存/推断）
}

// ToolSources 这一轮扫了哪些位置。扫不到东西时第一件要确认的事就是"你扫了哪"。
type ToolSources struct {
	Skill  []string          `json:"skill"`
	MCP    []MCPConfigSource `json:"mcp"`
	Plugin []string          `json:"plugin"`
	Agent  []string          `json:"agent"`
}

// ToolchainView 一次快照
type ToolchainView struct {
	GeneratedAt string         `json:"generated_at"`
	BaseDir     string         `json:"base_dir"`
	Counts      map[string]int `json:"counts"` // kind → 条目数
	Usable      map[string]int `json:"usable"` // kind → 真的能用的条目数
	Entries     []ToolEntry    `json:"entries"`
	Sources     ToolSources    `json:"sources"`
	Problems    []string       `json:"problems"`
}

// Toolchain 工具链聚合器
type Toolchain struct {
	baseDir string
	engine  *EngineV2
	agents  *AgentStore

	mu       sync.Mutex
	mcpCache map[string]mcpProbeCache
	// pluginProblems 上次插件扫描的失败原因（加载失败不阻断，但必须能看见）
	pluginProblems []string
	lastReload     time.Time
	reloadCount    int
}

type mcpProbeCache struct {
	info MCPServerInfo
	at   time.Time
}

// mcpProbeTTL 探活结果缓存时长。
//
// 为什么必须有：探活要 spawn 一个子进程（npx/node）跑完握手，1–5 秒。
// 工具台每次打开都全量探活，开三次就等于把用户机器上的 node 起三遍。
// 5 分钟是折中：刚改完配置点「重新扫描」会清缓存（见 Reload / InvalidateMCP）。
const mcpProbeTTL = 5 * time.Minute

// NewToolchain 装配
func NewToolchain(baseDir string, engine *EngineV2, agents *AgentStore) *Toolchain {
	return &Toolchain{baseDir: baseDir, engine: engine, agents: agents, mcpCache: map[string]mcpProbeCache{}}
}

// BaseDir 供 handler 做写操作
func (t *Toolchain) BaseDir() string { return t.baseDir }

// InvalidateMCP 清掉某台（或全部）服务器的探活缓存
func (t *Toolchain) InvalidateMCP(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name == "" {
		t.mcpCache = map[string]mcpProbeCache{}
		return
	}
	delete(t.mcpCache, name)
}

// Snapshot 生成清单。
//
// probeMCP=true 时对**未探活过的**服务器做一次真实探活（并行，受总超时约束）。
// 默认 false：列表要快，探活交给用户点「探活」或打开服务器详情。
func (t *Toolchain) Snapshot(ctx context.Context, probeMCP bool) ToolchainView {
	v := ToolchainView{
		GeneratedAt: time.Now().Format(time.RFC3339),
		BaseDir:     t.baseDir,
		Counts:      map[string]int{},
		Usable:      map[string]int{},
		Problems:    []string{},
	}
	v.Entries = append(v.Entries, t.skillEntries(&v)...)
	v.Entries = append(v.Entries, t.mcpEntries(ctx, probeMCP, &v)...)
	v.Entries = append(v.Entries, t.pluginEntries(&v)...)
	v.Entries = append(v.Entries, t.agentEntries(ctx, &v)...)

	for _, e := range v.Entries {
		v.Counts[e.Kind]++
		if e.Usable && e.Enabled {
			v.Usable[e.Kind]++
		}
	}
	sort.SliceStable(v.Entries, func(i, j int) bool {
		if v.Entries[i].Kind != v.Entries[j].Kind {
			return kindOrder(v.Entries[i].Kind) < kindOrder(v.Entries[j].Kind)
		}
		if v.Entries[i].Enabled != v.Entries[j].Enabled {
			return v.Entries[i].Enabled
		}
		return v.Entries[i].Name < v.Entries[j].Name
	})
	return v
}

func kindOrder(k string) int {
	switch k {
	case "agent":
		return 0
	case "skill":
		return 1
	case "mcp":
		return 2
	case "plugin":
		return 3
	}
	return 9
}

// ===== 技能包 =====

func (t *Toolchain) skillEntries(v *ToolchainView) []ToolEntry {
	roots := SkillRoots(t.baseDir)
	pluginRoots := PluginSkillRoots(t.baseDir)
	v.Sources.Skill = append(append([]string{}, roots...), pluginRoots...)

	policy := LoadSkillPolicy(filepath.Join(t.baseDir, "data", "skill-policy.json"))
	packs := ScanSkillPacks(roots, policy)
	packs = append(packs, ScanPluginPacks(pluginRoots, policy)...)

	out := make([]ToolEntry, 0, len(packs))
	for _, p := range packs {
		e := ToolEntry{
			Kind: "skill", ID: p.Name, Name: p.Name,
			Source: p.Source, Path: p.Path,
			// 技能包不是"开关"，而是"收不收录"。tier!=none 视为可用
			Enabled: p.Tier != TierNone,
			Managed: false,
			Usable:  p.Tier != TierNone,
			Detail:  p.Reason,
			Use:     fmt.Sprintf("在「外部智能体」或「MCP 工具」节点的上下文里引用 %s（执行时把 SKILL.md 路径写进任务描述）", p.Name),
			Actions: []string{"read", "open"},
			Probed:  true,
			Meta: map[string]any{
				"tier": p.Tier, "verdict": p.Verdict, "category": VerdictCategory(p.Verdict),
				"bytes": p.Bytes, "files": p.Files, "refs": len(p.Refs), "card": p.Card,
			},
		}
		out = append(out, e)
	}
	return out
}

// ===== MCP =====

func (t *Toolchain) mcpEntries(ctx context.Context, probe bool, v *ToolchainView) []ToolEntry {
	entries, problems, sources := LoadMCPServers(t.baseDir)
	v.Sources.MCP = sources
	v.Problems = append(v.Problems, problems...)

	// 并行探活（只有显式要求时才做）
	if probe {
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for _, e := range entries {
			if !e.Enabled {
				continue
			}
			if t.cachedProbeFresh(e.Name) {
				continue
			}
			wg.Add(1)
			go func(spec MCPServerSpec) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
				defer cancel()
				info := MCPProbe(pctx, spec, 25*time.Second)
				t.storeProbe(spec.Name, info)
			}(e.MCPServerSpec)
		}
		wg.Wait()
	}

	out := make([]ToolEntry, 0, len(entries))
	for _, e := range entries {
		entry := ToolEntry{
			Kind: "mcp", ID: e.Name, Name: e.Name,
			Source: e.Source, Path: e.Path,
			Enabled: e.Enabled, Managed: e.Managed,
			Use:     fmt.Sprintf("用「MCP 工具」节点，server 填 %s，tool 从工具台里挑", e.Name),
			Actions: []string{"probe", "tools"},
			Meta: map[string]any{
				"transport": e.Transport(),
				"command":   strings.TrimSpace(e.Command + " " + strings.Join(e.Args, " ")),
				"url":       e.URL,
			},
		}
		if e.Managed {
			entry.Actions = append(entry.Actions, "edit", "delete")
		}
		if e.Enabled {
			entry.Actions = append(entry.Actions, "disable")
		} else {
			entry.Actions = append(entry.Actions, "enable")
		}
		if len(e.Shadowed) > 0 {
			entry.Meta["shadowed"] = e.Shadowed
		}

		// 缓存里的探活结论
		if c, ok := t.probeOf(e.Name); ok {
			entry.Probed = true
			entry.Usable = c.OK && e.Enabled
			entry.Detail = c.Detail
			entry.Meta["tools"] = len(c.Tools)
			if c.Server != nil {
				entry.Meta["server_info"] = c.Server
			}
		} else if !e.Enabled {
			entry.Detail = "已停用"
		} else {
			entry.Detail = "未探活（点「探活」验证能不能连上、有哪些工具）"
		}
		out = append(out, entry)
	}
	return out
}

func (t *Toolchain) cachedProbeFresh(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.mcpCache[name]
	return ok && time.Since(c.at) < mcpProbeTTL
}

func (t *Toolchain) storeProbe(name string, info MCPServerInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mcpCache[name] = mcpProbeCache{info: info, at: time.Now()}
}

func (t *Toolchain) probeOf(name string) (MCPServerInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.mcpCache[name]
	if !ok || time.Since(c.at) > mcpProbeTTL {
		return MCPServerInfo{}, false
	}
	return c.info, true
}

// ProbeMCP 显式探活一台服务器（工具台的「探活」按钮）
func (t *Toolchain) ProbeMCP(ctx context.Context, name string) (MCPServerInfo, error) {
	spec, enabled := FindMCPServer(t.baseDir, name)
	if spec.Name == "" {
		return MCPServerInfo{}, fmt.Errorf("没有这个 MCP 服务器：%s", name)
	}
	if !enabled {
		return MCPServerInfo{Detail: "已停用：先启用再探活"}, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info := MCPProbe(pctx, spec, 30*time.Second)
	t.storeProbe(name, info)
	return info, nil
}

// MCPTools 取某台服务器的工具清单
func (t *Toolchain) MCPTools(ctx context.Context, name string) ([]MCPTool, error) {
	spec, enabled := FindMCPServer(t.baseDir, name)
	if spec.Name == "" {
		return nil, fmt.Errorf("没有这个 MCP 服务器：%s", name)
	}
	if !enabled {
		return nil, fmt.Errorf("服务器 %s 已停用：先启用再看它的工具", name)
	}
	if c, ok := t.probeOf(name); ok && len(c.Tools) > 0 {
		return c.Tools, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tools, err := MCPListTools(cctx, spec, 30*time.Second)
	if err != nil {
		return nil, err
	}
	t.storeProbe(name, MCPServerInfo{OK: true, Tools: tools, Detail: fmt.Sprintf("已连接 · %d 个工具", len(tools))})
	return tools, nil
}

// CallMCP 调用工具（节点与「试跑」共用同一条路径）
func (t *Toolchain) CallMCP(ctx context.Context, server, tool string, args map[string]any, timeout time.Duration) (MCPCallResult, error) {
	spec, enabled := FindMCPServer(t.baseDir, server)
	if spec.Name == "" {
		return MCPCallResult{}, fmt.Errorf("没有这个 MCP 服务器：%s（工具台里能看到全部已发现的服务器）", server)
	}
	if !enabled {
		return MCPCallResult{}, fmt.Errorf("服务器 %s 已停用", server)
	}
	return MCPCallTool(ctx, spec, tool, args, timeout)
}

// ===== 插件 =====

// nativePlugins 引擎自有插件（data/plugins）
func (t *Toolchain) scanNativePlugins() ([]NodePlugin, []ToolEntry) {
	dir := filepath.Join(t.baseDir, "data", "plugins")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var loaded []NodePlugin
	var out []ToolEntry
	var problems []string
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		pdir := filepath.Join(dir, name)
		m, err := LoadPluginManifest(pdir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("[%s] %v", name, err))
			out = append(out, ToolEntry{
				Kind: "plugin", ID: name, Name: name, Source: "引擎自有",
				Path: pdir, Enabled: false, Managed: true, Usable: false,
				Detail: "清单损坏：" + err.Error(), Actions: []string{"open"},
			})
			continue
		}
		title := m.Title
		if title == "" {
			title = m.Name
		}
		e := ToolEntry{
			Kind: "plugin", ID: m.Name, Name: title,
			Source: "引擎自有", Path: pdir,
			Enabled: m.IsEnabled(), Managed: true,
			Actions: []string{"open"},
			Meta: map[string]any{
				"type": m.Type, "version": m.Version, "category": m.Category,
				"description": m.Description, "side": m.Side,
			},
			Use: fmt.Sprintf("在画布节点面板搜「%s」直接拖出来（插件节点与内置节点同等）", title),
		}
		if m.IsEnabled() {
			e.Actions = append(e.Actions, "disable")
			p, lerr := LoadOnePlugin(pdir)
			if lerr != nil {
				e.Usable = false
				e.Detail = "加载失败：" + lerr.Error()
				problems = append(problems, fmt.Sprintf("[%s] %v", name, lerr))
			} else {
				e.Usable = true
				e.Detail = fmt.Sprintf("%s 型插件已加载 · 节点类型 %s", m.Type, m.Name)
				e.Meta["node_type"] = m.Name
				loaded = append(loaded, p)
			}
		} else {
			e.Actions = append(e.Actions, "enable")
			e.Detail = "已停用（节点类型不会注册）"
		}
		out = append(out, e)
	}
	t.mu.Lock()
	t.pluginProblems = problems
	t.mu.Unlock()
	return loaded, out
}

// workbuddyPlugins WorkBuddy 侧已安装插件（外部生态，只读发现）
//
// 为什么连它们一起列：机器上 60 多个插件承载着上百个技能包与智能体定义，
// 用户说「我装了很多」指的就是这些。FlowEdge 不能替 WorkBuddy 启停它们，
// 但必须让用户在这里看到"它们在哪、我能不能用"。
func (t *Toolchain) workbuddyPlugins() []ToolEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".workbuddy", "plugins")
	raw, err := os.ReadFile(filepath.Join(root, "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var f struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	keys := make([]string, 0, len(f.Plugins))
	for k := range f.Plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ToolEntry, 0, len(keys))
	for _, key := range keys {
		installs := f.Plugins[key]
		if len(installs) == 0 {
			continue
		}
		inst := installs[len(installs)-1]
		name := key
		market := ""
		if i := strings.Index(key, "@"); i > 0 {
			name, market = key[:i], key[i+1:]
		}
		title := name
		desc := ""
		version := inst.Version
		skills, agents := countPluginParts(inst.InstallPath)
		if m, derr := readWorkbuddyPluginManifest(inst.InstallPath); derr == nil {
			if m.DisplayName.Zh != "" {
				title = m.DisplayName.Zh
			}
			if m.Profession.Zh != "" {
				desc = m.Profession.Zh
			} else {
				desc = firstRunes(m.Description, 120)
			}
			if m.Version != "" {
				version = m.Version
			}
		}
		e := ToolEntry{
			Kind: "plugin", ID: key, Name: title,
			Source: "WorkBuddy·" + market, Path: inst.InstallPath,
			Enabled: true, Managed: false,
			// 装着不等于本机能用：它依赖的 connector 可能没连。
			// 这里只确认"文件在、能读"，能否跑的判断留给技能/智能体条目。
			Usable:  skills+agents > 0,
			Detail:  fmt.Sprintf("%s · %d 个技能 / %d 个智能体定义", version, skills, agents),
			Use:     "它的技能包已并入「技能」页签；智能体定义在「智能体」页签里可读",
			Actions: []string{"open"},
			Probed:  true,
			Meta: map[string]any{
				"version": version, "marketplace": market, "skills": skills, "agents": agents,
				"description": desc,
			},
		}
		out = append(out, e)
	}
	return out
}

func (t *Toolchain) pluginEntries(v *ToolchainView) []ToolEntry {
	home, _ := os.UserHomeDir()
	v.Sources.Plugin = []string{
		filepath.Join(t.baseDir, "data", "plugins"),
	}
	if home != "" {
		v.Sources.Plugin = append(v.Sources.Plugin, filepath.Join(home, ".workbuddy", "plugins", "installed_plugins.json"))
	}
	_, native := t.scanNativePlugins()
	out := append([]ToolEntry{}, native...)
	out = append(out, t.workbuddyPlugins()...)
	t.mu.Lock()
	v.Problems = append(v.Problems, t.pluginProblems...)
	t.mu.Unlock()
	return out
}

type wbPluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	DisplayName struct {
		Zh string `json:"zh"`
	} `json:"displayName"`
	Profession struct {
		Zh string `json:"zh"`
	} `json:"profession"`
	DisplayDescription struct {
		Zh string `json:"zh"`
	} `json:"displayDescription"`
}

func readWorkbuddyPluginManifest(installPath string) (wbPluginManifest, error) {
	var m wbPluginManifest
	for _, sub := range []string{".codebuddy-plugin", ".claude-plugin"} {
		raw, err := os.ReadFile(filepath.Join(installPath, sub, "plugin.json"))
		if err != nil {
			continue
		}
		if json.Unmarshal(raw, &m) == nil {
			return m, nil
		}
	}
	return m, fmt.Errorf("没有 plugin.json")
}

func countPluginParts(installPath string) (skills, agents int) {
	if ents, err := os.ReadDir(filepath.Join(installPath, "skills")); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				skills++
			}
		}
	}
	if ents, err := os.ReadDir(filepath.Join(installPath, "agents")); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				agents++
			}
		}
	}
	return
}

func firstRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// ===== 智能体 =====

// agentCLICandidates 本机可能装着的智能体 CLI。
//
// 为什么要自动发现：名册只认手写的 agents.json，而用户"装了个新工具"
// 通常就是往 PATH 里丢一个可执行文件。看不见它 = 这个工具在 FlowEdge 里不存在。
var agentCLICandidates = []struct {
	Cmd   string
	Label string
	Args  []string
	Caps  []string
}{
	{"claude", "Claude Code", []string{"-p", "{{prompt}}"}, []string{"code", "refactor"}},
	{"codex", "Codex", []string{"exec", "{{prompt}}"}, []string{"code"}},
	{"gemini", "Gemini CLI", []string{"-p", "{{prompt}}"}, []string{"code", "research"}},
	{"qwen", "Qwen Code", []string{"-p", "{{prompt}}"}, []string{"code"}},
	{"aider", "Aider", []string{"--message", "{{prompt}}"}, []string{"code"}},
	{"opencode", "opencode", []string{"run", "{{prompt}}"}, []string{"code"}},
	{"crush", "Crush", []string{"run", "{{prompt}}"}, []string{"code"}},
	{"goose", "Goose", []string{"run", "-t", "{{prompt}}"}, []string{"code"}},
	{"cursor-agent", "Cursor Agent", []string{"-p", "{{prompt}}"}, []string{"code"}},
	{"droid", "Droid", []string{"-p", "{{prompt}}"}, []string{"code"}},
}

func (t *Toolchain) agentEntries(ctx context.Context, v *ToolchainView) []ToolEntry {
	agentsPath := filepath.Join(t.baseDir, "data", "agents.json")
	v.Sources.Agent = []string{agentsPath}
	home, _ := os.UserHomeDir()
	if home != "" {
		v.Sources.Agent = append(v.Sources.Agent, filepath.Join(home, ".workbuddy", "plugins", "cache")+"/*/agents/*.md")
	}

	var out []ToolEntry
	knownCmd := map[string]bool{}

	if t.agents != nil {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		statuses := t.agents.ProbeAll(pctx)
		cancel()
		for _, st := range statuses {
			knownCmd[strings.ToLower(st.Command)] = true
			entry := ToolEntry{
				Kind: "agent", ID: st.ID, Name: st.Label,
				Source: "名册", Path: agentsPath,
				Enabled: st.IsEnabled(), Managed: true,
				Usable:  st.Available,
				Detail:  st.Detail,
				Probed:  true,
				Actions: []string{"open"},
				Meta: map[string]any{
					"kind": string(st.Kind), "command": st.Command, "capabilities": st.Capabilities,
					"base_url": st.BaseURL, "model": st.Model, "note": st.Note,
				},
			}
			if st.Kind == AgentCLI {
				entry.Use = fmt.Sprintf("用「外部智能体」节点，选 %s；命令行只存在 %s，不进工作流", st.Label, agentsPath)
			} else {
				entry.Use = fmt.Sprintf("用「外部智能体」节点，选 %s（OpenAI 兼容端点）", st.Label)
			}
			if st.IsEnabled() {
				entry.Actions = append(entry.Actions, "disable")
			} else {
				entry.Actions = append(entry.Actions, "enable")
			}
			entry.Actions = append(entry.Actions, "delete", "probe")
			out = append(out, entry)
		}
	}

	// PATH 里有、名册里没有 → 可一键纳管
	for _, c := range agentCLICandidates {
		if knownCmd[c.Cmd] {
			continue
		}
		p, err := exec.LookPath(c.Cmd)
		if err != nil {
			continue
		}
		out = append(out, ToolEntry{
			Kind: "agent", ID: "cli:" + c.Cmd, Name: c.Label,
			Source: "本机 PATH", Path: p,
			Enabled: false, Managed: false, Usable: true, Probed: true,
			Detail:  "检测到可执行文件但未纳管：" + p,
			Use:     "点「纳管」写进智能体名册，之后就能在「外部智能体」节点里选它",
			Actions: []string{"adopt"},
			Meta: map[string]any{
				"kind": "cli", "command": c.Cmd, "args": c.Args, "capabilities": c.Caps,
			},
		})
	}

	// 插件里的智能体定义（只读）
	for _, def := range pluginAgentDefs() {
		out = append(out, ToolEntry{
			Kind: "agent", ID: def.ID, Name: def.Name,
			Source: "插件·" + def.Plugin, Path: def.Path,
			Enabled: true, Managed: false, Usable: false, Probed: true,
			Detail:  "提示词/方法论定义（WorkBuddy 侧资产，引擎不直接执行）",
			Use:     "可查看正文，把它写进「外部智能体」节点的系统提示或作为技能包参考",
			Actions: []string{"read", "open"},
			Meta:    map[string]any{"plugin": def.Plugin},
		})
	}
	return out
}

type pluginAgentDef struct {
	ID     string
	Name   string
	Plugin string
	Path   string
}

// pluginAgentDefs 扫已安装插件的 agents/*.md。
//
// 上限 200 条：本机装了 60 多个插件，全量展开会把「智能体」页签塞满噪音。
// 超出时按插件名排序截断——可预期，不会随机少几个。
func pluginAgentDefs() []pluginAgentDef {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cacheDir := filepath.Join(home, ".workbuddy", "plugins", "cache")
	vendors, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}
	// 同一插件在缓存里可能有多个版本目录（1.0.0 / 1.0.1 / 带哈希的构建号），
	// 每个版本都有一份 agents/*.md。不按 ID 去重的话，面板上会出现三条一模一样的
	// 「code」——用户以为装了三份。按 ID 覆盖，保留版本号最大的那份。
	byID := map[string]pluginAgentDef{}
	for _, vendor := range vendors {
		if !vendor.IsDir() {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cacheDir, vendor.Name()))
		if err != nil {
			continue
		}
		for _, pl := range plugins {
			if !pl.IsDir() {
				continue
			}
			// cache/<vendor>/<plugin>/<version>/agents/*.md
			versions, err := os.ReadDir(filepath.Join(cacheDir, vendor.Name(), pl.Name()))
			if err != nil {
				continue
			}
			sort.Slice(versions, func(i, j int) bool { return versions[i].Name() < versions[j].Name() })
			for _, ver := range versions {
				if !ver.IsDir() {
					continue
				}
				agentDir := filepath.Join(cacheDir, vendor.Name(), pl.Name(), ver.Name(), "agents")
				files, err := os.ReadDir(agentDir)
				if err != nil {
					continue
				}
				for _, f := range files {
					if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
						continue
					}
					base := strings.TrimSuffix(f.Name(), ".md")
					id := "def:" + pl.Name() + "/" + base
					byID[id] = pluginAgentDef{
						ID: id, Name: base,
						Plugin: pl.Name(), Path: filepath.Join(agentDir, f.Name()),
					}
				}
			}
		}
	}
	out := make([]pluginAgentDef, 0, len(byID))
	for _, d := range byID {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// AdoptAgentCLI 把 PATH 里发现的 CLI 写进名册（一键纳管）
func (t *Toolchain) AdoptAgentCLI(cmd string) error {
	for _, c := range agentCLICandidates {
		if c.Cmd != cmd {
			continue
		}
		enabled := true
		def := AgentDef{
			ID: c.Cmd, Label: c.Label, Kind: AgentCLI, Enabled: &enabled,
			Command: c.Cmd, Args: c.Args, Capabilities: c.Caps,
			Note: "由工具台从本机 PATH 纳管（命令不在工作流里，只在本机名册）",
		}
		if t.agents == nil {
			return fmt.Errorf("引擎没有装配智能体名册")
		}
		return t.agents.Upsert(def)
	}
	return fmt.Errorf("不是已知的智能体 CLI：%s", cmd)
}

// ===== 重载 =====

// ReloadResult 重扫结果
type ReloadResult struct {
	Plugins     int      `json:"plugins"`
	NodeTypes   []string `json:"node_types"`
	Agents      int      `json:"agents"`
	Problems    []string `json:"problems"`
	ReloadCount int      `json:"reload_count"`
	At          string   `json:"at"`
}

// Reload 重新发现三类可执行工具，并把插件节点热注册进引擎。
//
// 为什么不做成"重启引擎"：重启会中断正在跑的运行、丢掉已加载的工作流状态。
// 插件的节点类型表是独立的（hotNodes），整体替换是原子的，跑着的图不受影响
// ——它已经拿到的执行函数还是旧的那份，下一张图用新的。
func (t *Toolchain) Reload() ReloadResult {
	res := ReloadResult{At: time.Now().Format(time.RFC3339)}

	plugins, _ := t.scanNativePlugins()
	execs := map[string]NodeExecutor{}
	sides := map[string]string{}
	for _, p := range plugins {
		plugin := p // 闭包捕获
		typ := plugin.Type()
		execs[typ] = func(_ context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
			out, err := plugin.Execute(upstream, node.Data)
			if err != nil {
				return nil, err
			}
			if out == nil {
				return map[string]interface{}{}, nil
			}
			return out, nil
		}
		if rp, ok := plugin.(*RuntimePlugin); ok {
			if s := rp.Side(); s != "" {
				sides[typ] = s
			}
		}
	}
	if t.engine != nil {
		t.engine.ReplaceHotNodes(execs, sides)
		res.NodeTypes = t.engine.HotNodeTypes()
	}
	res.Plugins = len(plugins)

	if t.agents != nil {
		res.Agents = len(t.agents.Reload())
	}

	t.InvalidateMCP("")

	t.mu.Lock()
	t.pluginProblems = append([]string{}, t.pluginProblems...)
	res.Problems = t.pluginProblems
	t.lastReload = time.Now()
	t.reloadCount++
	res.ReloadCount = t.reloadCount
	t.mu.Unlock()
	return res
}

// ===== 只读资产（正文） =====

// ReadPluginAgentDef 读取插件里的智能体定义正文。
//
// id 形如 def:<plugin>/<file>。路径必须落在已安装插件缓存目录下——
// 这个函数会把读到的内容返回给前端，不做边界检查就等于开了个任意文件读取口子。
func ReadPluginAgentDef(id string) (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	cacheRoot := filepath.Join(home, ".workbuddy", "plugins", "cache")
	for _, def := range pluginAgentDefs() {
		if def.ID != id {
			continue
		}
		abs, err := filepath.Abs(def.Path)
		if err != nil {
			return "", "", err
		}
		if !strings.HasPrefix(abs, filepath.Clean(cacheRoot)+string(filepath.Separator)) {
			return "", "", fmt.Errorf("路径不在插件缓存目录内，拒绝读取")
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return "", "", err
		}
		const maxDoc = 400 << 10
		if len(b) > maxDoc {
			return string(b[:maxDoc]) + "\n\n…（已截断）", abs, nil
		}
		return string(b), abs, nil
	}
	return "", "", fmt.Errorf("没有这个定义：%s", id)
}

// WriteJSONFile 原子写 JSON（插件启停等写操作共用）
func WriteJSONFile(path string, v any) error { return writeJSONFile(path, v) }

// SetAgentEnabled 启停名册里的一个智能体（保留其它字段原样）
func (t *Toolchain) SetAgentEnabled(id string, enabled bool) error {
	if t.agents == nil {
		return fmt.Errorf("引擎没有装配智能体名册")
	}
	def, ok := t.agents.Get(id)
	if !ok {
		return fmt.Errorf("名册里没有这个 id：%s", id)
	}
	def.Enabled = &enabled
	if err := t.agents.Upsert(def); err != nil {
		return err
	}
	t.agents.Reload()
	return nil
}

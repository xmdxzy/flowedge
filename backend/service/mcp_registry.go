package service

// MCP 服务器配置的发现与治理。
//
// 问题
//
//	用户机器上的 MCP 服务器从来不在一个文件里：WorkBuddy 一份、Cursor 一份、
//	Claude Desktop 一份、项目里再来一份。只读自己那份（data/mcp.json）的结果是
//	「工具台里空空的，可我明明装了十个」。
//
// 做法
//
//	按优先级扫全部已知位置，同名先到先得，其余记为 shadowed 一并返回
//	（用户要知道"这个名字还有别的来源"，不然改错文件会以为没生效）。
//
//	「能改」与「只能看」分开：
//	  data/mcp.json       引擎自有，可增删改
//	  其余                别人的配置文件，只读；启停写进 data/mcp-overrides.json
//
// 为什么不直接改别人的配置：那是 WorkBuddy / Cursor 的资产。改坏了整台机器的
// 智能体工具链一起坏，而用户只是想在这个界面里临时关掉某个服务器。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// MCPConfigSource 一处配置文件
type MCPConfigSource struct {
	Path    string `json:"path"`
	Label   string `json:"label"`
	Managed bool   `json:"managed"` // true = 引擎可写（data/mcp.json）
	Exists  bool   `json:"exists"`
}

// MCPEntry 一个服务器 + 来源信息
type MCPEntry struct {
	MCPServerSpec
	Source   string   `json:"source"`
	Path     string   `json:"path"`
	Managed  bool     `json:"managed"`
	Enabled  bool     `json:"enabled"`
	Shadowed []string `json:"shadowed,omitempty"` // 同名但被忽略的其他来源
}

// MCPOverrides data/mcp-overrides.json：对**外部**配置文件的启停覆盖
type MCPOverrides struct {
	Disabled []string `json:"disabled"`
}

// MCPConfigSources 全部配置来源（按优先级）
//
// 顺序即优先级：引擎自有在最前，用户改自己的那份额度最高。
func MCPConfigSources(baseDir string) []MCPConfigSource {
	if raw := strings.TrimSpace(os.Getenv("FLOWEDGE_MCP_CONFIGS")); raw != "" {
		var out []MCPConfigSource
		for _, p := range strings.Split(raw, ";") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			out = append(out, MCPConfigSource{Path: p, Label: "自定义", Managed: false})
		}
		for i := range out {
			_, err := os.Stat(out[i].Path)
			out[i].Exists = err == nil
		}
		return out
	}

	out := []MCPConfigSource{}
	add := func(path, label string, managed bool) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		_, statErr := os.Stat(abs)
		out = append(out, MCPConfigSource{Path: abs, Label: label, Managed: managed, Exists: statErr == nil})
	}

	if baseDir != "" {
		add(filepath.Join(baseDir, "data", "mcp.json"), "引擎自有", true)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		add(filepath.Join(home, ".workbuddy", "mcp.json"), "WorkBuddy", false)
		add(filepath.Join(home, ".cursor", "mcp.json"), "Cursor", false)
		add(filepath.Join(home, ".claude.json"), "Claude Code", false)
		add(filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json"), "Claude Desktop", false)
		add(filepath.Join(home, ".dsh", "mcp.json"), "DSH 侧", false)
	}
	if baseDir != "" {
		// 项目级：baseDir 通常是 运行/，仓库根在上一级
		root := filepath.Dir(baseDir)
		add(filepath.Join(root, ".mcp.json"), "项目级", false)
		add(filepath.Join(root, ".workbuddy", "mcp.json"), "项目级·WorkBuddy", false)
	}
	return out
}

// rawMCPFile mcp.json 的通用形态。字段兼容各家写法：
// 有的用 url，有的用 serverUrl；有的把 env 写成数组。
type rawMCPFile struct {
	MCPServers map[string]rawMCPEntry `json:"mcpServers"`
	Servers    map[string]rawMCPEntry `json:"servers"` // 少数客户端用 servers
}

type rawMCPEntry struct {
	Type      string            `json:"type"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers"`
	// 各家可能自带启用标记，认它但以 overrides 为准
	Disabled *bool `json:"disabled"`
	Enabled  *bool `json:"enabled"`
}

// LoadMCPServers 扫全部来源，返回去重后的条目与解析问题。
//
// 解析失败的文件不跳过静默：返回 problems，让面板能显示
// 「这个文件我读不了」而不是让用户对着空列表猜。
func LoadMCPServers(baseDir string) ([]MCPEntry, []string, []MCPConfigSource) {
	sources := MCPConfigSources(baseDir)
	overrides := loadMCPOverrides(baseDir)
	disabled := map[string]bool{}
	for _, n := range overrides.Disabled {
		disabled[n] = true
	}

	seen := map[string]int{} // name → out 下标
	out := []MCPEntry{}
	problems := []string{}

	for _, src := range sources {
		if !src.Exists {
			continue
		}
		raw, err := os.ReadFile(src.Path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s（%s）读取失败：%v", src.Label, src.Path, err))
			continue
		}
		var f rawMCPFile
		if err := json.Unmarshal(raw, &f); err != nil {
			problems = append(problems, fmt.Sprintf("%s（%s）不是合法 JSON：%v", src.Label, src.Path, err))
			continue
		}
		servers := f.MCPServers
		if len(servers) == 0 {
			servers = f.Servers
		}
		names := make([]string, 0, len(servers))
		for n := range servers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			e := servers[name]
			spec := MCPServerSpec{
				Name:    name,
				Type:    e.Type,
				Command: e.Command,
				Args:    e.Args,
				Env:     e.Env,
				URL:     firstNonEmpty(e.URL, e.ServerURL),
				Headers: e.Headers,
			}
			enabled := true
			if e.Disabled != nil && *e.Disabled {
				enabled = false
			}
			if e.Enabled != nil {
				enabled = *e.Enabled
			}
			if disabled[name] {
				enabled = false
			}
			if idx, ok := seen[name]; ok {
				out[idx].Shadowed = append(out[idx].Shadowed, src.Label+"："+src.Path)
				continue
			}
			seen[name] = len(out)
			out = append(out, MCPEntry{
				MCPServerSpec: spec,
				Source:        src.Label,
				Path:          src.Path,
				Managed:       src.Managed,
				Enabled:       enabled,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, problems, sources
}

func mcpOverridesPath(baseDir string) string {
	return filepath.Join(baseDir, "data", "mcp-overrides.json")
}

func loadMCPOverrides(baseDir string) MCPOverrides {
	var o MCPOverrides
	if baseDir == "" {
		return o
	}
	raw, err := os.ReadFile(mcpOverridesPath(baseDir))
	if err != nil {
		return o
	}
	_ = json.Unmarshal(raw, &o)
	return o
}

var mcpWriteMu sync.Mutex

// SetMCPEnabled 启停一个服务器。
//
// 自有服务器直接改 data/mcp.json 的 enabled 字段；外部服务器写 overrides
// （不碰别人的配置文件）。两种都返回真实落盘结果，不做"看起来成功"。
func SetMCPEnabled(baseDir, name string, enabled bool) error {
	mcpWriteMu.Lock()
	defer mcpWriteMu.Unlock()

	entries, _, _ := LoadMCPServers(baseDir)
	var target *MCPEntry
	for i := range entries {
		if entries[i].Name == name {
			target = &entries[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("没有这个 MCP 服务器：%s", name)
	}

	if target.Managed {
		// 读原文件 → 改 enabled → 写回（保留用户写的其它字段）
		path := target.Path
		var f struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &f)
		}
		if f.MCPServers == nil {
			f.MCPServers = map[string]map[string]any{}
		}
		m := f.MCPServers[name]
		if m == nil {
			m = map[string]any{}
		}
		m["enabled"] = enabled
		f.MCPServers[name] = m
		return writeJSONFile(path, f)
	}

	// 外部：写 overrides
	o := loadMCPOverrides(baseDir)
	set := map[string]bool{}
	for _, n := range o.Disabled {
		set[n] = true
	}
	if enabled {
		delete(set, name)
	} else {
		set[name] = true
	}
	o.Disabled = o.Disabled[:0]
	for n := range set {
		o.Disabled = append(o.Disabled, n)
	}
	sort.Strings(o.Disabled)
	if err := os.MkdirAll(filepath.Dir(mcpOverridesPath(baseDir)), 0o755); err != nil {
		return err
	}
	return writeJSONFile(mcpOverridesPath(baseDir), o)
}

// SaveMCPServer 新增/覆盖一个自有服务器（写 data/mcp.json）
func SaveMCPServer(baseDir string, spec MCPServerSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if strings.ContainsAny(spec.Name, " \t/\\") {
		return fmt.Errorf("name 不能含空格或斜杠：%q", spec.Name)
	}
	mcpWriteMu.Lock()
	defer mcpWriteMu.Unlock()

	path := filepath.Join(baseDir, "data", "mcp.json")
	var f struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	if f.MCPServers == nil {
		f.MCPServers = map[string]map[string]any{}
	}
	m := map[string]any{}
	if spec.Type != "" {
		m["type"] = spec.Type
	}
	if spec.Command != "" {
		m["command"] = spec.Command
	}
	if len(spec.Args) > 0 {
		m["args"] = spec.Args
	}
	if len(spec.Env) > 0 {
		m["env"] = spec.Env
	}
	if spec.URL != "" {
		m["url"] = spec.URL
	}
	if len(spec.Headers) > 0 {
		m["headers"] = spec.Headers
	}
	m["enabled"] = true
	f.MCPServers[spec.Name] = m
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSONFile(path, f)
}

// DeleteMCPServer 删除一个自有服务器。外部来源拒绝（含提示该怎么停用）。
func DeleteMCPServer(baseDir, name string) error {
	mcpWriteMu.Lock()
	defer mcpWriteMu.Unlock()

	entries, _, _ := LoadMCPServers(baseDir)
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if !e.Managed {
			return fmt.Errorf("%s 来自 %s（%s），不归引擎管：想让它消失请到那个配置里删，或在工具台里停用", name, e.Source, e.Path)
		}
		path := e.Path
		var f struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		delete(f.MCPServers, name)
		return writeJSONFile(path, f)
	}
	return fmt.Errorf("没有这个 MCP 服务器：%s", name)
}

// FindMCPServer 按名取可用（enabled）的服务器
func FindMCPServer(baseDir, name string) (MCPServerSpec, bool) {
	entries, _, _ := LoadMCPServers(baseDir)
	for _, e := range entries {
		if e.Name == name {
			return e.MCPServerSpec, e.Enabled
		}
	}
	return MCPServerSpec{}, false
}

// writeJSONFile 原子写（先写 .tmp 再 rename），格式统一两空格缩进。
func writeJSONFile(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

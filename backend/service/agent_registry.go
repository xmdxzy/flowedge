package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 外部智能体名册（roster）。
//
// 为什么需要
//
//	FlowEdge 原来的执行单位只有"确定性模块"：出图、转写、合成、门。这类活儿
//	可复现、成本可预测，但也意味着**凡是需要判断的事都进不来**——写文案、审片意见、
//	改代码、查资料，只能人在外面做完再把结果粘进来。
//
//	Raven 的做法是让每个节点绑定「模型 + harness」对，直接把外部 agent 当执行者。
//	这里吸收的是这个**形态**，但砍掉它最危险的那一半（见下）。
//
// 砍掉了什么
//
//	① 图里不许出现 shell 命令。节点只能引用 agent_id，真正的命令行只存在
//	   data/agents.json（本机文件、人写的）。否则一张工作流就能变成任意命令执行器。
//	② 不继承父进程环境。只透传白名单 + 用户在 agents.json 里显式声明的 env。
//	   尤其不透传 http_proxy —— 本机常驻代理，传下去会让 agent 的网络请求全走代理。
//	③ 不做"自动挑选 agent"。选哪个由节点数据写死，不由 LLM 猜。
//	   自动分配是 compose 的事，先把"能可靠地调一个"做对。

// AgentKind 接通方式
type AgentKind string

const (
	AgentCLI  AgentKind = "cli"  // 起子进程（Claude Code / Codex / 任意命令行工具）
	AgentHTTP AgentKind = "http" // OpenAI 兼容 /chat/completions 端点（vLLM、Ollama、托管服务）
)

// AgentDef 一个外部智能体的定义
type AgentDef struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Kind    AgentKind `json:"kind"`
	Enabled *bool     `json:"enabled,omitempty"` // nil = true

	// CLI 方式。模板变量：{{prompt}} / {{workdir}}
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Stdin 非空时把渲染后的内容写进 stdin（有些 CLI 不接受长 prompt 当参数）
	Stdin string `json:"stdin,omitempty"`
	// WorkDir 为空时每个节点用独立临时目录，避免并发 agent 互相踩文件
	WorkDir string `json:"workdir,omitempty"`

	// HTTP 方式（OpenAI 兼容）
	BaseURL      string `json:"base_url,omitempty"`
	Model        string `json:"model,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Credential 凭据名（真实密钥在 data/credentials.json，已加密）
	Credential string `json:"credential,omitempty"`

	TimeoutSec   int               `json:"timeout_sec,omitempty"` // 默认 300
	Env          map[string]string `json:"env,omitempty"`         // 额外透传的环境变量
	Capabilities []string          `json:"capabilities,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// IsEnabled nil 视为启用（手写 json 时不用每次都写 enabled）
func (d AgentDef) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// Timeout 执行超时，默认 5 分钟、上限 1 小时
func (d AgentDef) Timeout() time.Duration {
	s := d.TimeoutSec
	if s <= 0 {
		s = 300
	}
	if s > 3600 {
		s = 3600
	}
	return time.Duration(s) * time.Second
}

// AgentStatus 名册里的一项 + 可用性探测结果。
// 「已配置」与「真的能用」必须分开报：配置了 claude 但没装，面板要能说出来。
type AgentStatus struct {
	AgentDef
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// AgentStore data/agents.json 读写（与 llm-profiles / cards 同一套模式）
type AgentStore struct {
	path string

	mu     sync.RWMutex
	agents []AgentDef
}

type agentFile struct {
	Version int        `json:"_schema"`
	Agents  []AgentDef `json:"agents"`
}

// defaultAgents 首次无文件时给的示例，**全部默认停用**。
//
// 停用是刻意的：装没装、路径在哪，只有本机知道。给一个 enabled=true 的假想命令，
// 首次运行时就会在节点上报"command not found"，比空列表更误导。
func defaultAgents() []AgentDef {
	off := false
	return []AgentDef{
		{
			ID: "claude-code", Label: "Claude Code", Kind: AgentCLI, Enabled: &off,
			Command: "claude", Args: []string{"-p", "{{prompt}}"},
			Capabilities: []string{"code", "refactor"},
			Note:         "填好命令并启用后可用。命令行只存在本文件，不进工作流。",
		},
		{
			ID: "codex", Label: "Codex", Kind: AgentCLI, Enabled: &off,
			Command: "codex", Args: []string{"exec", "{{prompt}}"},
			Capabilities: []string{"code"},
			Note:         "按你本机 codex 的实际子命令调整 args。",
		},
		{
			ID: "local-openai", Label: "本地 OpenAI 兼容端点", Kind: AgentHTTP, Enabled: &off,
			BaseURL: "http://127.0.0.1:8000/v1", Model: "Qwen3-Coder",
			Capabilities: []string{"code"},
			Note:         "vLLM / Ollama / 任意兼容 /chat/completions 的服务。",
		},
	}
}

// LoadAgents 读名册。文件不存在或损坏都退回内置示例，不让引擎起不来。
func LoadAgents(path string) *AgentStore {
	s := &AgentStore{path: path, agents: defaultAgents()}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f agentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s
	}
	if len(f.Agents) > 0 {
		s.agents = f.Agents
	}
	return s
}

// Reload 从磁盘重读名册。
//
// 用途：工具台里改过 agents.json（或一键纳管了本机 CLI）之后，让内存里的名册
// 与文件保持一致，不用重启引擎。文件读不到/坏掉时**保留当前内存状态**——
// 编辑到一半的坏 JSON 不该把已经能用的名册清空。
func (s *AgentStore) Reload() []AgentDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	if raw, err := os.ReadFile(s.path); err == nil {
		var f agentFile
		if json.Unmarshal(raw, &f) == nil && f.Agents != nil {
			s.agents = f.Agents
		}
	}
	out := make([]AgentDef, len(s.agents))
	copy(out, s.agents)
	return out
}

// List 全部（含停用的），按 id 排序保证面板顺序稳定
func (s *AgentStore) List() []AgentDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AgentDef, len(s.agents))
	copy(out, s.agents)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 按 id 取
func (s *AgentStore) Get(id string) (AgentDef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.agents {
		if a.ID == id {
			return a, true
		}
	}
	return AgentDef{}, false
}

// Upsert 新增或覆盖（同 id 视为更新）
func (s *AgentStore) Upsert(def AgentDef) error {
	if strings.TrimSpace(def.ID) == "" {
		return fmt.Errorf("agent id 不能为空")
	}
	if def.Kind != AgentCLI && def.Kind != AgentHTTP {
		return fmt.Errorf("kind 只能是 cli 或 http（收到 %q）", def.Kind)
	}
	if def.Kind == AgentCLI && strings.TrimSpace(def.Command) == "" {
		return fmt.Errorf("cli 类型必须配 command")
	}
	if def.Kind == AgentHTTP && strings.TrimSpace(def.BaseURL) == "" {
		return fmt.Errorf("http 类型必须配 base_url")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.agents {
		if s.agents[i].ID == def.ID {
			s.agents[i] = def
			replaced = true
			break
		}
	}
	if !replaced {
		s.agents = append(s.agents, def)
	}
	return s.persistLocked()
}

// Delete 删除（内置示例被删掉也没关系，Next 会退回空）
func (s *AgentStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.agents[:0]
	found := false
	for _, a := range s.agents {
		if a.ID == id {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return fmt.Errorf("没有这个 agent：%s", id)
	}
	s.agents = out
	return s.persistLocked()
}

func (s *AgentStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(agentFile{Version: 1, Agents: s.agents}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Probe 探测是否真的能用。面板靠它区分「配置了」与「装好了」。
//
// cli  → 在 PATH 里找得到命令（找不到就说找不到，不猜）
// http → TCP 能连上 host:port（不验模型对不对，那需要发真请求，代价太高）
func (s *AgentStore) Probe(ctx context.Context, id string) AgentStatus {
	def, ok := s.Get(id)
	if !ok {
		return AgentStatus{AgentDef: AgentDef{ID: id}, Available: false, Detail: "名册里没有这个 id"}
	}
	st := AgentStatus{AgentDef: def}
	if !def.IsEnabled() {
		st.Detail = "已停用"
		return st
	}

	switch def.Kind {
	case AgentCLI:
		path, err := exec.LookPath(def.Command)
		if err != nil {
			st.Detail = fmt.Sprintf("PATH 里找不到命令 %q（装好或改成绝对路径后再启用）", def.Command)
			return st
		}
		st.Available = true
		st.Detail = path
	case AgentHTTP:
		u, err := url.Parse(def.BaseURL)
		if err != nil || u.Host == "" {
			st.Detail = "base_url 不合法：" + def.BaseURL
			return st
		}
		host := u.Host
		if !strings.Contains(host, ":") {
			if u.Scheme == "https" {
				host += ":443"
			} else {
				host += ":80"
			}
		}
		d := net.Dialer{Timeout: 1500 * time.Millisecond}
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			st.Detail = "连不上 " + host + "（服务没起？）"
			return st
		}
		_ = conn.Close()
		st.Available = true
		st.Detail = "端口可达 " + host
	default:
		st.Detail = "未知 kind：" + string(def.Kind)
	}
	return st
}

// ProbeAll 名册全量探测（面板一次拉完）
func (s *AgentStore) ProbeAll(ctx context.Context) []AgentStatus {
	list := s.List()
	out := make([]AgentStatus, 0, len(list))
	for _, a := range list {
		out = append(out, s.Probe(ctx, a.ID))
	}
	return out
}

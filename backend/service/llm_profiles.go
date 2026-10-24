package service

// 自定义模型（LLM Profile）——让节点能指向本机 Ollama 之外的服务。
//
// 为什么单独一个文件、单独一个存储
// --------------------------------
// `data/models.json` 是**扩散侧权重 bundle**（逻辑角色 → safetensors + 采样参数），
// 与"对话模型走哪个 HTTP 端点"完全是两件事，混在一张表里迟早互相污染。
// 这里单独 `data/llm-profiles.json`，只描述「怎么调一个 OpenAI 兼容端点」。
//
// 密钥不进工作流、不进这个文件
// ----------------------------
// profile 只存**凭据名**（credential），真实密钥在 `data/credentials.json`（已加密）。
// 这样导出工作流 JSON 时不会把 key 带出去——工作流是要给人看、给智能体读的。
// 凭据约定：在「凭据」面板新建一条，headers 里放 `Authorization: Bearer sk-xxx`。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LLMProfile 一个可被节点引用的模型端点。
type LLMProfile struct {
	Name string `json:"name"`
	// Backend ollama / openai。空 = 按 base_url 现场判定（见 ResolveLLMBackend）
	Backend string `json:"backend,omitempty"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// Credential 凭据名（可选）。从凭据里取 Authorization 头注入请求。
	Credential string `json:"credential,omitempty"`
	// Note 人话备注，面板上显示一行
	Note string `json:"note,omitempty"`
}

// LLMProfileStore data/llm-profiles.json 读写。
// 文件不存在等价于空表——首次使用不需要手工建文件。
type LLMProfileStore struct {
	path string

	mu       sync.RWMutex
	profiles []LLMProfile
	def      string
}

type llmProfileFile struct {
	Version  int          `json:"_schema"`
	Default  string       `json:"default,omitempty"`
	Profiles []LLMProfile `json:"profiles"`
}

func LoadLLMProfiles(path string) *LLMProfileStore {
	s := &LLMProfileStore{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s // 不存在就是空表，不报错
	}
	var f llmProfileFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s // 文件坏了也不能让引擎起不来；下次保存会覆盖
	}
	s.profiles = f.Profiles
	s.def = f.Default
	return s
}

func (s *LLMProfileStore) Path() string { return s.path }

func (s *LLMProfileStore) List() []LLMProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LLMProfile, len(s.profiles))
	copy(out, s.profiles)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *LLMProfileStore) Default() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.def
}

func (s *LLMProfileStore) Get(name string) (LLMProfile, bool) {
	if strings.TrimSpace(name) == "" {
		return LLMProfile{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.profiles {
		if p.Name == name {
			return p, true
		}
	}
	return LLMProfile{}, false
}

// Replace 整体替换（面板保存 = 一份完整清单，避免增量合并的分支）。
// 校验在写入前完成：名字唯一、base_url 合法、model 非空。
func (s *LLMProfileStore) Replace(profiles []LLMProfile, def string) error {
	seen := map[string]bool{}
	clean := make([]LLMProfile, 0, len(profiles))
	for i := range profiles {
		p := profiles[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Model = strings.TrimSpace(p.Model)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if p.Name == "" {
			return fmt.Errorf("第 %d 个模型没有名字", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("模型名 %q 重复", p.Name)
		}
		seen[p.Name] = true
		if p.Model == "" {
			return fmt.Errorf("模型 %q 没填 model（要调的具体模型名）", p.Name)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("模型 %q 没填 base_url", p.Name)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("模型 %q 的 base_url 不合法：%s（要 http(s)://host[:port]/v1）", p.Name, p.BaseURL)
		}
		if p.Backend == "" {
			p.Backend = ResolveLLMBackend(p.BaseURL, "")
		}
		clean = append(clean, p)
	}
	if def != "" && !seen[def] {
		return fmt.Errorf("默认模型 %q 不在清单里", def)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = clean
	s.def = def

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(llmProfileFile{Version: 1, Default: def, Profiles: clean}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}

// authTransport 给所有出站请求补 Authorization 头。
//
// 用 Transport 而不是在每个 .Post() 前面加代码：
// LLM 有 6 处请求构造点（chat / stream / tools / unload / vision…），
// 逐处加等于制造 6 个漏写点，而漏写的症状是"某个功能 401，其他都好"。
type authTransport struct {
	base http.RoundTripper
	key  string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.key != "" && req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.key)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// WithAuth 克隆出一个带鉴权的 service（不改动共享实例）。
func (s *LLMService) WithAuth(key string) *LLMService {
	if s == nil {
		return nil
	}
	c := *s
	if c.client == nil {
		c.client = defaultLLMHTTPClient
	}
	cc := *c.client
	cc.Transport = &authTransport{base: cc.Transport, key: key}
	c.client = &cc
	return &c
}

// CloneForProfile 按 profile 造一个独立的 LLMService。
// creds 可为 nil（没有凭据体系时按无鉴权处理）。
func (e *EngineV2) llmServiceForProfile(p LLMProfile) *LLMService {
	if e.llm == nil {
		return nil
	}
	base, model := p.BaseURL, p.Model
	if base == "" {
		base = e.llm.BaseURL
	}
	if model == "" {
		model = e.llm.Model
	}
	svc := &LLMService{
		BaseURL: base,
		Model:   model,
		Backend: p.Backend,
		client:  e.llm.httpClient(),
	}
	if svc.Backend == "" {
		svc.Backend = ResolveLLMBackend(base, "")
	}

	// 凭据：端点清单里的 credential 直接就是 Bearer key（不再经过凭据仓库）
	if p.Credential != "" {
		svc = svc.WithAuth(p.Credential)
	}
	return svc
}

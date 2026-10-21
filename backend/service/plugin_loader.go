package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// 运行期插件加载
//
// 为什么必须做（2026-10-21）：
//
//	插件接口早就成型（Type/Name/Category/Description/Execute），但只有 3 个内置
//	玩具插件，而且**加一个新插件要重编译 33MB 的 exe**。这是"生态广度"最硬的墙：
//	社区不可能为了加一个 20 行的节点去装 Go 工具链。
//
// 两种插件形态，覆盖两种人：
//
//	http   声明一个 HTTP 端点，引擎转发 {input, config} 过去。任何语言都能写
//	       （Python 脚本、Node 服务、甚至 curl 一个 SaaS）。零沙箱风险。
//	script 放一个 main.go，用 yaegi 在进程内解释执行（复用 code 节点的白名单沙箱）。
//	       约定一个纯字符串接口，避免 map 类型跨解释器边界的坑。
//
// 目录结构：
//
//	data/plugins/<任意名>/plugin.json
//	data/plugins/<任意名>/main.go      （script 型）
//
// 加载失败**不能阻断启动**：坏插件只记录错误，其余插件继续加载
// （一个手写错的插件不该让整个引擎起不来）。
const PluginDir = "data/plugins"

// pluginManifest 插件描述
type pluginManifest struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Type        string `json:"type"` // script | http
	Entry       string `json:"entry"`
	URL         string `json:"url"`
	Side        string `json:"side"` // 显存侧别（"" = 不占显存）
	TimeoutMS   int    `json:"timeout_ms"`
}

// RuntimePlugin 运行期插件（同时满足 NodePlugin 接口）
type RuntimePlugin struct {
	manifest pluginManifest
	code     string // script 型
	client   *http.Client
	dir      string
}

func (p *RuntimePlugin) Type() string {
	if p.manifest.Name == "" {
		return "plugin"
	}
	return p.manifest.Name
}

func (p *RuntimePlugin) Name() string {
	if p.manifest.Title != "" {
		return p.manifest.Title
	}
	return p.manifest.Name
}

func (p *RuntimePlugin) Category() string {
	if p.manifest.Category != "" {
		return p.manifest.Category
	}
	return "plugin"
}

func (p *RuntimePlugin) Description() string { return p.manifest.Description }

// Side 显存侧别（引擎注册节点时用；空字符串表示确实不吃显存）
func (p *RuntimePlugin) Side() string { return p.manifest.Side }

// Execute 执行插件
func (p *RuntimePlugin) Execute(input, config map[string]interface{}) (map[string]interface{}, error) {
	switch p.manifest.Type {
	case "http":
		return p.executeHTTP(input, config)
	case "script":
		return p.executeScript(input, config)
	default:
		return nil, fmt.Errorf("插件 %s 的 type=%q 不支持（应为 script 或 http）", p.Type(), p.manifest.Type)
	}
}

// executeHTTP 转发到外部端点。
//
// 请求体固定为 {"input":..., "config":...}，响应必须是 JSON object。
// 超时默认 30s：插件多半是轻量变换，跑很久说明写错了。
func (p *RuntimePlugin) executeHTTP(input, config map[string]interface{}) (map[string]interface{}, error) {
	if strings.TrimSpace(p.manifest.URL) == "" {
		return nil, fmt.Errorf("插件 %s 是 http 型但没配 url", p.Type())
	}
	body, err := json.Marshal(map[string]interface{}{"input": input, "config": config})
	if err != nil {
		return nil, err
	}
	timeout := 30 * time.Second
	if p.manifest.TimeoutMS > 0 {
		timeout = time.Duration(p.manifest.TimeoutMS) * time.Millisecond
	}
	client := p.client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Post(p.manifest.URL, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("插件 %s 请求失败（%s）：%w", p.Type(), p.manifest.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("插件 %s 返回 %d：%.300s", p.Type(), resp.StatusCode, string(raw))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("插件 %s 的响应不是 JSON object：%.200s", p.Type(), string(raw))
	}
	return out, nil
}

// executeScript 用 yaegi 解释执行插件脚本。
//
// 约定（写进文档，别改）：
//
//	func Execute(inputJSON string, configJSON string) (string, error)
//
// 为什么用字符串进字符串出：map[string]interface{} 跨解释器边界时
// 类型对不上（yaegi 里是 interp 的 map 类型），每次都要靠反射搬一遍，
// 出错信息还极难懂。JSON 是最稳的边界。
func (p *RuntimePlugin) executeScript(input, config map[string]interface{}) (map[string]interface{}, error) {
	if strings.TrimSpace(p.code) == "" {
		return nil, fmt.Errorf("插件 %s 是 script 型但脚本为空", p.Type())
	}
	if err := checkCodeSafety(p.code); err != nil {
		return nil, err
	}
	inJSON, _ := json.Marshal(input)
	cfgJSON, _ := json.Marshal(config)

	i := buildCodeInterpreter()
	// 脚本里不写 import（沙箱一次性注入白名单包）
	src := codeImportBlock + "\n" + p.code
	if _, err := i.Eval(src); err != nil {
		return nil, fmt.Errorf("插件 %s 编译失败：%w", p.Type(), err)
	}
	fnVal, err := i.Eval("Execute")
	if err != nil {
		return nil, fmt.Errorf("插件 %s 必须定义 func Execute(inputJSON string, configJSON string) (string, error)：%w", p.Type(), err)
	}
	fn, ok := fnVal.Interface().(func(string, string) (string, error))
	if !ok {
		// 用反射兜底：yaegi 的函数值类型可能不完全等价于宿主签名
		rv := reflect.ValueOf(fnVal.Interface())
		if rv.Kind() != reflect.Func || rv.Type().NumIn() != 2 || rv.Type().NumOut() != 2 {
			return nil, fmt.Errorf("插件 %s 的 Execute 签名不符（需要 func(string,string)(string,error)）", p.Type())
		}
		outs := rv.Call([]reflect.Value{reflect.ValueOf(string(inJSON)), reflect.ValueOf(string(cfgJSON))})
		if !outs[1].IsNil() {
			return nil, fmt.Errorf("插件 %s 执行失败：%v", p.Type(), outs[1].Interface())
		}
		return decodePluginOutput(p.Type(), outs[0].String())
	}
	outStr, execErr := fn(string(inJSON), string(cfgJSON))
	if execErr != nil {
		return nil, fmt.Errorf("插件 %s 执行失败：%w", p.Type(), execErr)
	}
	return decodePluginOutput(p.Type(), outStr)
}

func decodePluginOutput(name, s string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		// 不是 JSON 就当成单个 text 字段——比报错更符合直觉
		return map[string]interface{}{"text": s}, nil
	}
	if out == nil {
		return map[string]interface{}{}, nil
	}
	return out, nil
}

// LoadRuntimePlugins 扫描目录加载全部插件，返回成功加载的插件与失败原因。
//
// 失败不阻断：一个手写坏的插件只影响它自己。
func LoadRuntimePlugins(dir string) ([]NodePlugin, []string) {
	if strings.TrimSpace(dir) == "" {
		dir = PluginDir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// 目录不存在是正常状态（没装任何插件），不是错误
		return nil, nil
	}
	var plugins []NodePlugin
	var errs []string
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 稳定加载顺序，便于复现问题
	for _, name := range names {
		p, err := loadOnePlugin(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s] %v", name, err))
			continue
		}
		plugins = append(plugins, p)
	}
	return plugins, errs
}

func loadOnePlugin(dir string) (*RuntimePlugin, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, fmt.Errorf("缺少 plugin.json")
	}
	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("plugin.json 解析失败：%w", err)
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("plugin.json 缺少 name")
	}
	// name 会作为节点类型出现在画布上，必须像标识符
	for _, r := range m.Name {
		if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return nil, fmt.Errorf("name %q 只能含字母数字下划线连字符", m.Name)
		}
	}
	p := &RuntimePlugin{manifest: m, dir: dir}
	switch m.Type {
	case "http":
		if strings.TrimSpace(m.URL) == "" {
			return nil, fmt.Errorf("http 型插件必须配 url")
		}
	case "script":
		entry := m.Entry
		if entry == "" {
			entry = "main.go"
		}
		if strings.ContainsAny(entry, "/\\") || strings.Contains(entry, "..") {
			return nil, fmt.Errorf("entry 只能是目录内的文件名")
		}
		code, err := os.ReadFile(filepath.Join(dir, entry))
		if err != nil {
			return nil, fmt.Errorf("读取脚本 %s 失败：%w", entry, err)
		}
		// 编译期预检：签名不对的插件在加载时就报，而不是等第一次运行
		if !strings.Contains(string(code), "func Execute(") {
			return nil, fmt.Errorf("脚本必须定义 func Execute(inputJSON string, configJSON string) (string, error)")
		}
		p.code = string(code)
	default:
		return nil, fmt.Errorf("type=%q 不支持（应为 script 或 http）", m.Type)
	}
	return p, nil
}

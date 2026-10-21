package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ComfyUI 提交前预检（/object_info）
//
// 为什么需要它（2026-10-21）：
//
//	改造前引擎从不读 /object_info。模型名写错、节点没装、权重没落位，
//	唯一的表现是 ComfyUI 返回 "400 prompt_outputs_failed_validation"，
//	用户看到的是这一行，看不出是哪个节点的哪个字段错了。
//	实测这条坑在 AGENTS.md §0.1/§0.8 里记了两次（H3 迁权重后「一个模型都看不到」）。
//
// 设计约束：
//
//  1. **不可达 ≠ 空结果**。object_info 拉不到时（服务没起 / 老版本 / 代理劫持）
//     返回 error，调用方降级为「跳过预检」而不是「判定图非法」。
//  2. 只做**能确定**的判断：class_type 缺失、枚举字段的值不在候选列表里。
//     不做类型推导——猜错的预检比没有预检更糟。
//  3. 60 秒缓存。首次 /object_info 可能几 MB，每次提交都拉会拖慢出图。
const objectInfoTTL = 60 * time.Second

// graphIssue 一条预检问题
type graphIssue struct {
	Severity string `json:"severity"` // error | warn
	NodeID   string `json:"node_id,omitempty"`
	Class    string `json:"class_type,omitempty"`
	Field    string `json:"field,omitempty"`
	Value    string `json:"value,omitempty"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

func (i graphIssue) String() string {
	loc := i.NodeID
	if i.Class != "" {
		loc = strings.TrimSpace(loc + " " + i.Class)
	}
	if loc == "" {
		return i.Message
	}
	return loc + "：" + i.Message
}

// objectInfo 精简后的 /object_info
type objectInfo struct {
	nodes map[string]objectNodeInfo
	at    time.Time
}

type objectNodeInfo struct {
	// 枚举型输入：字段名 → 候选值集合（只收字符串枚举，动态/对象型留空）
	enums map[string]map[string]bool
}

type rawObjectNode struct {
	Input struct {
		Required map[string]json.RawMessage `json:"required"`
		Optional map[string]json.RawMessage `json:"optional"`
	} `json:"input"`
}

// 需要校验候选值的权重类字段。这些字段名在 ComfyUI 自定义节点里高度统一。
var weightEnumFields = map[string]bool{
	"ckpt_name":        true,
	"unet_name":        true,
	"clip_name":        true,
	"clip_name1":       true,
	"clip_name2":       true,
	"vae_name":         true,
	"model_name":       true,
	"lora_name":        true,
	"control_net_name": true,
	"style_model_name": true,
}

var (
	objInfoMu    sync.Mutex
	objInfoCache = map[string]*objectInfo{}
)

// fetchObjectInfo 拉 /object_info（带缓存）。返回的 error 表示「探不到」，
// 调用方必须把它与「探到了但没有这个节点」区分开。
func fetchObjectInfo(baseURL string) (*objectInfo, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("ComfyUI 地址为空")
	}
	objInfoMu.Lock()
	if cached, ok := objInfoCache[baseURL]; ok && time.Since(cached.at) < objectInfoTTL {
		objInfoMu.Unlock()
		return cached, nil
	}
	objInfoMu.Unlock()

	// 本地端口必须绕开代理：本机 shell 常驻 http_proxy 时，
	// 请求 127.0.0.1 会被劫持成 502（AGENTS §0.6）。
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/object_info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ComfyUI /object_info 不可达（%s）：%w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ComfyUI /object_info 返回 %d（%s）", resp.StatusCode, baseURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 /object_info 失败：%w", err)
	}
	var raw map[string]rawObjectNode
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 /object_info 失败：%w", err)
	}

	info := &objectInfo{nodes: make(map[string]objectNodeInfo, len(raw)), at: time.Now()}
	for class, node := range raw {
		enums := map[string]map[string]bool{}
		for _, group := range []map[string]json.RawMessage{node.Input.Required, node.Input.Optional} {
			for field, spec := range group {
				if !weightEnumFields[field] {
					continue
				}
				if set := parseEnumCandidates(spec); len(set) > 0 {
					enums[field] = set
				}
			}
		}
		info.nodes[class] = objectNodeInfo{enums: enums}
	}
	objInfoMu.Lock()
	objInfoCache[baseURL] = info
	objInfoMu.Unlock()
	return info, nil
}

// parseEnumCandidates 从 `[["a","b"], {...}]` 里取出候选字符串集合。
// 动态枚举（第一项是对象而非数组）返回空集合——此时不做判断。
func parseEnumCandidates(spec json.RawMessage) map[string]bool {
	var arr []json.RawMessage
	if err := json.Unmarshal(spec, &arr); err != nil || len(arr) == 0 {
		return nil
	}
	var candidates []string
	if err := json.Unmarshal(arr[0], &candidates); err != nil {
		return nil
	}
	if len(candidates) == 0 {
		return nil
	}
	set := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		set[c] = true
	}
	return set
}

// validateGraph 对图做静态校验，返回 error 级与 warn 级问题。
// info 为 nil 时返回一条 warn（表示「没预检成」），绝不当成通过。
func validateGraph(info *objectInfo, graph map[string]interface{}) []graphIssue {
	if info == nil {
		return []graphIssue{{Severity: "warn", Message: "未取得 ComfyUI /object_info，本次跳过模型名与节点校验"}}
	}
	var issues []graphIssue
	// 稳定输出顺序，便于测试与阅读
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		node, ok := graph[id].(map[string]interface{})
		if !ok {
			continue
		}
		class, _ := node["class_type"].(string)
		if class == "" {
			continue
		}
		meta, exists := info.nodes[class]
		if !exists {
			issues = append(issues, graphIssue{
				Severity: "error", NodeID: id, Class: class,
				Message: fmt.Sprintf("ComfyUI 未注册节点类型 %q", class),
				Fix:     "该节点来自自定义节点包，先在 ComfyUI 里安装并重启实例；或在 data/models.json 里换成已装的等价角色",
			})
			continue
		}
		inputs, _ := node["inputs"].(map[string]interface{})
		for field, set := range meta.enums {
			val, ok := inputs[field].(string)
			if !ok || val == "" || set[val] {
				continue
			}
			issues = append(issues, graphIssue{
				Severity: "error", NodeID: id, Class: class, Field: field, Value: val,
				Message: fmt.Sprintf("权重 %q 不在 ComfyUI 的可选列表里（%d 个候选）", val, len(set)),
				Fix:     weightFixHint(field, val, set),
			})
		}
	}
	return issues
}

// weightFixHint 给出「怎么改」而不是只说「错了」。
func weightFixHint(field, value string, set map[string]bool) string {
	cands := make([]string, 0, len(set))
	for c := range set {
		cands = append(cands, c)
	}
	sort.Strings(cands)
	preview := cands
	if len(preview) > 5 {
		preview = preview[:5]
	}
	return fmt.Sprintf("改 data/models.json 里的 %s 为实际文件名（现有：%s）",
		field, strings.Join(preview, ", "))
}

// preflightGraph 对外入口：拉 object_info 并校验。
// 返回值 issues 里 severity=error 的项会导致调用方拒绝提交；
// err != nil 表示探不到，调用方应继续执行但把原因带回（不可达 ≠ 校验通过）。
func preflightGraph(baseURL string, graph map[string]interface{}) (issues []graphIssue, err error) {
	// 逃生开关：某些自定义节点包会返回巨大/畸形 object_info，预检本身不该成为故障源
	if strings.EqualFold(os.Getenv("FLOWEDGE_SKIP_PREFLIGHT"), "1") {
		return nil, nil
	}
	info, ferr := fetchObjectInfo(baseURL)
	if ferr != nil {
		return nil, ferr
	}
	return validateGraph(info, graph), nil
}

// blockingIssues 过滤出必须阻断的问题，并拼成一条可读错误
func blockingIssues(issues []graphIssue) error {
	var msgs []string
	for _, i := range issues {
		if i.Severity != "error" {
			continue
		}
		m := i.String()
		if i.Fix != "" {
			m += "（" + i.Fix + "）"
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return nil
	}
	if len(msgs) > 4 {
		msgs = append(msgs[:4], fmt.Sprintf("……另有 %d 项", len(msgs)-4))
	}
	return fmt.Errorf("工作流预检未通过：%s", strings.Join(msgs, "；"))
}

// InvalidateObjectInfoCache 重量级改动（换权重目录、装节点包）后强制重取
func InvalidateObjectInfoCache() {
	objInfoMu.Lock()
	objInfoCache = map[string]*objectInfo{}
	objInfoMu.Unlock()
}

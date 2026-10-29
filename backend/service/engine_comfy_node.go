package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"flowedge/backend/model"
)

// comfy 节点：跑一张**用户自己提供的 ComfyUI 图**。
//
// 它是画布的"逃生舱"。前面 12 种节点只覆盖我们已经封装好的链路；社区里
// ControlNet 组合、自定义采样器、双阶段放大这些玩法永远封装不完，
// 而用户一旦遇到没封装的玩法就会绕开 FlowEdge 直连 ComfyUI —— 那"统御工作流"
// 就落空了。这个节点把那张图接进来，并且**照旧受管辖**：
// 进 GPU 道排队、显存单侧驻留、节点超时、取消语义、产物归集一个不少。
//
// 刻意不做的三件事（每条都是为了不让它变成第二套系统）：
//  1. **不解释图的语义**。我们知道它在跑一张 ComfyUI 图，不知道它在画什么，
//     所以质量门只看技术项（帧数/时长/响度），不做"画面好不好"的判断。
//  2. **不校验图里的节点与模型是否存在**。那要打 /object_info，而这一步已经在
//     UI 的 dry-run 里做过；运行期再查一次只会把"实例刚被卸载"变成假失败。
//     真缺模型时 ComfyUI 自己会拒绝，我们把它的话原样带出来。
//  3. **不做占位符的隐式兜底**。模板里写了 {{image}} 而节点没拿到图，直接报错；
//     悄悄替换成空串会提交一张画不出东西的图，然后表现为"跑成功了但画面是空的"。
//
// 支持的占位符（只有这几个，多了会变成一门语言）：
//
//	{{prompt}} {{image}} {{width}} {{height}} {{seed}}

const (
	// comfyDefaultTimeout 单张自定义图给 20 分钟：够 1024² 五十步 + 双阶段，
	// 又不至于让一条卡死的任务占用整张卡一小时。
	comfyDefaultTimeout = 20 * time.Minute
	// comfyMaxTimeout 硬上限。自定义图可能含多阶段放大，比常规出图贵得多。
	comfyMaxTimeout = 2 * time.Hour
)

// dataFloat 读节点数值字段（取不到返回默认值，让调用方自己判零）。
func dataFloat(n *model.NodeInstance, key string, def float64) float64 {
	if n == nil {
		return def
	}
	switch v := n.Data[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}

// comfyNodeTimeout 节点超时：data.timeout_min 优先，夹在 [1 分钟, 硬上限]。
func comfyNodeTimeout(n *model.NodeInstance) time.Duration {
	minutes := dataFloat(n, "timeout_min", 0)
	if minutes <= 0 {
		return comfyDefaultTimeout
	}
	d := time.Duration(minutes) * time.Minute
	if d < time.Minute {
		d = time.Minute
	}
	if d > comfyMaxTimeout {
		d = comfyMaxTimeout
	}
	return d
}

// comfyInstanceProfile 归一化实例名："" / "comfyui" → 默认实例，"h3"，"moss"。
func comfyInstanceProfile(n *model.NodeInstance) string {
	switch v := strings.ToLower(strings.TrimSpace(fieldString(n, "instance"))); v {
	case "", "comfyui", "default", "main":
		return ""
	case "h3":
		return "h3"
	case "moss":
		return "moss"
	default:
		return v // 未知实例交给 ensureComfy 报"实例不可用"，不静默退回默认实例
	}
}

// comfyInstanceBaseURL 取实例的 HTTP 端点。空串表示"用默认客户端自己的 BaseURL"。
func (e *EngineV2) comfyInstanceBaseURL(profile string) string {
	if profile == "" {
		return "" // runComfyGraph 会退回 s.BaseURL
	}
	if e.deps == nil {
		return ""
	}
	name := "comfy_h3"
	if profile == "moss" {
		name = "comfy_moss"
	}
	if spec, ok := e.deps.Spec(name); ok {
		return spec.Endpoint
	}
	return ""
}

// resolveComfyGraphText 取要提交的图：内联 JSON 优先，其次 data/comfy_workflows/<name>.json。
func resolveComfyGraphText(n *model.NodeInstance) (text string, source string, err error) {
	if inline := strings.TrimSpace(fieldString(n, "workflow_json")); inline != "" {
		return inline, "内联 workflow_json", nil
	}
	name := strings.TrimSpace(fieldString(n, "template"))
	if name == "" {
		return "", "", fmt.Errorf("comfy 节点需要一个模板名（data/comfy_workflows/<name>.json）或内联 workflow_json")
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	path := filepath.Join(ComfyTemplateDir, name)
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		avail := ListComfyTemplates()
		sort.Strings(avail)
		hint := "（目录里一个模板都没有）"
		if len(avail) > 0 {
			hint = "（可用：" + strings.Join(avail, ", ") + "）"
		}
		return "", "", fmt.Errorf("读不到 ComfyUI 模板 %s：%v%s", path, rerr, hint)
	}
	return string(data), "模板 " + name, nil
}

// comfySubstitutions 收集占位符的值。取不到的留空 —— 由替换函数决定是报错还是忽略。
func comfySubstitutions(n *model.NodeInstance, upstream map[string]interface{}) map[string]string {
	prompt := strings.TrimSpace(fieldString(n, "prompt"))
	if prompt == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
					prompt = strings.TrimSpace(t)
					break
				}
			}
		}
	}
	subs := map[string]string{
		"prompt": prompt,
		"image":  pickImageInput(n, upstream, "image_path"),
	}
	if v := dataFloat(n, "width", 0); v > 0 {
		subs["width"] = strconv.Itoa(int(v))
	}
	if v := dataFloat(n, "height", 0); v > 0 {
		subs["height"] = strconv.Itoa(int(v))
	}
	if v := dataFloat(n, "seed", 0); v > 0 {
		subs["seed"] = strconv.FormatInt(int64(v), 10)
	}
	return subs
}

// comfySupportedPlaceholders 给错误信息用（顺序稳定，便于用户照着改）。
var comfySupportedPlaceholders = []string{"prompt", "image", "width", "height", "seed"}

// substituteComfyPlaceholders 替换 {{key}}。
//
// 两类都报错、都不猜：
//   - 模板用了 {{key}} 但这次没有值 → 报"缺什么输入"，而不是填空串；
//   - 模板里出现了不认识的 {{whatever}} → 报拼写问题，而不是把它当普通文本提交上去
//     （当普通文本的后果是 ComfyUI 那一端拿到字面量花括号，画面莫名其妙）。
func substituteComfyPlaceholders(text string, subs map[string]string) (string, error) {
	out := text
	for _, key := range comfySupportedPlaceholders {
		token := "{{" + key + "}}"
		if !strings.Contains(out, token) {
			continue
		}
		val := strings.TrimSpace(subs[key])
		if val == "" {
			return "", fmt.Errorf("模板用到了 %s，但节点没拿到对应的值：%s", token, comfyValueHint(key))
		}
		out = strings.ReplaceAll(out, token, val)
	}
	// 扫剩下的 {{...}}：不认识的占位符一律点名
	for {
		i := strings.Index(out, "{{")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("模板里有一个没闭合的占位符（从 %q 开始）", truncateForError(out[i:]))
		}
		name := strings.TrimSpace(out[i+2 : i+j])
		return "", fmt.Errorf("模板里有不认识的占位符 {{%s}}（支持：%s）",
			name, strings.Join(comfySupportedPlaceholders, ", "))
	}
	return out, nil
}

func comfyValueHint(key string) string {
	switch key {
	case "prompt":
		return "在节点里填提示词，或连一条文本线进来"
	case "image":
		return "连一条图片线进来，或在节点里填 image_path"
	case "width", "height":
		return "在节点里填宽/高"
	case "seed":
		return "在节点里填 seed（0 表示不用占位符）"
	}
	return "检查节点参数"
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// kindOfMediaPath 按扩展名判产物类型（图/视频/音频）。
// 与 quality_loop 里"PNG 被 ffprobe 当成 1 帧视频"是同一类坑的反面：
// 这里不看容器，只按扩展名，因为 ComfyUI 的 SaveImage/SaveVideo 各写各的后缀。
func kindOfMediaPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".webm", ".mkv", ".mov", ".gif":
		return "video"
	case ".wav", ".mp3", ".flac", ".ogg", ".m4a":
		return "audio"
	}
	return "image"
}

// execComfyNode 执行一张自定义 ComfyUI 图。
func (e *EngineV2) execComfyNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	profile := comfyInstanceProfile(node)
	// ensureComfy 对不认识的实例名会静默退回默认实例——这里必须先拦，
	// 否则用户写错实例名时图被提交到默认实例，表现为「跑成功了但不是我要的实例」。
	switch profile {
	case "", "h3", "moss":
	default:
		return nil, fmt.Errorf("comfy 节点的 instance %q 不是可用实例（支持：comfyui / h3 / moss）", profile)
	}
	release := e.enterSide(sideDiffusion)
	defer release()

	if err := e.ensureComfy(profile); err != nil {
		return nil, err
	}
	if e.t2i == nil {
		return nil, fmt.Errorf("comfy 节点缺少 ComfyUI 客户端（引擎未配置文生图服务）")
	}

	text, source, err := resolveComfyGraphText(node)
	if err != nil {
		return nil, err
	}
	text, err = substituteComfyPlaceholders(text, comfySubstitutions(node, upstream))
	if err != nil {
		return nil, err
	}

	var wf map[string]interface{}
	if jerr := json.Unmarshal([]byte(text), &wf); jerr != nil {
		return nil, fmt.Errorf("%s 不是合法的 ComfyUI API 工作流 JSON：%v", source, jerr)
	}
	if len(wf) == 0 {
		return nil, fmt.Errorf("%s 是空对象，没有可执行节点", source)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout := comfyNodeTimeout(node)
	base := e.comfyInstanceBaseURL(profile)
	path, err := e.t2i.runComfyGraph(base, wf, timeout)
	if err != nil {
		// 把 ComfyUI 的原话带出去：它说"缺 ckpt 文件"比我们说"生成失败"有用得多
		return nil, fmt.Errorf("comfy 节点执行失败（%s）：%w", source, err)
	}
	kind := kindOfMediaPath(path)
	return map[string]interface{}{
		kind:             path,
		"kind":           kind,
		"comfy_source":   source,
		"comfy_instance": profile,
	}, nil
}

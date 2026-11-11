package service

import (
	"fmt"
	"strings"

	"flowedge/backend/model"
)

// 连线校验。
//
// 为什么必须有这一层：v1 的画布能把 `image` 输出连到 `prompt`（text）输入上，
// 图存得下、点运行也能起来，然后跑到中途炸掉；或者更糟——不炸，但那个节点
// 悄悄用了自己的默认值，用户以为"我连了"。**这就是「我明明填了却没生效」的根源。**
//
// 分级是实测调出来的，不是拍脑袋（2026-10-29）：
//
// 一开始把「类型不匹配」定成 error，结果**现网 image_edit 流水线自己被拦下**——
// 它的 `start-1 → up-1` 是一条纯执行顺序依赖（文本接到只收图片的超分节点），
// 运行期无害：引擎把上游产物并进一个 map，节点按 key 取自己要的东西，
// 取不到就用自身字段。所以「类型不匹配」是**死线（dead wire），warn 级**。
//
// 真正该拦的是它掩盖的那个洞：**必填输入既没有兼容的入边、节点自身也没有值**。
// 这种情况一定会失败（v1 里表现为「跑半天才报缺输入图」），定级 error：
//
//	type_mismatch       warn   这条线不会被使用（可忽略，也可改连）
//	unknown_port        warn   handle 名不认识（旧 UI 遗留）
//	unknown_node_type   error  本版本没有这个节点（v1 遗留图）
//	input_unreachable   error  必填输入无来源
//
// 另外一条刻意不做的事：**不做隐式转换**。image 能不能当 text 用？
// 技术上能（描述它），但那是语义决定，不该由连线校验替用户拍板。

// 稳定错误码（前端、文档、测试共用；改名等于破坏协议）。
const (
	IssueTypeMismatch     = "type_mismatch"
	IssueUnknownPortKey   = "unknown_port"
	IssueUnknownNodeType  = "unknown_node_type"
	IssueInputUnreachable = "input_unreachable"
)

// PortIssue 一条连线/端口问题。
type PortIssue struct {
	Code    string   `json:"code"`
	Level   string   `json:"level"` // error | warn
	Message string   `json:"message"`
	Nodes   []string `json:"nodes,omitempty"`
	EdgeID  string   `json:"edge_id,omitempty"`
}

func (i PortIssue) Error() string { return i.Message }

func kindList(kinds []PortKind) string {
	if len(kinds) == 0 {
		return "（无端口）"
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return strings.Join(out, "/")
}

// kindsCompatible 两个端口类型集合是否可连。
// any 兼容一切；同名类型互连；任一侧没有端口声明（控制节点）视为兼容。
func kindsCompatible(src, dst []PortKind) bool {
	hasAny := func(ks []PortKind) bool {
		for _, k := range ks {
			if k == PortAny {
				return true
			}
		}
		return false
	}
	if len(src) == 0 || len(dst) == 0 || hasAny(src) || hasAny(dst) {
		return true
	}
	set := make(map[PortKind]bool, len(src))
	for _, k := range src {
		set[k] = true
	}
	for _, k := range dst {
		if set[k] {
			return true
		}
	}
	return false
}

// dataAliases 节点自身数据里能当某个输入端口用的键。
// 这些名字来自执行体真实读取的键（engine_v2_nodes.go），不是猜的：
// 例如超分读 image_path、文生图读 prompt/prompts、配音读 text。
var dataAliases = map[PortKind][]string{
	PortText:  {"text", "prompt", "prompts", "instruction", "plan"},
	PortImage: {"image_path", "image", "source_image"},
	PortAudio: {"ref_audio", "audio", "audio_path"},
	PortVideo: {"video", "video_path"},
}

// nodeHasOwnValue 节点自身是否已具备该类型的输入值（非空字符串 / 非空数组）。
func nodeHasOwnValue(n *model.NodeInstance, kind PortKind) bool {
	for _, key := range dataAliases[kind] {
		v, ok := n.Data[key]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				return true
			}
		case []interface{}:
			if len(t) > 0 {
				return true
			}
		default:
			if fmt.Sprint(t) != "" {
				return true
			}
		}
	}
	return false
}

// ValidatePorts 校验节点类型已知性、连线类型兼容性、以及必填输入是否可满足。
func ValidatePorts(nodes []model.NodeInstance, edges []model.EdgeInstance) []PortIssue {
	var issues []PortIssue

	byID := make(map[string]*model.NodeInstance, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		byID[n.ID] = n
		if _, ok := nodeContracts[n.Type]; !ok {
			issues = append(issues, PortIssue{
				Code:  IssueUnknownNodeType,
				Level: "error",
				Message: fmt.Sprintf("节点 %s 的类型 %q 在本版本不存在。v1 遗留图（if_else / loop / vision / comfy_workflow 等）"+
					"需要用现有 %d 种节点重建，本版本不做静默转换", n.ID, n.Type, len(nodeContracts)),
				Nodes: []string{n.ID},
			})
		}
	}

	// 每个节点：该类型能接收的端口类型（按兼容边累加）
	satisfied := make(map[string]map[PortKind]bool, len(nodes))

	for _, e := range edges {
		src, okS := byID[e.Source]
		dst, okD := byID[e.Target]
		if !okS || !okD {
			continue // 悬空边由 DAG 校验报，避免同一问题报两遍
		}
		srcKinds, okSK := PortKindsOf(src.Type, true)
		dstKinds, okDK := PortKindsOf(dst.Type, false)
		if !okSK || !okDK {
			continue // 未知类型上面已报
		}

		if e.SourceHandle != "" && !PortDeclared(src.Type, true, e.SourceHandle) {
			issues = append(issues, PortIssue{
				Code: IssueUnknownPortKey, Level: "warn",
				Message: fmt.Sprintf("%s 没有名为 %q 的输出端口（可用：%s）", src.Type, e.SourceHandle, kindList(srcKinds)),
				Nodes:   []string{e.Source}, EdgeID: e.ID,
			})
		}
		if e.TargetHandle != "" && !PortDeclared(dst.Type, false, e.TargetHandle) {
			issues = append(issues, PortIssue{
				Code: IssueUnknownPortKey, Level: "warn",
				Message: fmt.Sprintf("%s 没有名为 %q 的输入端口（可用：%s）", dst.Type, e.TargetHandle, kindList(dstKinds)),
				Nodes:   []string{e.Target}, EdgeID: e.ID,
			})
		}

		if !kindsCompatible(srcKinds, dstKinds) {
			issues = append(issues, PortIssue{
				Code: IssueTypeMismatch, Level: "warn",
				Message: fmt.Sprintf("这条线不会被使用：%s 产出 %s，而 %s 只接受 %s。"+
					"若本意是给它输入，请改连对应类型；若只是想表达执行顺序，可以忽略（image_edit 流水线就是这种）",
					src.Type, kindList(srcKinds), dst.Type, kindList(dstKinds)),
				Nodes:  []string{e.Source, e.Target},
				EdgeID: e.ID,
			})
			continue
		}

		// 兼容的入边：把目标节点对应类型的必填输入标记为已满足
		if satisfied[dst.ID] == nil {
			satisfied[dst.ID] = make(map[PortKind]bool, 2)
		}
		for _, k := range srcKinds {
			if k == PortAny {
				for _, dk := range dstKinds {
					satisfied[dst.ID][dk] = true
				}
				continue
			}
			satisfied[dst.ID][k] = true
		}
		if len(srcKinds) == 0 {
			for _, dk := range dstKinds {
				satisfied[dst.ID][dk] = true
			}
		}
	}

	// 必填输入必须有来源：兼容入边 或 节点自身字段
	for i := range nodes {
		n := &nodes[i]
		contract, ok := nodeContracts[n.Type]
		if !ok {
			continue
		}
		for _, p := range contract.In {
			if !p.Required || p.Kind == PortAny {
				continue
			}
			if satisfied[n.ID][p.Kind] || nodeHasOwnValue(n, p.Kind) {
				continue
			}
			issues = append(issues, PortIssue{
				Code: IssueInputUnreachable, Level: "error",
				Message: fmt.Sprintf("节点 %s（%s）的 %s 输入没有任何来源："+
					"连一条 %s 类型的线进来，或在节点里填上对应字段", n.ID, n.Type, p.Kind, p.Kind),
				Nodes: []string{n.ID},
			})
		}
	}
	return issues
}

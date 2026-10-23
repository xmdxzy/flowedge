package service

import (
	"fmt"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// 图结构评分：不执行、不看产物，只判断「这张图本身合不合格」。
//
// 为什么需要（学 Raven 的 MAOB，但换了对象）
//
//	Raven 的 MAOB 拿生成 DAG 与人工参考图比对（Node F1 / Edge F1 / Exact Match），
//	**在 dispatch 之前**出分 —— 这是它最省钱的地方：一条任务几秒钟，不用起 worker。
//	我们照这个思路做两件事，但分两级，因为它们的前提不同：
//
//	  L0 结构合法性 —— 不需要参考图，任何时候都能算。
//	     契约覆盖、端口兼容、必填有源、无环、非孤立。
//	     这是"这张图能不能跑"的硬门槛，compose 生成完立刻能判。
//
//	  L1 结构相似度 —— 需要「目标 → 参考图」的配对，只有 case 集建好后才能算。
//	     这是"这张图像不像人画的"，衡量生成质量而非可执行性。
//
//	分两级的理由：L0 现在就能用（不需要任何人定片子），L1 要等 case 集。
//	把两者混成一个分数，会让"图能跑但画得丑"和"画得像但跑不起来"看不出区别。

// GraphScore 一张图的结构评分
type GraphScore struct {
	Nodes  int `json:"nodes"`
	Edges  int `json:"edges"`
	Layers int `json:"layers"`

	// 分项（每项 0~1，可直接展示，也能定位到具体原因）
	ContractCoverage  float64 `json:"contractCoverage"`  // 有契约的模块占比
	PortCompatibility float64 `json:"portCompatibility"` // 端口兼容的边占比（无边记 1）
	Wiring            float64 `json:"wiring"`            // 接线完整度：非孤立、必填有源
	Acyclic           float64 `json:"acyclic"`           // 无环记 1，有环记 0

	// 总评（加权，见 graphScoreWeights）
	Score float64 `json:"score"`
	Legal bool    `json:"legal"` // 硬门槛：无环 + 无端口冲突 + 契约全覆盖

	// 明细（每条都指到具体节点/边，不写"存在若干问题"）
	ContractMissing []string `json:"contractMissing,omitempty"`
	PortMismatches  []string `json:"portMismatches,omitempty"`
	Orphans         []string `json:"orphans,omitempty"`
	RequiredNoFeed  []string `json:"requiredNoFeed,omitempty"`
	Cycles          []string `json:"cycles,omitempty"`
}

// 权重：契约与端口是"能不能跑"，接线是"跑得对不对"，无环是一票否决式但有独立权重。
// 三个 0.3 + 一个 0.1 = 1.0，改了要一起改（有测试盯）。
var graphScoreWeights = struct {
	Contract, Ports, Wiring, Acyclic float64
}{0.3, 0.3, 0.3, 0.1}

// ScoreGraph 算一张图的 L0 结构评分（不需要参考图）
func ScoreGraph(nodes []model.NodeInstance, edges []model.EdgeInstance) GraphScore {
	s := GraphScore{Nodes: len(nodes), Edges: len(edges)}
	if len(nodes) == 0 {
		return s
	}

	byID := make(map[string]*model.NodeInstance, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	incoming := make(map[string][]model.EdgeInstance)
	for _, e := range edges {
		incoming[e.Target] = append(incoming[e.Target], e)
	}

	// 1) 契约覆盖
	declared := 0
	for i := range nodes {
		if _, ok := ModuleContractOf(nodes[i].Type); ok {
			declared++
		} else {
			s.ContractMissing = append(s.ContractMissing,
				fmt.Sprintf("%s(%s)", nodes[i].ID, nodes[i].Type))
		}
	}
	s.ContractCoverage = float64(declared) / float64(len(nodes))

	// 2) 端口兼容：逐条边问 PortKindAccepts（不写节点白名单，与 preflight 同一条规则）
	//
	// 关键语义：**handle 为空时不能拿"第一个端口"冒充**。
	// 画布上不指定 handle 就是"随便接"——引擎运行期正是这么做的（否则那些图根本跑不起来）。
	// 所以空 handle 要按"上游任一输出 × 下游任一输入，能配上一对就算合法"来判。
	// 2026-10-23 实测踩到：按"第一个端口"判，5 条真实跑通的图（含已出片的「修士斗法」）
	// 全被判端口冲突 —— 单元测试全绿，但工具是错的。
	portOK := 0
	for _, e := range edges {
		src, sok := byID[e.Source]
		dst, dok := byID[e.Target]
		if !sok || !dok {
			s.PortMismatches = append(s.PortMismatches,
				fmt.Sprintf("%s→%s（端点不存在）", e.Source, e.Target))
			continue
		}
		// 2b) 互斥端口的源（文件源这类）：契约声明多个输出、运行期只填 kind 那一路。
		// 边明确指定了下游端口时，这一层可以静态判出来 —— 否则要等到运行时
		// 由下游报「没有 audio 输入」，那时错误已经离原因很远了。
		if msg, conflict := exclusiveSourceConflict(src, dst, e.TargetHandle); conflict {
			s.PortMismatches = append(s.PortMismatches, msg)
			continue
		}
		ok, outDesc, inDesc := edgePortsCompatible(src.Type, dst.Type, e.SourceHandle, e.TargetHandle)
		if ok {
			portOK++
		} else {
			s.PortMismatches = append(s.PortMismatches,
				fmt.Sprintf("%s(%s:%s)→%s(%s:%s)", src.ID, src.Type, outDesc, dst.ID, dst.Type, inDesc))
		}
	}
	if len(edges) == 0 {
		s.PortCompatibility = 1 // 单节点图没有边，不该被扣分
	} else {
		s.PortCompatibility = float64(portOK) / float64(len(edges))
	}

	// 3) 接线完整度：只看「必填输入有没有来源」
	//
	// 孤立节点单独记进 Orphans 供排查，但**不单独扣分**：
	// 输入全是可选的节点靠参数自给是合法接线（tts 的文本就常写在参数里），
	// 给它扣分会让"能跑通的图"永远拿不到满分，评分器就失去定标意义。
	// 2026-10-23 实测：wf2/wf3 两条跑通过的真实图正因为这个被扣了 0.1。
	problems := 0
	for i := range nodes {
		n := &nodes[i]
		c, ok := ModuleContractOf(n.Type)
		if !ok {
			continue // 没契约的已在契约项扣过，不重复罚
		}
		if c.Role != RoleSource && len(incoming[n.ID]) == 0 && len(nodes) > 1 {
			s.Orphans = append(s.Orphans, fmt.Sprintf("%s(%s)", n.ID, n.Type))
		}
		provided := map[string]bool{}
		for _, e := range incoming[n.ID] {
			provided[resolveInputPort(n.Type, e.TargetHandle)] = true
		}
		for _, p := range c.Inputs {
			if p.Required && !provided[p.Name] && !inputCoveredByParam(n.Data, p) {
				s.RequiredNoFeed = append(s.RequiredNoFeed,
					fmt.Sprintf("%s(%s).%s", n.ID, n.Type, p.Name))
				problems++
			}
		}
	}
	s.Wiring = 1 - float64(problems)/float64(len(nodes))
	if s.Wiring < 0 {
		s.Wiring = 0
	}

	// 4) 环检测：直接复用 spec 的拓扑分层（同一套规则，不另写一份——
	// 两份实现迟早对不上，而"任务书说成环、评分说没成环"是最难解释的那种不一致）
	layers, cycle := topoLayers(nodes, edges)
	s.Layers = len(layers)
	s.Cycles = cycle
	if len(cycle) > 0 {
		s.Acyclic = 0
	} else {
		s.Acyclic = 1
	}

	s.Score = graphScoreWeights.Contract*s.ContractCoverage +
		graphScoreWeights.Ports*s.PortCompatibility +
		graphScoreWeights.Wiring*s.Wiring +
		graphScoreWeights.Acyclic*s.Acyclic

	// 硬门槛：三项必须全满，否则"能跑"不成立（分数还能看，但 Legal 才允许自动执行）
	s.Legal = len(s.ContractMissing) == 0 && len(s.PortMismatches) == 0 && len(s.Cycles) == 0
	return s
}

// edgePortsCompatible 一条边在端口上是否接得上。
//
// handle 有值 → 只认那一个端口；handle 为空 → 认全部端口（画布的"随便接"语义）。
// 返回不合手时的可读描述，便于用户知道该改成哪个端口。
func edgePortsCompatible(srcType, dstType, srcHandle, dstHandle string) (bool, string, string) {
	outs := candidateOutKinds(srcType, srcHandle)
	ins := candidateInKinds(dstType, dstHandle)
	for _, o := range outs {
		for _, i := range ins {
			if PortKindAccepts(o, i) {
				return true, string(o), string(i)
			}
		}
	}
	return false, joinKinds(outs), joinKinds(ins)
}

// candidateOutKinds 该节点在该 handle 下可能的输出 kind
func candidateOutKinds(typ, handle string) []PortKind {
	c, ok := ModuleContractOf(typ)
	if !ok {
		return []PortKind{PortAny} // 没契约就按 any 放行（契约项已单独扣分，不重复罚）
	}
	if strings.TrimSpace(handle) != "" {
		if k := kindOfPort(c.Outputs, handle); k != "" {
			return []PortKind{k}
		}
	}
	out := make([]PortKind, 0, len(c.Outputs))
	for _, p := range c.Outputs {
		out = append(out, p.Kind)
	}
	if len(out) == 0 {
		return []PortKind{PortAny}
	}
	return out
}

// candidateInKinds 该节点在该 handle 下可接受的输入 kind
func candidateInKinds(typ, handle string) []PortKind {
	c, ok := ModuleContractOf(typ)
	if !ok {
		return []PortKind{PortAny}
	}
	if strings.TrimSpace(handle) != "" {
		if k := kindOfPort(c.Inputs, handle); k != "" {
			return []PortKind{k}
		}
	}
	out := make([]PortKind, 0, len(c.Inputs))
	for _, p := range c.Inputs {
		out = append(out, p.Kind)
	}
	if len(out) == 0 {
		return []PortKind{PortAny}
	}
	return out
}

// kindOfPort 按端口名找 kind（端口名可能与 handle 不完全一致，按 Name 与 Kind 都试一次）
func kindOfPort(ports []Port, handle string) PortKind {
	h := strings.TrimSpace(handle)
	for _, p := range ports {
		if p.Name == h {
			return p.Kind
		}
	}
	// 有些画布把 handle 写成 "in-image" / "out-video" 这种形态，取末段再试
	if i := strings.LastIndexAny(h, "-_:"); i >= 0 && i+1 < len(h) {
		tail := h[i+1:]
		for _, p := range ports {
			if p.Name == tail {
				return p.Kind
			}
		}
	}
	return ""
}

func joinKinds(ks []PortKind) string {
	if len(ks) == 0 {
		return "?"
	}
	seen := map[string]bool{}
	var parts []string
	for _, k := range ks {
		if s := string(k); !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "|")
}

// ===== L1：与参考图比结构相似度（需要配对，见 §5.9.4）=====

// GraphShape 一张图的可比对形态。
//
// 为什么不比节点 id：生成图的 id 与参考图毫无关系（一个是 "n1"，一个是 "text2image_3"）。
// 比 id 会得到恒为 0 的 F1，看起来像"生成得极差"，其实只是命名不同。
// 所以比**类型多重集**：参考图用了 3 次 text2image，生成图用了 2 次 → 按数量算 TP。
type GraphShape struct {
	NodeTypes map[string]int
	EdgePairs map[string]int // "源类型→目标类型"
}

// ShapeOf 把图压成可比对形态
func ShapeOf(nodes []model.NodeInstance, edges []model.EdgeInstance) GraphShape {
	sh := GraphShape{NodeTypes: map[string]int{}, EdgePairs: map[string]int{}}
	typ := make(map[string]string, len(nodes))
	for _, n := range nodes {
		sh.NodeTypes[n.Type]++
		typ[n.ID] = n.Type
	}
	for _, e := range edges {
		s, sok := typ[e.Source]
		d, dok := typ[e.Target]
		if !sok || !dok {
			continue
		}
		sh.EdgePairs[s+"→"+d]++
	}
	return sh
}

// StructureF1 与参考图比较，返回 Node F1 / Edge F1 与各项明细。
//
// 用多重集的最小计数做 TP —— 这是标准做法，且可解释：
// 多画了的算 FP（画蛇添足），漏画的算 FN（缺环节）。
func StructureF1(got, want GraphShape) (nodeF1, edgeF1 float64, detail map[string]interface{}) {
	nTP, nFP, nFN := multisetMatch(got.NodeTypes, want.NodeTypes)
	eTP, eFP, eFN := multisetMatch(got.EdgePairs, want.EdgePairs)

	nodeF1 = f1(nTP, nFP, nFN)
	edgeF1 = f1(eTP, eFP, eFN)

	detail = map[string]interface{}{
		"nodes":        map[string]int{"tp": nTP, "fp": nFP, "fn": nFN},
		"edges":        map[string]int{"tp": eTP, "fp": eFP, "fn": eFN},
		"missingNodes": missingKeys(got.NodeTypes, want.NodeTypes),
		"extraNodes":   missingKeys(want.NodeTypes, got.NodeTypes),
	}
	return nodeF1, edgeF1, detail
}

func multisetMatch(got, want map[string]int) (tp, fp, fn int) {
	for k, g := range got {
		w := want[k]
		if g < w {
			tp += g
			fn += w - g
		} else {
			tp += w
			fp += g - w
		}
	}
	for k, w := range want {
		if _, ok := got[k]; !ok {
			fn += w
		}
	}
	return
}

func f1(tp, fp, fn int) float64 {
	if tp == 0 {
		return 0
	}
	p := float64(tp) / float64(tp+fp)
	r := float64(tp) / float64(tp+fn)
	return 2 * p * r / (p + r)
}

// missingKeys got 里缺的键（want 有、got 没有或更少）
func missingKeys(got, want map[string]int) []string {
	var out []string
	for k, w := range want {
		if got[k] < w {
			out = append(out, fmt.Sprintf("%s×%d", k, w-got[k]))
		}
	}
	sort.Strings(out)
	return out
}

// Summarize 一行人话，面板/CLI 直接显示
func (s GraphScore) Summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "结构分 %.2f（%d 节点 / %d 边 / %d 层）", s.Score, s.Nodes, s.Edges, s.Layers)
	if s.Legal {
		b.WriteString(" · 可执行")
	} else {
		b.WriteString(" · 不可执行")
	}
	if len(s.ContractMissing) > 0 {
		fmt.Fprintf(&b, " · 无契约 %d", len(s.ContractMissing))
	}
	if len(s.PortMismatches) > 0 {
		fmt.Fprintf(&b, " · 端口冲突 %d", len(s.PortMismatches))
	}
	if len(s.Orphans) > 0 {
		fmt.Fprintf(&b, " · 孤立 %d", len(s.Orphans))
	}
	if len(s.Cycles) > 0 {
		fmt.Fprintf(&b, " · 成环 %d", len(s.Cycles))
	}
	return b.String()
}

// TopologyLayers 对外暴露拓扑分层（内部复用 spec 的 topoLayers）。
// 构图后自动布局要用它 —— 布局与任务书必须基于同一套分层，
// 否则"任务书说第 2 层、画布画在第 3 列"这种不一致会反复出现。
func TopologyLayers(nodes []model.NodeInstance, edges []model.EdgeInstance) ([][]string, []string) {
	return topoLayers(nodes, edges)
}

// exclusiveSourceConflict 判「多输出源」的实际类型与下游端口是否对得上。
//
// 判据刻意不写节点类型字面量，而是靠**契约形态**：
//   Role == source（无输入）+ 输出多于一个 + 带 kind 参数
// 这正好是"运行期按 kind 只输出一路"的定义。以后再有同类模块，自动被这条覆盖。
//
// 只在 handle 非空时判：空 handle 的语义是"随便接"，引擎运行期也真的会自己找，
// 硬判会误伤（2026-10-23 就因为这个把 5 条真实图判成端口冲突）。
func exclusiveSourceConflict(src, dst *model.NodeInstance, dstHandle string) (string, bool) {
	c, ok := ModuleContractOf(src.Type)
	if !ok || c.Role != RoleSource || len(c.Outputs) <= 1 {
		return "", false
	}
	hasKind := false
	for _, p := range c.KeyParams {
		if p.Key == "kind" {
			hasKind = true
			break
		}
	}
	if !hasKind || strings.TrimSpace(dstHandle) == "" {
		return "", false
	}
	dc, ok := ModuleContractOf(dst.Type)
	if !ok {
		return "", false
	}
	want := kindOfPort(dc.Inputs, dstHandle)
	if want == "" || want == PortAny {
		return "", false
	}
	got := PortKind(SourceKindOf(src))
	if PortKindAccepts(got, want) {
		return "", false
	}
	return fmt.Sprintf("%s(%s:kind=%s)→%s(%s:%s)（这个源只输出 %s，接不到 %s 端口）",
		src.ID, src.Type, got, dst.ID, dst.Type, want, got, want), true
}

package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"flowedge/backend/model"
)

// 任务书回写（Compose）：把「编辑」变成对节点数据的真实改动。
//
// 为什么需要
//
//	spec 以前只能出不能进（GET 只有导出）。智能体拿到任务书能读懂图，却改不动，
//	于是"按任务书调参"只能靠人在画布上手点。这一步补上回路，才谈得上探针—组合。
//
// 为什么不做成"随便改"
//
//	任务书是**执行契约**，不是草稿。四条硬拦（见 EditProblem.Code 注释）分别守住：
//	图结构（不许改 id/type/position）、并发（不许覆盖别人的改动）、
//	门（放宽验收必须留名）、原子性（一批里有一条不过，整批不生效）。
//
//	半批生效最危险：图会停在一个谁都没设计过的状态，而每一条改动单独看都"成功"了。

// 编辑问题码。
const (
	EditNodeNotFound   = "node_not_found"    // 404
	EditPathNotAllowed = "path_not_allowed"  // 403 改了 data 之外的字段
	EditGateLocked     = "gate_param_locked" // 403 动门阈值却没说清为什么
	EditFromMismatch   = "from_mismatch"     // 409 现场值与声明的 from 不一致
	EditBadValue       = "bad_edit"          // 400 编辑本身不成立（空 path、from==to 等）
	EditNodeNotObj     = "node_not_object"   // 400 节点数据不是对象，无法按路径改
)

// EditProblem 一条编辑为什么没通过
type EditProblem struct {
	Index  int    `json:"index"`
	NodeID string `json:"nodeId,omitempty"`
	Path   string `json:"path,omitempty"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Status int    `json:"-"`
}

// SpecEdit 一条编辑。
//
// From 是**必填**的现场值：它不是冗余，是乐观锁。智能体读到任务书之后、提交之前，
// 人可能已经在画布上改过了；没有 From 就无法区分"我基于旧值改"和"我要覆盖别人的改动"。
type SpecEdit struct {
	NodeID string      `json:"nodeId"`
	Path   string      `json:"path"` // 只允许 data.<key>，可点分下钻
	From   interface{} `json:"from"`
	To     interface{} `json:"to"`
	Reason string      `json:"reason,omitempty"`
}

// ApplyOptions 控制一次回写
type ApplyOptions struct {
	// AllowGateChange 允许改动门层参数（放宽验收）。必须同时给 Reason，否则拒绝：
	// 门槛是"牙齿"，放宽它必须留下"谁、凭什么"，否则它就不再是门。
	AllowGateChange bool
	DryRun          bool
}

// ApplyResult 回写结果
type ApplyResult struct {
	Applied []AppliedEdit        `json:"applied"`
	Nodes   []model.NodeInstance `json:"-"`
}

// AppliedEdit 一条生效的编辑（便于回执与审计）
type AppliedEdit struct {
	Index  int         `json:"index"`
	NodeID string      `json:"nodeId"`
	Path   string      `json:"path"`
	Old    interface{} `json:"old"`
	New    interface{} `json:"new"`
}

// ApplySpecEdits 把编辑应用到节点列表上（纯函数，不碰 DB、不落盘）。
//
// 返回 problems 非空时，nodes 原样返回 —— 调用方必须把整批视为失败。
func ApplySpecEdits(nodes []model.NodeInstance, edits []SpecEdit, opts ApplyOptions) ([]model.NodeInstance, []AppliedEdit, []EditProblem) {
	var problems []EditProblem
	var applied []AppliedEdit

	if len(edits) == 0 {
		return nodes, nil, []EditProblem{{
			Index: -1, Code: EditBadValue, Status: http.StatusBadRequest,
			Detail: "edits 为空：没有要改的东西",
		}}
	}

	// 拷贝一层：先在副本上改，全部通过才交出去，避免"校验到一半已经写脏"
	work := make([]model.NodeInstance, len(nodes))
	copy(work, nodes)
	for i := range work {
		d := make(map[string]interface{}, len(work[i].Data))
		for k, v := range work[i].Data {
			d[k] = v
		}
		work[i].Data = d
	}

	gateKeys := gateKeySet()

	for idx, e := range edits {
		p := EditProblem{Index: idx, NodeID: e.NodeID, Path: e.Path}

		if strings.TrimSpace(e.NodeID) == "" || strings.TrimSpace(e.Path) == "" {
			p.Code, p.Status = EditBadValue, http.StatusBadRequest
			p.Detail = "nodeId 与 path 都必须非空"
			problems = append(problems, p)
			continue
		}

		// 只允许改 data.*：id/type/position 是图结构，改了等于换一张图，
		// 那不是"调参"，必须走正常编辑流程并重新过契约检查。
		if e.Path != "data" && !strings.HasPrefix(e.Path, "data.") {
			p.Code, p.Status = EditPathNotAllowed, http.StatusForbidden
			p.Detail = "只允许改 data.* —— id/type/position 属于图结构，改它等于换一张图"
			problems = append(problems, p)
			continue
		}

		ni := -1
		for i := range work {
			if work[i].ID == e.NodeID {
				ni = i
				break
			}
		}
		if ni < 0 {
			p.Code, p.Status = EditNodeNotFound, http.StatusNotFound
			p.Detail = "图里没有这个节点"
			problems = append(problems, p)
			continue
		}

		segs := strings.Split(strings.TrimPrefix(e.Path, "data."), ".")
		key := segs[0]

		// 门层参数：默认锁死，允许但必须说明理由（放宽验收必须留名）
		if gateKeys[key] && !opts.AllowGateChange {
			p.Code, p.Status = EditGateLocked, http.StatusForbidden
			p.Detail = fmt.Sprintf("「%s」是门层参数，默认禁止由任务书回写；确需放宽请带 allowGateChange=true 并写明 reason", key)
			problems = append(problems, p)
			continue
		}
		if gateKeys[key] && strings.TrimSpace(e.Reason) == "" {
			p.Code, p.Status = EditBadValue, http.StatusBadRequest
			p.Detail = "改动门层参数必须写明 reason（审计要求：谁、凭什么放宽了验收）"
			problems = append(problems, p)
			continue
		}

		old, found := deepGet(work[ni].Data, segs)
		if !found {
			p.Code, p.Status = EditFromMismatch, http.StatusConflict
			p.Detail = "路径不存在，无法核对 from"
			problems = append(problems, p)
			continue
		}
		// from 必填：它是乐观锁，防覆盖别人在这期间的改动
		if e.From == nil {
			p.Code, p.Status = EditBadValue, http.StatusBadRequest
			p.Detail = "from 必填（用来核对现场值，防覆盖别人的改动）"
			problems = append(problems, p)
			continue
		}
		if !jsonEqual(old, e.From) {
			p.Code, p.Status = EditFromMismatch, http.StatusConflict
			p.Detail = fmt.Sprintf("现场值是 %s，与 from=%s 不一致：这期间图被改过了，请重新取任务书",
				jsonText(old), jsonText(e.From))
			problems = append(problems, p)
			continue
		}

		if err := deepSet(work[ni].Data, segs, e.To); err != nil {
			p.Code, p.Status = EditNodeNotObj, http.StatusBadRequest
			p.Detail = err.Error()
			problems = append(problems, p)
			continue
		}
		applied = append(applied, AppliedEdit{Index: idx, NodeID: e.NodeID, Path: e.Path, Old: old, New: e.To})
	}

	if len(problems) > 0 {
		// 原子性：一条不过，整批作废。半批生效会把图留在没人设计过的状态。
		return nodes, nil, problems
	}
	return work, applied, nil
}

// gateKeySet 门层参数键集合（从 spec.go 的词汇表派生，不另立一份）
func gateKeySet() map[string]bool {
	out := make(map[string]bool)
	for _, keys := range gateParamKeys {
		for _, k := range keys {
			out[k] = true
		}
	}
	return out
}

// deepGet 按路径取值
func deepGet(root map[string]interface{}, segs []string) (interface{}, bool) {
	var cur interface{} = root
	for _, s := range segs {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[s]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// deepSet 按路径赋值（中间层缺失则建对象）
func deepSet(root map[string]interface{}, segs []string, val interface{}) error {
	if len(segs) == 0 {
		return fmt.Errorf("路径为空")
	}
	cur := root
	for i, s := range segs {
		if i == len(segs)-1 {
			cur[s] = val
			return nil
		}
		next, exists := cur[s]
		if !exists || next == nil {
			m := map[string]interface{}{}
			cur[s] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]interface{})
		if !ok {
			return fmt.Errorf("路径 %q 不是对象，无法下钻", s)
		}
		cur = m
	}
	return nil
}

// jsonEqual 按 JSON 语义比较（124 与 124.0 视为相等）。
//
// 直接 == 比不了：从请求体解出来的数字是 float64，从图里读出来的可能是 int。
// 用 JSON 序列化统一，比写一堆类型分支可靠。
func jsonEqual(a, b interface{}) bool {
	return jsonText(a) == jsonText(b)
}

func jsonText(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

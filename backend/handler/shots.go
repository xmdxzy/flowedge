package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
)

// 单镜（镜头级）重跑
//
// 解决的问题（engine-gap-audit R1 的最后一环）：
//
//	以前的选项只有两个——整条重跑（20 分钟），或者手动去改工作流节点加
//	`reroll: 3` 再点「从该节点继续」。用户真正想要的是「只重拍第 3 镜」。
//
// 关键设计：**不修改用户保存的工作流**。
// reroll 只写进这次调用的内存节点图（resumeFromWithOverride），
// 否则一次临时操作会把 `reroll: 3` 永久留在工作流里——以后每次正常运行都会重拍第 3 镜，
// 而且没人记得为什么。
//
// POST /api/runs/:runId/shots/:index/reroll?node_id=<可选>
//
// index 从 1 开始（与分镜表里的镜号一致，不是数组下标）。

// RerollShot 重拍某一镜
func (h *WorkflowHandler) RerollShot(c *gin.Context) {
	runID := c.Param("runId")
	idxStr := c.Param("index")
	idx, err := strconv.Atoi(strings.TrimSpace(idxStr))
	if err != nil || idx < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜号必须是 >=1 的整数（与分镜表的镜号一致，不是数组下标）"})
		return
	}

	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if run.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "该运行仍在进行中，先等它结束或取消"})
		return
	}
	var wf model.Workflow
	if err := h.db.First(&wf, run.WorkflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	nodesJSON, nodeID, shotCount, err := injectReroll([]byte(wf.Nodes), c.Query("node_id"), idx)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Request.URL.RawQuery = "" // 避免 node_id 之类的参数影响下游解析
	h.resumeFromWithOverride(c, nodeID, nodesJSON)
	_ = shotCount
}

// ListShots GET /api/runs/:runId/shots?node_id=
//
// 返回本次运行里每一镜的状态（成功/失败/复用）、文件、耗时。
// 「第 3 镜失败」不再需要去翻日志。
func (h *WorkflowHandler) ListShots(c *gin.Context) {
	runID := c.Param("runId")
	id, err := strconv.ParseUint(runID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	nodeID := c.Query("node_id")
	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	// node_id 为空时，从该 run 的节点状态里找出所有 shotlist 节点
	nodeIDs := []string{nodeID}
	if nodeID == "" {
		nodeIDs = nil
		var states []model.NodeInstance
		if len(run.NodeStates) > 0 {
			_ = json.Unmarshal(run.NodeStates, &states)
		}
		for _, n := range states {
			if n.Type == "shotlist" {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
	}

	out := map[string]interface{}{}
	for _, nid := range nodeIDs {
		if nid == "" {
			continue
		}
		out[nid] = h.engine.Shots(uint(id), nid)
	}
	if len(out) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "该运行里没有分镜节点（shotlist）"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": id, "shots": out})
}

// injectReroll 在节点图里给 shotlist 节点写入 reroll=idx，返回新 JSON 与命中的节点 id。
//
// 用 []map[string]interface{} 而不是强类型结构：节点图里前端会存
// position / style / measured 等一堆字段，强类型解析会把这些字段全丢掉，
// 续跑时前端拿到的工作流就变形了。
func injectReroll(nodesRaw []byte, wantNodeID string, idx int) ([]byte, string, int, error) {
	var nodes []map[string]interface{}
	if err := json.Unmarshal(nodesRaw, &nodes); err != nil {
		return nil, "", 0, fmt.Errorf("节点数据解析失败：%w", err)
	}
	if len(nodes) == 0 {
		return nil, "", 0, fmt.Errorf("工作流没有节点")
	}
	target := -1
	for i, n := range nodes {
		typ, _ := n["type"].(string)
		id, _ := n["id"].(string)
		if typ != "shotlist" {
			continue
		}
		if wantNodeID != "" && id != wantNodeID {
			continue
		}
		target = i
		break
	}
	if target < 0 {
		if wantNodeID != "" {
			return nil, "", 0, fmt.Errorf("节点 %s 不是 shotlist 节点或不存在", wantNodeID)
		}
		return nil, "", 0, fmt.Errorf("该工作流没有 shotlist（分镜出片）节点，无法按镜重跑")
	}

	node := nodes[target]
	nodeID, _ := node["id"].(string)
	data, ok := node["data"].(map[string]interface{})
	if !ok {
		data = map[string]interface{}{}
		node["data"] = data
	}
	shotCount := countShots(data)
	if shotCount > 0 && idx > shotCount {
		return nil, "", shotCount, fmt.Errorf("镜号 %d 超出范围（该节点共 %d 镜）", idx, shotCount)
	}
	// 保留已有的其它 reroll 声明（用户可能在节点里写了 1,3，再点重拍第 5 镜）
	prev, _ := data["reroll"].(string)
	if strings.TrimSpace(prev) == "" {
		data["reroll"] = strconv.Itoa(idx)
	} else {
		merged := prev + "," + strconv.Itoa(idx)
		data["reroll"] = merged
	}

	out, err := json.Marshal(nodes)
	if err != nil {
		return nil, "", 0, err
	}
	return out, nodeID, shotCount, nil
}

// countShots 从分镜节点配置里数镜数（shots 数组或 plan JSON 字符串）
func countShots(data map[string]interface{}) int {
	if shots, ok := data["shots"].([]interface{}); ok {
		return len(shots)
	}
	if plan, ok := data["plan"].(string); ok && strings.TrimSpace(plan) != "" {
		var parsed struct {
			Shots []interface{} `json:"shots"`
		}
		s := plan
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if j := strings.LastIndex(s, "}"); j >= 0 {
			s = s[:j+1]
		}
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return len(parsed.Shots)
		}
	}
	return 0
}

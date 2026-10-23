package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 证据聚合与登记出口。
//
// 两个端点回答两个不同问题：
//   GET  /api/evidence/summary   —— 「我这批 run 到底成不成」→ 三数 + 门级诊断
//   POST /api/evidence/register  —— 「把诊断登记成一条候选」→ 交给序策 registry
//
// 为什么要分两步而不是自动登记：诊断是**规则推出来的**，不是人确认过的结论。
// 自动往进化登记表里塞"机器猜的原因"，会让 registry 被低质量候选淹没，
// 而它一旦被淹没，人就再也不会去看它了 —— 那比没有 registry 更糟。
type EvidenceHandler struct {
	db       *gorm.DB
	governor *service.GovernorGate // 复用同一套「问不到就放行」的 HTTP 配置
	registry string                // 序策 registry 地址（默认同 governor host）
	// wf 复用 workflow handler 的 preflight / StartRun / 版本快照与写图逻辑。
	// 不复制一套：两处写图的代码迟早会在"先快照还是先写"这种顺序上分叉。
	wf *WorkflowHandler
}

func NewEvidenceHandler(db *gorm.DB, gov *service.GovernorGate) *EvidenceHandler {
	base := "http://127.0.0.1:30002"
	if v := strings.TrimSpace(os.Getenv("GOVERNOR_BASE_URL")); v != "" {
		base = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("REGISTRY_BASE_URL")); v != "" {
		base = strings.TrimRight(v, "/")
	}
	return &EvidenceHandler{db: db, governor: gov, registry: base}
}

// Summary GET /api/evidence/summary?limit=500
//
// 数据来源两路合并：
//  1. data/evidence/*.json —— 新数据，带门明细
//  2. workflow_runs 表 —— 历史数据，只有状态（不能让 125 条历史白白丢掉）
// evidence 里已有的 runID 会覆盖历史记录（信息更全）
func (h *EvidenceHandler) Summary(c *gin.Context) {
	limit := 500
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}

	recs, bad, err := service.LoadEvidenceRecords(service.EvidenceDir())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读证据目录失败：" + err.Error()})
		return
	}

	// 历史记录：只补 evidence 里没有的 runID
	have := make(map[uint]bool, len(recs))
	for _, r := range recs {
		have[r.RunID] = true
	}

	var runs []model.WorkflowRun
	h.db.Order("id desc").Limit(limit).Find(&runs)

	// 工作流名：一次查完，别在循环里查库
	wfNames := map[uint]string{}
	var wfs []model.Workflow
	h.db.Select("id", "name").Find(&wfs)
	for _, w := range wfs {
		wfNames[w.ID] = w.Name
	}

	for _, r := range runs {
		if have[r.ID] {
			continue
		}
		var dur int64
		if r.StartedAt != nil && r.EndedAt != nil {
			dur = r.EndedAt.Sub(*r.StartedAt).Milliseconds()
		}
		// runs 表只有 success/failed 这类状态，映射到证据的口径时要分清三件事：
		//   门判不过（failed）—— 需要门明细，历史数据没有
		//   执行中断（aborted）—— 状态就写着 failed，这是**确定的事实**
		//   没测到（unscored）—— 既没门也没跑完
		// 把历史 failed 塞进 unscored 会让 CR_clean 的分母变 0，报出 1.000 的假分数
		// （2026-10-23 用 125 条真实数据验证时抓到的）。
		result := "unscored"
		switch r.Status {
		case "success":
			result = service.EvidencePassed
		case "failed":
			result = "aborted"
		case "paused":
			result = service.EvidencePaused
		case "running", "pending":
			result = "running"
		}
		recs = append(recs, service.RunRecord{
			RunID:      r.ID,
			WorkflowID: r.WorkflowID,
			Workflow:   wfNames[r.WorkflowID],
			Result:     result,
			DurationMs: dur,
			Source:     "history",
		})
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].RunID > recs[j].RunID })
	if len(recs) > limit {
		recs = recs[:limit]
	}

	rep := service.AggregateRuns(recs)
	c.JSON(http.StatusOK, gin.H{
		"report":      rep,
		"badFiles":    bad,
		"evidenceDir": service.EvidenceDir(),
	})
}

// RegisterBody 登记一条候选
type registerBody struct {
	// Diagnosis 必须是 summary 里给出来的那条（前端/agent 直接透传）
	Diagnosis service.Diagnosis `json:"diagnosis"`
	// Hypothesis / Change 允许人改写；留空则用诊断消息兜底
	Hypothesis string   `json:"hypothesis"`
	Change     string   `json:"change"`
	Tags       []string `json:"tags"`
	// WorkflowID 用于带上该工作流的结构分（如果有）
	WorkflowID *uint `json:"workflowId"`
	DryRun     bool  `json:"dryRun"`
}

// Register POST /api/evidence/register
//
// 把一条诊断变成序策 registry 的 candidate。
// **只登记，不晋升** —— 晋升要过序策的门禁，那不在 FlowEdge 的权限内。
func (h *EvidenceHandler) Register(c *gin.Context) {
	var b registerBody
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if b.Diagnosis.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "diagnosis 必填（从 /api/evidence/summary 的 diagnoses 里取一条原样传）"})
		return
	}

	hypothesis := strings.TrimSpace(b.Hypothesis)
	if hypothesis == "" {
		hypothesis = "（规则诊断，待人工确认）" + b.Diagnosis.Message
	}
	change := strings.TrimSpace(b.Change)
	if change == "" {
		change = b.Diagnosis.Suggestion
	}

	evidence := []map[string]interface{}{{
		"observationId": fmt.Sprintf("flowedge:%s:%s", b.Diagnosis.Kind, b.Diagnosis.Metric),
		"project":       "flowedge",
		"check":         b.Diagnosis.Metric,
		"runIds":        b.Diagnosis.Evidence,
	}}
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]interface{}{
		"target":         "workflow",
		"hypothesis":     hypothesis,
		"change":         change,
		"tags":           append([]string{"flowedge", b.Diagnosis.Kind}, b.Tags...),
		"sourceEvidence": evidence,
		"state":          "proposed",
		"createdAt":      now,
	}

	if b.DryRun {
		c.JSON(http.StatusOK, gin.H{"dryRun": true, "payload": payload})
		return
	}

	// fail-open：序策没起时不要假装登记成功，也不要把它变成 FlowEdge 的错误。
	// 明确告诉调用方"没登记上，原因是什么"，附上可直接重放的 payload。
	status, respBody, err := h.postRegistry(c.Request.Context(), "/api/flow/registry/register", payload)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "序策 registry 不可达，本次未登记",
			"detail":  err.Error(),
			"payload": payload,
			"hint":    "启动序策后把 payload 直接 POST 到 " + h.registry + "/api/flow/registry/register 即可补登记",
		})
		return
	}
	if status >= 400 {
		c.JSON(status, gin.H{"error": "registry 拒绝了这条候选", "body": string(respBody), "payload": payload})
		return
	}
	c.JSON(http.StatusOK, gin.H{"registered": true, "registry": h.registry, "response": json.RawMessage(respBody)})
}

// postRegistry 调序策。代理必须显式关闭（本机常驻 http_proxy，否则会拿假 502）
func (h *EvidenceHandler) postRegistry(ctx context.Context, path string, payload interface{}) (int, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.registry+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil || len(buf) > 1<<20 {
			break
		}
	}
	return resp.StatusCode, buf, nil
}

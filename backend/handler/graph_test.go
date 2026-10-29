package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

func newGraphRouter(g *GraphHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/node-catalog", g.Catalog)
	r.GET("/api/workflows", g.ListWorkflows)
	r.POST("/api/workflows", g.CreateWorkflow)
	r.GET("/api/workflows/templates", g.Templates)
	r.GET("/api/workflows/:id", g.GetWorkflow)
	r.PUT("/api/workflows/:id", g.UpdateWorkflow)
	r.DELETE("/api/workflows/:id", g.DeleteWorkflow)
	r.POST("/api/graph/validate", g.Validate)
	r.POST("/api/graph/dry-run", g.DryRun)
	return r
}

func doJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// 模板必须等于「现网产品」：每条流水线都能反向组装成一张可保存、零阻断的图，
// 且每个节点都有坐标（否则打开画布全堆在原点 —— v1 的头号败因）。
//
// 这条测试同时是「画布与流水线同构」的守门人：pipelines.json 改坏、或端口契约
// 与真实连线不符，这里立刻红。
func TestTemplatesAreRunnableAndLaidOut(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	w := doJSON(r, http.MethodGet, "/api/workflows/templates", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Templates []struct {
			PipelineID string               `json:"pipeline_id"`
			Name       string               `json:"name"`
			Summary    string               `json:"summary"`
			Nodes      []model.NodeInstance `json:"nodes"`
			Edges      []model.EdgeInstance `json:"edges"`
			Report     serviceReport        `json:"report"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Templates) == 0 {
		t.Fatal("一条模板都没有")
	}
	for _, tp := range resp.Templates {
		if len(tp.Nodes) == 0 {
			t.Errorf("%s 组装出 0 个节点（summary=%q）", tp.PipelineID, tp.Summary)
			continue
		}
		if !tp.Report.OK {
			t.Errorf("%s 的图有阻断问题 %v：%s", tp.PipelineID, tp.Report.BlockingCodes, firstMsg(tp.Report))
		}
		for _, n := range tp.Nodes {
			if n.Position == nil || (n.Position["x"] == 0 && n.Position["y"] == 0) {
				t.Errorf("%s 的节点 %s 没有坐标（打开画布会堆在原点）", tp.PipelineID, n.ID)
			}
		}
	}
}

// serviceReport 只取断言要用的字段，避免测试与 service 包结构体绑死。
type serviceReport struct {
	OK            bool     `json:"ok"`
	Blocking      int      `json:"blocking"`
	BlockingCodes []string `json:"blocking_codes"`
	DAG           []struct {
		Message string `json:"message"`
	} `json:"dag"`
	Ports []struct {
		Message string `json:"message"`
	} `json:"ports"`
}

func firstMsg(r serviceReport) string {
	for _, i := range r.Ports {
		if i.Message != "" {
			return i.Message
		}
	}
	for _, i := range r.DAG {
		if i.Message != "" {
			return i.Message
		}
	}
	return ""
}

const goodGraph = `{"name":"最小图","nodes":[
 {"id":"s","type":"start","data":{"prompt":"雨夜旧客栈"}},
 {"id":"t2i","type":"text2image","data":{"width":1024,"height":1024}},
 {"id":"e","type":"end","data":{}}],
 "edges":[{"id":"e1","source":"s","target":"t2i"},{"id":"e2","source":"t2i","target":"e"}]}`

func TestCreateWorkflowRoundTrip(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)

	w := doJSON(r, http.MethodPost, "/api/workflows", goodGraph)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		ID     uint   `json:"id"`
		Origin string `json:"origin"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == 0 || created.Origin != "user" {
		t.Fatalf("创建结果不对：%+v", created)
	}

	// 读回来：必须有坐标（保存时自动分层布局），且报告为绿
	w = doJSON(r, http.MethodGet, "/api/workflows/1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("读取应 200，实际 %d", w.Code)
	}
	var got struct {
		Nodes  []model.NodeInstance `json:"nodes"`
		Edges  []model.EdgeInstance `json:"edges"`
		Report serviceReport        `json:"report"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 3 || len(got.Edges) != 2 {
		t.Fatalf("读回的图不对：%+v", got)
	}
	if !got.Report.OK {
		t.Errorf("保存后的图必须体检为绿，实际 %v", got.Report.BlockingCodes)
	}
	for _, n := range got.Nodes {
		if n.Position["x"] == 0 && n.Position["y"] == 0 {
			t.Errorf("节点 %s 没有坐标", n.ID)
		}
	}

	// 改名保存
	w = doJSON(r, http.MethodPut, "/api/workflows/1", `{"name":"改过名","nodes":[
	 {"id":"s","type":"start","data":{"prompt":"x"}},
	 {"id":"t2i","type":"text2image","data":{}},
	 {"id":"e","type":"end","data":{}}],
	 "edges":[{"id":"e1","source":"s","target":"t2i"},{"id":"e2","source":"t2i","target":"e"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", w.Code, w.Body.String())
	}

	// 删除后读不到
	if w = doJSON(r, http.MethodDelete, "/api/workflows/1", ""); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if w = doJSON(r, http.MethodGet, "/api/workflows/1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404，实际 %d", w.Code)
	}
}

// 必填输入没有来源 = 一定跑不起来的图，保存期就要拦（400 + 可定位的报告）。
func TestCreateWorkflowRejectsUnreachableInput(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	body := `{"name":"坏图","nodes":[
	 {"id":"s","type":"start","data":{"prompt":"x"}},
	 {"id":"up","type":"upscale","data":{}}],
	 "edges":[{"id":"e1","source":"s","target":"up"}]}`
	w := doJSON(r, http.MethodPost, "/api/workflows", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "input_unreachable") {
		t.Errorf("响应应含稳定错误码 input_unreachable，实际 %s", w.Body.String())
	}
}

func TestCreateWorkflowRejectsReservedPrefix(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	body := strings.Replace(goodGraph, `"最小图"`, `"qc:image"`, 1)
	w := doJSON(r, http.MethodPost, "/api/workflows", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "qc:") {
		t.Fatalf("qc: 前缀应被拒绝，实际 %d %s", w.Code, w.Body.String())
	}
}

func TestCreateWorkflowRejectsDuplicateNodeID(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	body := `{"name":"重 id","nodes":[
	 {"id":"a","type":"start","data":{"prompt":"x"}},
	 {"id":"a","type":"text2image","data":{}}],"edges":[]}`
	w := doJSON(r, http.MethodPost, "/api/workflows", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "重复") {
		t.Fatalf("节点 id 重复应 400 并说明，实际 %d %s", w.Code, w.Body.String())
	}
}

// 质量循环临时图不属于画布：不许出现在列表里，也不许被编辑/删除。
func TestTransientWorkflowsAreInvisibleAndReadOnly(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Workflow{Name: "qc:image", Nodes: []byte("[]"), Edges: []byte("[]")}).Error; err != nil {
		t.Fatal(err)
	}
	g := NewGraphHandler(db, newTestPipelines(t))
	r := newGraphRouter(g)

	w := doJSON(r, http.MethodGet, "/api/workflows", "")
	if strings.Contains(w.Body.String(), "qc:image") {
		t.Fatalf("列表不该出现临时图，实际 %s", w.Body.String())
	}
	if w = doJSON(r, http.MethodGet, "/api/workflows/1", ""); w.Code != http.StatusOK {
		t.Fatalf("按 id 应能读到（引擎要能用），实际 %d", w.Code)
	}
	if w = doJSON(r, http.MethodPut, "/api/workflows/1", goodGraph); w.Code != http.StatusForbidden {
		t.Fatalf("临时图不该可编辑，实际 %d", w.Code)
	}
	if w = doJSON(r, http.MethodDelete, "/api/workflows/1", ""); w.Code != http.StatusForbidden {
		t.Fatalf("临时图不该可删除，实际 %d", w.Code)
	}
}

// 干跑：不提交、不改状态，返回四段信息；没注入探测器时必须说 skipped 而不是"都齐了"。
func TestDryRunReportsRangeAndProbeState(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	body := `{"nodes":[
	 {"id":"s","type":"start","data":{"prompt":"白鹿立于雪峰"}},
	 {"id":"t2v","type":"text2video","data":{"width":608,"height":352,"frames":124,"segments":2}},
	 {"id":"e","type":"end","data":{}}],
	 "edges":[{"id":"e1","source":"s","target":"t2v"},{"id":"e2","source":"t2v","target":"e"}]}`
	w := doJSON(r, http.MethodPost, "/api/graph/dry-run", body)
	if w.Code != http.StatusOK {
		t.Fatalf("干跑应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK         bool   `json:"ok"`
		RunReady   bool   `json:"run_ready"`
		ProbeState string `json:"probe_state"`
		Estimate   struct {
			P50   float64 `json:"p50_sec"`
			P90   float64 `json:"p90_sec"`
			Worst float64 `json:"worst_sec"`
		} `json:"estimate"`
		Report serviceReport `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ProbeState != "skipped" {
		t.Errorf("没注入探测器时必须报 skipped（不可达 ≠ 空结果），实际 %q", resp.ProbeState)
	}
	if !resp.Report.OK {
		t.Errorf("这张图结构应通过，实际 %v", resp.Report.BlockingCodes)
	}
	if !(0 < resp.Estimate.P50 && resp.Estimate.P50 < resp.Estimate.P90 && resp.Estimate.P90 < resp.Estimate.Worst) {
		t.Errorf("估算四档不自洽：%+v", resp.Estimate)
	}
	// 顶层不许出现单一总时长字段（AGENTS §0.3）：
	// 单行明细里的 seconds 是合法的，被禁止的是"一句话告诉你这条要几分钟"。
	var raw struct {
		Estimate map[string]interface{} `json:"estimate"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, banned := range []string{"seconds", "total_seconds", "eta_sec"} {
		if _, found := raw.Estimate[banned]; found {
			t.Errorf("估算顶层不许出现 %q（必须给区间）", banned)
		}
	}
}

// 体检端点是"查询"，用 200+ok 返回报告；用 4xx 前端会丢掉详情。
func TestValidateReturnsReportWith200(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	body := `{"nodes":[{"id":"if","type":"if_else","data":{}}],"edges":[]}`
	w := doJSON(r, http.MethodPost, "/api/graph/validate", body)
	if w.Code != http.StatusOK {
		t.Fatalf("体检应 200，实际 %d", w.Code)
	}
	var rep serviceReport
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if rep.OK {
		t.Fatal("含不存在节点的图不该判绿")
	}
	if len(rep.BlockingCodes) == 0 || rep.BlockingCodes[0] != "unknown_node_type" {
		t.Fatalf("应报 unknown_node_type，实际 %v", rep.BlockingCodes)
	}
}

// 目录端点：节点数与 NodeSpecs 一致，每种都带端口与成本（前端不许再硬编码类型）。
func TestCatalogEndpoint(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	r := newGraphRouter(g)
	w := doJSON(r, http.MethodGet, "/api/node-catalog", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp struct {
		Count int `json:"count"`
		Nodes []struct {
			Type   string `json:"type"`
			Fields []struct {
				Key string `json:"key"`
			} `json:"fields"`
			Cost struct {
				Unit string `json:"unit"`
			} `json:"cost"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != len(service.NodeSpecs) {
		t.Errorf("节点应为 %d 种（与 NodeSpecs 一致），实际 %d", len(service.NodeSpecs), resp.Count)
	}
	for _, n := range resp.Nodes {
		if n.Cost.Unit == "" {
			t.Errorf("%s 缺成本单位", n.Type)
		}
	}
}

// ---- 画布图的运行入口：守门顺序必须早于任何执行 ----

func TestRunWorkflowRejectsUnknownID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := NewProducer(newTestDB(t), nil, nil, newTestPipelines(t))
	r := gin.New()
	r.POST("/api/workflows/:id/run", p.RunWorkflow)
	w := doJSON(r, http.MethodPost, "/api/workflows/42/run", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的图应 404，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// 保存时是好的、之后被手改坏（或老版本遗留）的图，运行前必须再拦一次。
// 这条同时钉住「运行 = 最后一次守门」：NormalizeEngine 为 nil 也不该走到执行。
func TestRunWorkflowRejectsBrokenGraph(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.Create(&model.Workflow{
		Name:  "被改坏的图",
		Nodes: []byte(`[{"id":"a","type":"if_else","data":{}}]`),
		Edges: []byte(`[]`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	p := NewProducer(db, nil, nil, newTestPipelines(t))
	r := gin.New()
	r.POST("/api/workflows/:id/run", p.RunWorkflow)
	w := doJSON(r, http.MethodPost, "/api/workflows/1/run", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("含不存在节点的图应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown_node_type") {
		t.Errorf("响应要点名阻断原因，实际 %s", w.Body.String())
	}
}

// 重跑画布 run 不能掉进「没有这条流水线：graph:N」的分支。
func TestGraphPipelineIDRoundTrip(t *testing.T) {
	if !IsGraphRun("graph:12") {
		t.Fatal("graph:12 应识别为画布图")
	}
	id, ok := GraphWorkflowID("graph:12")
	if !ok || id != 12 {
		t.Fatalf("应解析出 12，实际 %d %v", id, ok)
	}
	for _, bad := range []string{"image", "graph:", "graph:abc", ""} {
		if IsGraphRun(bad) {
			if _, ok := GraphWorkflowID(bad); ok {
				t.Errorf("%q 不该解析出图 id", bad)
			}
		}
	}
}
// 质量循环包壳：外层必须只有 3 个节点，且 quality_loop 指向刚落库的内层图。
// 提示词要沿用到外层 start，否则每一轮都会重跑一张没有主题的图。
func TestWrapGraphQCBuildsShell(t *testing.T) {
	db := newTestDB(t)
	p := NewProducer(db, nil, nil, newTestPipelines(t))

	wf := model.Workflow{Name: "雨夜客栈", Nodes: []byte(`[{"id":"s","type":"start","data":{"prompt":"雨夜客栈"}}]`), Edges: []byte(`[]`)}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatal(err)
	}
	inner := []model.NodeInstance{{ID: "s", Type: "start", Data: map[string]interface{}{"prompt": "雨夜客栈"}}}

	nodesJSON, edgesJSON, transientID, err := p.wrapGraphQC(wf, inner, 4)
	if err != nil || transientID == 0 {
		t.Fatalf("包壳失败：%v（transient=%d）", err, transientID)
	}

	var outer []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &outer); err != nil {
		t.Fatal(err)
	}
	if len(outer) != 3 {
		t.Fatalf("外层应只有 3 个节点，实际 %d", len(outer))
	}
	if outer[1].Type != "quality_loop" {
		t.Fatalf("中间节点应为 quality_loop，实际 %s", outer[1].Type)
	}
	if got := outer[1].Data["workflow_id"]; got != float64(transientID) && got != transientID {
		t.Errorf("quality_loop.workflow_id 应指向内层图 %d，实际 %v", transientID, got)
	}
	if got := outer[0].Data["prompt"]; got != "雨夜客栈" {
		t.Errorf("外层 start 应沿用内层提示词，实际 %v", got)
	}

	var edgeList []model.EdgeInstance
	_ = json.Unmarshal(edgesJSON, &edgeList)
	if len(edgeList) != 2 {
		t.Errorf("外层应有 2 条连线，实际 %d", len(edgeList))
	}

	// 临时图必须带 qc: 前缀 —— SweepTransient 靠它清残留，否则进程崩溃会攒垃圾
	var saved model.Workflow
	if err := db.First(&saved, transientID).Error; err != nil {
		t.Fatalf("内层图应已落库：%v", err)
	}
	if !strings.HasPrefix(saved.Name, "qc:") {
		t.Errorf("临时图名应以 qc: 开头（否则清扫不到），实际 %q", saved.Name)
	}
}
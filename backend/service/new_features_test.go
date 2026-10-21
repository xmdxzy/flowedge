package service

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// ===== 变量聚合器 =====

func TestAggregateNodeModes(t *testing.T) {
	e := &EngineV2{}
	n := &model.NodeInstance{ID: "agg", Type: "aggregate", Data: map[string]interface{}{}}
	upstream := map[string]interface{}{
		"a": map[string]interface{}{"text": "第一段"},
		"b": map[string]interface{}{"text": "第二段"},
	}

	out, err := e.execAggregateNode(n, upstream)
	if err != nil {
		t.Fatalf("aggregate 失败：%v", err)
	}
	m := out.(map[string]interface{})
	if m["count"].(int) != 2 {
		t.Fatalf("count 应为 2，实际 %v", m["count"])
	}
	text := m["text"].(string)
	// 稳定排序：a 在 b 之前
	if !strings.HasPrefix(text, "第一段") || !strings.HasSuffix(text, "第二段") {
		t.Fatalf("文本拼接顺序不对：%q", text)
	}
	if _, ok := m["nodes"].(map[string]interface{})["a"]; !ok {
		t.Fatalf("object 模式应输出 nodes 映射")
	}
}

func TestAggregateNodeExplicitInputsAndField(t *testing.T) {
	e := &EngineV2{}
	n := &model.NodeInstance{ID: "agg", Type: "aggregate", Data: map[string]interface{}{
		"mode":          "list",
		"inputs":        "b, a",
		"field":         "image",
		"separator":     "|",
		"include_empty": true,
	}}
	upstream := map[string]interface{}{
		"a": map[string]interface{}{"text": "x", "image": "/tmp/a.png"},
		"b": map[string]interface{}{"text": "y", "image": "/tmp/b.png"},
	}
	out, err := e.execAggregateNode(n, upstream)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]interface{})
	if m["text"].(string) != "/tmp/b.png|/tmp/a.png" {
		t.Fatalf("显式顺序未生效：%q", m["text"])
	}
	vals := m["values"].([]interface{})
	if len(vals) != 2 || vals[0] != "/tmp/b.png" {
		t.Fatalf("values 不对：%v", vals)
	}
}

func TestAggregateNodeNoInput(t *testing.T) {
	e := &EngineV2{}
	n := &model.NodeInstance{ID: "agg", Type: "aggregate", Data: map[string]interface{}{}}
	if _, err := e.execAggregateNode(n, map[string]interface{}{}); err == nil {
		t.Fatal("无上游输入时应明确报错")
	}
}

// ===== 文档解析 =====

func makeTestDocx(paras ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paras {
		sb.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)
	w.Write([]byte(sb.String()))
	zw.Close()
	return buf.Bytes()
}

func TestExtractDOCX(t *testing.T) {
	data := makeTestDocx("第一段：道门圣境", "第二段：FlowEdge 引擎")
	txt, err := ExtractDocumentText("demo.docx", data)
	if err != nil {
		t.Fatalf("docx 解析失败：%v", err)
	}
	if !strings.Contains(txt, "道门圣境") || !strings.Contains(txt, "FlowEdge") {
		t.Fatalf("docx 文本未正确抽取：%q", txt)
	}
}

func TestExtractPlainText(t *testing.T) {
	txt, err := ExtractDocumentText("a.md", []byte("\uFEFF# 标题\r\n\r\n正文  "))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(txt, "\uFEFF") || strings.Contains(txt, "\r") {
		t.Fatalf("BOM/换行未归一化：%q", txt)
	}
}

func TestExtractPDFTextStream(t *testing.T) {
	// 最小可解析 PDF：未压缩内容流里的 Tj 文本
	pdf := []byte("%PDF-1.4\n1 0 obj<< /Length 44 >>stream\nBT /F1 12 Tf (Hello FlowEdge PDF) Tj ET\nendstream\nendobj\ntrailer<<>>\n%%EOF\n")
	txt, err := ExtractDocumentText("demo.pdf", pdf)
	if err != nil {
		t.Fatalf("pdf 解析失败：%v", err)
	}
	if !strings.Contains(txt, "Hello FlowEdge") {
		t.Fatalf("pdf 文本未抽取：%q", txt)
	}
}

func TestExtractUnsupportedDoc(t *testing.T) {
	if _, err := ExtractDocumentText("a.doc", []byte("x")); err == nil {
		t.Fatal("旧版 .doc 应显式报错")
	}
	if IsSupportedDoc("a.exe") {
		t.Fatal("exe 不应被视为可导入文档")
	}
}

// ===== 凭据加密 =====

func TestCredentialStoreEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCredentialStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-super-secret-value-123"
	if err := store.Set("openai", CredentialSecret{
		Headers: map[string]string{"Authorization": "Bearer " + secret},
		Note:    "测试",
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("凭据文件中出现了明文密钥")
	}

	// 重新打开（模拟重启）后仍能解密
	store2, err := NewCredentialStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := store2.Resolve("openai")
	if err != nil {
		t.Fatal(err)
	}
	if h["Authorization"] != "Bearer "+secret {
		t.Fatalf("解密结果不对：%v", h)
	}

	// 列表只暴露 header 名
	list := store2.List()
	if len(list) != 1 || len(list[0].HeaderKeys) != 1 || list[0].HeaderKeys[0] != "Authorization" {
		t.Fatalf("脱敏列表不对：%+v", list)
	}

	if err := store2.Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store2.Get("openai"); ok {
		t.Fatal("删除后仍能取到")
	}
}

func TestCredentialResolveMissing(t *testing.T) {
	store, err := NewCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve("nope"); err == nil {
		t.Fatal("引用不存在的凭据应报错")
	}
}

func TestAppendQuery(t *testing.T) {
	got := appendQuery("http://x/api?b=2", map[string]string{"a": "1 2"})
	if got != "http://x/api?b=2&a=1+2" {
		t.Fatalf("query 拼接不对：%s", got)
	}
}

// ===== 单步 / 断点 =====

func stepEngine(t *testing.T) *EngineV2 {
	t.Helper()
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	e.RegisterNodeType("tick", func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		return map[string]interface{}{"text": node.ID}, nil
	})
	return e
}

func TestStepControlExecutesOneNodePerCall(t *testing.T) {
	e := stepEngine(t)
	nodes := []byte(`[
		{"id":"a","type":"start","data":{"prompt":"go"}},
		{"id":"b","type":"tick","data":{}},
		{"id":"c","type":"tick","data":{}}
	]`)
	edges := []byte(`[
		{"id":"e1","source":"a","target":"b"},
		{"id":"e2","source":"b","target":"c"}
	]`)

	rc := NewStepControl(1)
	states, err := e.ExecuteWithControl(nodes, edges, 0, rc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if paused, at, reason := rc.State(); !paused || reason != "step" {
		t.Fatalf("第一步应因单步而暂停，实际 paused=%v at=%s reason=%s", paused, at, reason)
	}
	if countStatus(states, "success") != 1 {
		t.Fatalf("第一步应只完成 1 个节点，实际 %d", countStatus(states, "success"))
	}

	// 第二步：从暂停状态继续推进一个节点
	rc2 := NewStepControl(1)
	states, err = e.ResumeWithControl(nodes, edges, states, 0, rc2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if countStatus(states, "success") != 2 {
		t.Fatalf("第二步应完成 2 个节点，实际 %d", countStatus(states, "success"))
	}

	// 第三步：跑完
	rc3 := NewStepControl(1)
	states, err = e.ResumeWithControl(nodes, edges, states, 0, rc3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if paused, _, _ := rc3.State(); paused {
		t.Fatal("最后一个节点执行后不应再暂停")
	}
	if countStatus(states, "success") != 3 {
		t.Fatalf("应全部完成，实际 %d", countStatus(states, "success"))
	}
}

func TestBreakpointStopsBeforeNode(t *testing.T) {
	e := stepEngine(t)
	nodes := []byte(`[
		{"id":"a","type":"start","data":{"prompt":"go"}},
		{"id":"b","type":"tick","data":{}},
		{"id":"c","type":"tick","data":{}}
	]`)
	edges := []byte(`[
		{"id":"e1","source":"a","target":"b"},
		{"id":"e2","source":"b","target":"c"}
	]`)

	rc := NewBreakpointControl(map[string]bool{"b": true})
	states, err := e.ExecuteWithControl(nodes, edges, 0, rc, nil)
	if err != nil {
		t.Fatal(err)
	}
	paused, at, reason := rc.State()
	if !paused || at != "b" || reason != "breakpoint" {
		t.Fatalf("应停在 b 之前，实际 paused=%v at=%s reason=%s", paused, at, reason)
	}
	for _, s := range states {
		if s.ID == "b" && s.Status != "pending" {
			t.Fatalf("b 应保持 pending，实际 %s", s.Status)
		}
		if s.ID == "a" && s.Status != "success" {
			t.Fatalf("a 应已成功，实际 %s", s.Status)
		}
	}
}

func TestRunControlTakeAndReset(t *testing.T) {
	rc := NewStepControl(2)
	if !rc.Take() || !rc.Take() {
		t.Fatal("前两次应拿到许可")
	}
	if rc.Take() {
		t.Fatal("预算耗尽后不应再拿到许可")
	}
	rc.MarkPaused("x", "step")
	if paused, at, _ := rc.State(); !paused || at != "x" {
		t.Fatal("暂停状态记录不对")
	}
	rc.Reset()
	if paused, _, _ := rc.State(); paused {
		t.Fatal("Reset 后不应仍处于暂停")
	}
}

func countStatus(states []model.NodeInstance, status string) int {
	n := 0
	for _, s := range states {
		if s.Status == status {
			n++
		}
	}
	return n
}

// ===== 产物血缘 =====

func TestLineageSkipsPassThroughNodes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("内存库打不开：%v", err)
	}
	if err := db.AutoMigrate(&ArtifactLineage{}); err != nil {
		t.Fatal(err)
	}
	store := NewLineageStore(db)

	nodes := []model.NodeInstance{
		{ID: "t1", Type: "text2image", Status: "success", Output: map[string]interface{}{"image": "C:/x/a.png"}},
		{ID: "t2", Type: "text2image", Status: "success", Output: map[string]interface{}{"image": "C:/x/b.png"}},
		{ID: "tts", Type: "tts", Status: "success", Output: map[string]interface{}{"audio": "C:/x/c.wav"}},
		{ID: "v", Type: "video_compose", Status: "success", Output: map[string]interface{}{"video": "C:/x/d.mp4"}},
		// end 只是透传上游输出，不该被记成产物来源
		{ID: "e", Type: "end", Status: "success", Output: map[string]interface{}{"video": "C:/x/d.mp4"}},
	}
	edges := []model.EdgeInstance{
		{ID: "1", Source: "t1", Target: "v"},
		{ID: "2", Source: "t2", Target: "v"},
		{ID: "3", Source: "tts", Target: "v"},
		{ID: "4", Source: "v", Target: "e"},
	}

	if got := store.RecordRun(7, 9, nodes, edges); got != 4 {
		t.Fatalf("应记录 4 件产物（两图一音一视频），实际 %d", got)
	}

	self, ancestors, ok := store.Get("d.mp4")
	if !ok {
		t.Fatal("视频没有血缘记录")
	}
	if self.NodeID != "v" {
		t.Fatalf("视频应归属 video_compose 节点，实际 %s(%s)", self.NodeID, self.NodeType)
	}
	if len(ancestors) != 3 {
		t.Fatalf("视频应有 3 个上游产物，实际 %d：%+v", len(ancestors), ancestors)
	}
	names := map[string]bool{}
	for _, a := range ancestors {
		names[a.Name] = true
	}
	for _, want := range []string{"a.png", "b.png", "c.wav"} {
		if !names[want] {
			t.Fatalf("上游缺少 %s：%v", want, names)
		}
	}
}

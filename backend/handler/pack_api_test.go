package handler

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 导演包的 HTTP 往返：建图 → 导出 zip → 导入 → 得到一张新图，素材路径指向解包目录。
//
// 这条用例守的是"API 层面真的能用"：服务层单测过了但路由没挂上/字段名对不上，
// 用户那边就是 404 或 400。
func TestPackExportImportHTTP(t *testing.T) {
	db := newTestDB(t)
	g := NewGraphHandler(db, newTestPipelines(t))
	dataDir := t.TempDir()
	g.SetDataDir(dataDir)

	// 造一个素材文件，塞进图的 data 里
	assetDir := t.TempDir()
	ref := filepath.Join(assetDir, "ref.png")
	if err := os.WriteFile(ref, []byte("IMGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	nodes, _ := json.Marshal([]map[string]interface{}{{
		"id": "sl-1", "type": "shotlist",
		"data": map[string]interface{}{"prompt": "p", "ref_images": []string{ref}},
	}})
	wf := model.Workflow{Name: "交接片", Nodes: nodes, Edges: json.RawMessage(`[]`), Origin: "user", SchemaVersion: 1}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatalf("建图失败：%v", err)
	}

	r := newGraphRouter(g)
	// 路由里补上两个新端点（newGraphRouter 是旧用例的固定集合，这里单独挂）
	r.POST("/api/workflows/:id/pack", g.ExportPack)
	r.POST("/api/packs/import", g.ImportPack)

	w := doJSON(r, http.MethodPost, "/api/workflows/1/pack", "")
	if w.Code != http.StatusOK {
		t.Fatalf("导出应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var exp struct {
		Path   string `json:"path"`
		Assets int    `json:"assets"`
		Bytes  int64  `json:"bytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &exp); err != nil {
		t.Fatalf("导出响应不是 JSON：%v", err)
	}
	if exp.Assets != 1 {
		t.Fatalf("应打包 1 个素材，实际 %d", exp.Assets)
	}
	if _, err := os.Stat(exp.Path); err != nil {
		t.Fatalf("导出文件不存在：%v", err)
	}
	if !strings.Contains(exp.Path, filepath.Join("packs")) {
		t.Fatalf("包应落在 <dataDir>/packs 下，实际 %s", exp.Path)
	}

	body, _ := json.Marshal(map[string]string{"path": exp.Path, "name": "在另一台机器上"})
	w2 := doJSON(r, http.MethodPost, "/api/packs/import", string(body))
	if w2.Code != http.StatusCreated {
		t.Fatalf("导入应 201，实际 %d：%s", w2.Code, w2.Body.String())
	}
	var imp struct {
		ID        uint   `json:"id"`
		Name      string `json:"name"`
		AssetsDir string `json:"assets_dir"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &imp); err != nil {
		t.Fatalf("导入响应不是 JSON：%v", err)
	}
	if imp.ID == 0 || imp.ID == wf.ID {
		t.Fatalf("导入应建一张新图，实际 id=%d（原图 %d）", imp.ID, wf.ID)
	}

	// 新图里的素材路径必须指向解包目录，且文件真在
	var got model.Workflow
	if err := db.First(&got, imp.ID).Error; err != nil {
		t.Fatalf("新图没落库：%v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(got.Nodes, &arr); err != nil {
		t.Fatalf("新图节点解析失败：%v", err)
	}
	data := arr[0]["data"].(map[string]interface{})
	gotRef := data["ref_images"].([]interface{})[0].(string)
	if !strings.HasPrefix(gotRef, imp.AssetsDir) {
		t.Fatalf("导入后素材应指向 %s，实际 %s", imp.AssetsDir, gotRef)
	}
	if b, err := os.ReadFile(gotRef); err != nil || string(b) != "IMGDATA" {
		t.Fatalf("素材内容没还原：%v %q", err, string(b))
	}
}

// 不给 dataDir 时必须显式报错，而不是把包丢到当前目录。
func TestPackExportWithoutDataDir(t *testing.T) {
	db := newTestDB(t)
	g := NewGraphHandler(db, newTestPipelines(t))
	nodes, _ := json.Marshal([]map[string]interface{}{{"id": "n", "type": "start", "data": map[string]interface{}{}}})
	wf := model.Workflow{Name: "x", Nodes: nodes, Edges: json.RawMessage(`[]`), SchemaVersion: 1}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatal(err)
	}
	r := newGraphRouter(g)
	r.POST("/api/workflows/:id/pack", g.ExportPack)
	w := doJSON(r, http.MethodPost, "/api/workflows/1/pack", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("未配置数据目录应 500，实际 %d：%s", w.Code, w.Body.String())
	}
}

// 导入一个不存在的路径必须 400 且说清楚，不能 500。
func TestPackImportMissingFile(t *testing.T) {
	g := NewGraphHandler(newTestDB(t), newTestPipelines(t))
	g.SetDataDir(t.TempDir())
	r := newGraphRouter(g)
	r.POST("/api/packs/import", g.ImportPack)
	w := doJSON(r, http.MethodPost, "/api/packs/import", `{"path":"C:/nope/x.fepack.zip"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("找不到包应 400，实际 %d：%s", w.Code, w.Body.String())
	}
}

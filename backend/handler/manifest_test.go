package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 血缘清单 API 的三条语义必须钉住：
//  1. 有清单 → 200 + 清单内容（能被前端/脚本直接消费）
//  2. 旧运行（ManifestPath 空）→ 404 且说明原因，不能返回空对象冒充成功
//  3. run 不存在 → 404
func TestGetManifest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	p := &Producer{db: db}

	dir := t.TempDir()
	art := filepath.Join(dir, "film.mp4")
	if err := os.WriteFile(art, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &service.Manifest{
		Version:    1,
		RunID:      101,
		PipelineID: "film",
		Engine:     "test",
		Nodes: []service.NodeManifest{{
			NodeID:    "sl-1",
			NodeType:  "shotlist",
			Status:    "success",
			Key:       "abc123",
			Artifacts: []service.ArtifactRef{{Path: art, Kind: "video", Bytes: 4096}},
		}},
	}
	path, err := service.WriteManifest(filepath.Join(dir, "manifests"), m)
	if err != nil {
		t.Fatal(err)
	}

	withManifest := model.WorkflowRun{PipelineID: "film", Status: "success", ManifestPath: path}
	if err := db.Create(&withManifest).Error; err != nil {
		t.Fatal(err)
	}
	legacy := model.WorkflowRun{PipelineID: "image", Status: "success"}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.GET("/api/runs/:runId/manifest", p.GetManifest)
	do := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id+"/manifest", nil)
		r.ServeHTTP(w, req)
		return w
	}

	// 1) 有清单
	w := do(itoaU(withManifest.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("有清单应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var got service.Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法清单：%v", err)
	}
	if got.RunID != 101 || len(got.Nodes) != 1 || got.Nodes[0].Key != "abc123" {
		t.Fatalf("清单内容不一致：%+v", got)
	}

	// 2) 旧运行：必须 404 且说明原因（不能返回空清单）
	w = do(itoaU(legacy.ID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("无清单应 404，实际 %d", w.Code)
	}
	if !containsStr(w.Body.String(), "没有血缘清单") {
		t.Errorf("404 响应应说明原因，实际：%s", w.Body.String())
	}

	// 3) 不存在的 run
	if w = do("999999"); w.Code != http.StatusNotFound {
		t.Fatalf("不存在的 run 应 404，实际 %d", w.Code)
	}

	// 4) 非法 id
	if w = do("abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 应 400，实际 %d", w.Code)
	}
}

func itoaU(v uint) string { return strconv.FormatUint(uint64(v), 10) }

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

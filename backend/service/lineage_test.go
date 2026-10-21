package service

import (
	"testing"

	"flowedge/backend/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 血缘白名单扩展的回归锁（docs/系统评估-2026-10-21.md §3 S5）。
//
// 历史缺口：白名单只有 text2image/tts/video_compose，视频管线的主产物
// （shotlist / text2video / upscale / image2image / inpaint）全部没有血缘，
// 而这几类恰好是最需要溯源的那批。
func TestLineageCoversVideoPipelineProducers(t *testing.T) {
	for _, typ := range []string{"shotlist", "text2video", "image2image", "inpaint", "upscale", "comfy_workflow"} {
		if !mediaProducerTypes[typ] {
			t.Fatalf("%s 是真实产出媒体文件的节点类型，必须进白名单", typ)
		}
	}
	// select 只挑候选、不造文件，**不能**进白名单：否则同一件产物会被后写记录覆盖，
	// "谁做的"会指向 select 而不是真正生成的节点。
	if mediaProducerTypes["select"] {
		t.Fatal("select 是选择器不是生产者，不该进白名单")
	}
}

func TestLineageRecordsShotlistArtifact(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("内存库打不开：%v", err)
	}
	if err := db.AutoMigrate(&ArtifactLineage{}); err != nil {
		t.Fatal(err)
	}
	store := NewLineageStore(db)

	nodes := []model.NodeInstance{
		{ID: "sb", Type: "storyboard", Status: "success", Output: map[string]interface{}{"text": "{}"}},
		{ID: "sl", Type: "shotlist", Status: "success", Output: map[string]interface{}{
			"video": "C:/x/final.mp4", "kind": "video", "shots": 3,
		}},
		{ID: "e", Type: "end", Status: "success", Output: map[string]interface{}{"video": "C:/x/final.mp4"}},
	}
	edges := []model.EdgeInstance{
		{ID: "1", Source: "sb", Target: "sl"},
		{ID: "2", Source: "sl", Target: "e"},
	}

	if got := store.RecordRun(1, 2, nodes, edges); got != 1 {
		t.Fatalf("应只记录 1 件产物（分镜成片，end 透传不算），实际 %d", got)
	}
	self, ancestors, ok := store.Get("final.mp4")
	if !ok {
		t.Fatal("分镜成片没有血缘记录")
	}
	if self.NodeType != "shotlist" {
		t.Fatalf("成片应归属 shotlist，实际 %s", self.NodeType)
	}
	if len(ancestors) != 0 {
		t.Fatalf("分镜由文本驱动，不该有媒体上游，实际 %d", len(ancestors))
	}
}

func TestLineagePassthroughGuard(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("内存库打不开：%v", err)
	}
	if err := db.AutoMigrate(&ArtifactLineage{}); err != nil {
		t.Fatal(err)
	}
	store := NewLineageStore(db)

	// comfy_workflow 在白名单里，但这次它把上游产出的同一张图原样返回（没造新文件）。
	// 结构判断应该识别出这是搬运，保留 text2image 的记录不被覆盖。
	nodes := []model.NodeInstance{
		{ID: "t2i", Type: "text2image", Status: "success", Output: map[string]interface{}{"image": "C:/x/a.png"}},
		{ID: "cw", Type: "comfy_workflow", Status: "success", Output: map[string]interface{}{"image": "C:/x/a.png"}},
	}
	edges := []model.EdgeInstance{{ID: "1", Source: "t2i", Target: "cw"}}

	store.RecordRun(3, 4, nodes, edges)

	self, _, ok := store.Get("a.png")
	if !ok {
		t.Fatal("产物应有血缘记录")
	}
	if self.NodeType != "text2image" {
		t.Fatalf("透传节点不该覆盖生产者记录，实际归属 %s", self.NodeType)
	}
}

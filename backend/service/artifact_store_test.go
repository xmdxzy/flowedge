package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ArtifactKey 必须对「任何一个输入变化」都改变 hash —— 这是血缘可用性的底线。
// 少了任何一项，就会出现「换了权重/参考图/seed，缓存照样命中」的假复现。
func TestArtifactKey_Completeness(t *testing.T) {
	base := ArtifactKey{
		NodeType: "text2video",
		Pipeline: "film",
		Params:   map[string]any{"prompt": "a cat", "steps": float64(12)},
		Models:   map[string]string{"video.h3.ref2va.unet": "abc123"},
		Seed:     42,
		Inputs:   []string{"hash-of-ref-image"},
		Code:     "lite|v1",
	}
	want := base.Hash()
	if len(want) != 32 {
		t.Fatalf("hash 长度应为 32，实际 %d (%q)", len(want), want)
	}

	// 同一输入重复计算必须稳定（map 序列化顺序不能影响结果）
	for i := 0; i < 5; i++ {
		if got := base.Hash(); got != want {
			t.Fatalf("第 %d 次 hash 不稳定：%s != %s", i, got, want)
		}
	}

	cases := []struct {
		name   string
		mutate func(k *ArtifactKey)
	}{
		{"参数变", func(k *ArtifactKey) { k.Params["prompt"] = "a dog" }},
		{"步数变", func(k *ArtifactKey) { k.Params["steps"] = float64(20) }},
		{"权重变", func(k *ArtifactKey) { k.Models["video.h3.ref2va.unet"] = "def456" }},
		{"seed 变", func(k *ArtifactKey) { k.Seed = 43 }},
		{"参考图变", func(k *ArtifactKey) { k.Inputs = []string{"hash-of-other-image"} }},
		{"代码版本变", func(k *ArtifactKey) { k.Code = "lite|v2" }},
		{"节点类型变", func(k *ArtifactKey) { k.NodeType = "text2image" }},
		{"流水线变", func(k *ArtifactKey) { k.Pipeline = "video" }},
	}
	for _, c := range cases {
		k := ArtifactKey{
			NodeType: base.NodeType,
			Pipeline: base.Pipeline,
			Params:   map[string]any{"prompt": "a cat", "steps": float64(12)},
			Models:   map[string]string{"video.h3.ref2va.unet": "abc123"},
			Seed:     base.Seed,
			Inputs:   append([]string{}, base.Inputs...),
			Code:     base.Code,
		}
		c.mutate(&k)
		if got := k.Hash(); got == want {
			t.Errorf("%s 后 hash 未变化（应为不同 key）", c.name)
		}
	}
}

// 指纹必须随「文件内容/大小」变化，且同文件重复读取走缓存不重算。
func TestFingerprintCache_DetectsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.safetensors")
	if err := os.WriteFile(path, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := LoadFingerprintCache(filepath.Join(dir, "fp.json"))
	fp1 := c.Of(path)
	if fp1.Digest == "" || fp1.Method != "quick" {
		t.Fatalf("首次指纹应为 quick 且非空，实际 %+v", fp1)
	}
	if fp2 := c.Of(path); fp2.Digest != fp1.Digest {
		t.Fatalf("同一文件两次指纹不一致：%s != %s", fp1.Digest, fp2.Digest)
	}

	// 改内容（大小不变）必须换指纹 —— 只靠 size/mtime 会漏掉同尺寸替换
	if err := os.WriteFile(path, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 显式推进 mtime 到 2 秒后，避免文件系统时间粒度导致的假「未变化」
	if err := os.Chtimes(path, fp1Time().Add(2*time.Second), fp1Time().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	fp3 := c.Of(path)
	if fp3.Digest == fp1.Digest {
		t.Errorf("内容变了但指纹没变（%s）——同尺寸替换会被漏检", fp3.Digest)
	}
}

// 指纹缓存必须落盘并能读回（重启后不重算 20GB）。
func TestFingerprintCache_Persist(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "w.safetensors")
	if err := os.WriteFile(modelPath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, "fp.json")

	c1 := LoadFingerprintCache(cachePath)
	fp := c1.Of(modelPath)
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

	c2 := LoadFingerprintCache(cachePath)
	got := c2.Of(modelPath)
	if got.Digest != fp.Digest || got.Bytes != fp.Bytes {
		t.Fatalf("读回指纹不一致：%+v vs %+v", got, fp)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("缓存文件应存在：%v", err)
	}
}

func TestModelRolesForNodes(t *testing.T) {
	roles := ModelRolesForNodes([]string{"text2image", "text2video", "tts", "unknown"})
	has := func(r string) bool {
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}
	for _, want := range []string{RoleT2IPrimary, RoleVideoH3Ref2va} {
		if !has(want) {
			t.Errorf("缺少角色 %s，实际 %v", want, roles)
		}
	}
	if ModelRolesForNodes([]string{"tts"}) != nil {
		t.Errorf("tts 不应映射任何 bundle 角色（Kokoro 不在 bundle 里）")
	}
}

// manifest 必须可读回、可序列化，且产物缺失时不能编造路径。
func TestManifest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	art := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(art, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{
		Version:    1,
		RunID:      7,
		PipelineID: "film",
		Engine:     "test",
		Params:     map[string]any{"seconds": float64(30)},
		Nodes: []NodeManifest{{
			NodeID:    "sl-1",
			NodeType:  "shotlist",
			Status:    "success",
			Key:       "k1",
			Artifacts: []ArtifactRef{{Path: art, Kind: "video", Bytes: 2048}},
		}},
	}
	path, err := WriteManifest(filepath.Join(dir, "manifests"), m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.RunID != 7 || len(back.Nodes) != 1 || back.Nodes[0].Key != "k1" {
		t.Fatalf("读回内容不一致：%+v", back)
	}
	if _, ok := ArtifactRefOf(filepath.Join(dir, "nope.mp4")); ok {
		t.Errorf("不存在的产物不应生成引用")
	}
	if _, ok := ArtifactRefOf(art); !ok {
		t.Errorf("存在的产物应生成引用")
	}
}

func TestArtifactRefOf_Kind(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"a.mp4": "video", "b.png": "image", "c.wav": "audio", "d.srt": "text", "e.bin": "other",
	}
	for name, want := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		ref, ok := ArtifactRefOf(p)
		if !ok {
			t.Fatalf("%s 应被识别", name)
		}
		if ref.Kind != want {
			t.Errorf("%s 类型应为 %s，实际 %s", name, want, ref.Kind)
		}
	}
}

// BuildManifest 必须把 params/模型/产物/耗时都带上，缺一就不算可复现。
func TestBuildManifest_Basic(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "unet.safetensors")
	if err := os.WriteFile(modelPath, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	artPath := filepath.Join(dir, "shot1.mp4")
	if err := os.WriteFile(artPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	fps := LoadFingerprintCache(filepath.Join(dir, "fp.json"))
	now := fp1Time()
	m := BuildManifest(RunMeta{
		ID: 9, PipelineID: "film", Params: map[string]any{"prompt": "x"},
		StartedAt: &now, EndedAt: &now,
	}, []NodeInstanceLite{{
		ID: "sl-1", Type: "shotlist", Status: "success",
		Seed: 42, DurationMS: 120000,
		Params:        map[string]any{"seconds": float64(10)},
		ModelNames:    map[string]string{"video.h3.ref2va.unet": "unet.safetensors"},
		ArtifactPaths: []string{artPath},
	}}, fps, "test")

	if len(m.Nodes) != 1 {
		t.Fatalf("节点数应为 1，实际 %d", len(m.Nodes))
	}
	n := m.Nodes[0]
	if n.Key == "" {
		t.Error("有参数/模型/产物时应生成 key")
	}
	if n.Models["video.h3.ref2va.unet"] == "" {
		t.Error("模型指纹不应为空")
	}
	if len(n.Artifacts) != 1 || n.Artifacts[0].SHA256 == "" {
		t.Errorf("产物引用不完整：%+v", n.Artifacts)
	}
	if n.DurationMS != 120000 {
		t.Errorf("耗时丢失：%d", n.DurationMS)
	}

	// 序列化必须能过（handler 要把它塞进 API 响应）
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("manifest 不可序列化：%v", err)
	}
}

func fp1Time() time.Time { return time.Now() }

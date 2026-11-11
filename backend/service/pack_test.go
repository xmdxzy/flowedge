package service

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTmp 造一个素材文件
func writeTmp(t *testing.T, dir, name string, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("造素材失败：%v", err)
	}
	return p
}

func nodeTree(t *testing.T, refs []string, bgm string) json.RawMessage {
	t.Helper()
	nodes := []map[string]interface{}{{
		"id": "sl-1", "type": "shotlist",
		"data": map[string]interface{}{
			"ref_images": refs,
			"bgm":        bgm,
			"prompt":     "一个不该被当成文件的字符串",
			"url":        "https://example.com/x.png",
			"width":      608,
		},
	}}
	b, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeNodes(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("节点 JSON 解析失败：%v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("应只有 1 个节点，实际 %d", len(arr))
	}
	return arr[0]["data"].(map[string]interface{})
}

// 导演包往返：素材进包、图里的绝对路径换成包内相对路径、导入后还原成新机器上的绝对路径。
func TestPackRoundTrip(t *testing.T) {
	src := t.TempDir()
	img := writeTmp(t, src, "角色 定妆.png", "IMGDATA")
	aud := writeTmp(t, src, "bgm.wav", "AUDIO")
	nodes := nodeTree(t, []string{img, ""}, aud)

	zipPath := filepath.Join(t.TempDir(), "p.fepack.zip")
	m, err := ExportPack("测试片", nodes, json.RawMessage(`[]`), zipPath)
	if err != nil {
		t.Fatalf("导出失败：%v", err)
	}
	if m.Format != PackFormat || m.FormatVersion != PackFormatVersion {
		t.Fatalf("清单头不对：%+v", m.Format)
	}
	if len(m.Assets) != 2 {
		t.Fatalf("应收 2 个素材（空字符串和 URL 不算），实际 %d：%+v", len(m.Assets), m.Assets)
	}
	for _, a := range m.Assets {
		if !strings.HasPrefix(a.File, packAssetPrefix) {
			t.Fatalf("素材应落在 %s 下，实际 %s", packAssetPrefix, a.File)
		}
		for _, r := range a.File {
			if r > 127 {
				t.Fatalf("包内路径必须全 ASCII，实际含 %q（%s）", r, a.File)
			}
		}
		if a.SHA256 == "" {
			t.Fatal("必须带 sha256：导入时要靠它发现包被截断/篡改")
		}
	}

	// 导出后的图里应已经是包内相对路径
	exp := decodeNodes(t, m.Nodes)
	if got := exp["ref_images"].([]interface{})[0].(string); !strings.HasPrefix(got, packAssetPrefix) {
		t.Fatalf("导出后 ref_images[0] 应为包内相对路径，实际 %s", got)
	}
	if got := exp["prompt"].(string); got != "一个不该被当成文件的字符串" {
		t.Fatalf("非文件字段被改动了：%q", got)
	}
	if got := exp["url"].(string); got != "https://example.com/x.png" {
		t.Fatalf("URL 不该被当成文件：%q", got)
	}

	// 导入到「另一台机器」
	dest := t.TempDir()
	m2, nodes2, edges2, err := ImportPack(zipPath, dest)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	if m2.Name != "测试片" {
		t.Fatalf("包名丢了：%q", m2.Name)
	}
	if string(edges2) != "[]" {
		t.Fatalf("边没带过来：%s", edges2)
	}
	got := decodeNodes(t, nodes2)
	ref := got["ref_images"].([]interface{})[0].(string)
	if !strings.HasPrefix(ref, dest) {
		t.Fatalf("导入后素材路径应指向解包目录 %s，实际 %s", dest, ref)
	}
	if b, err := os.ReadFile(ref); err != nil || string(b) != "IMGDATA" {
		t.Fatalf("素材内容没还原：%v %q", err, string(b))
	}
	if bgm := got["bgm"].(string); !strings.HasPrefix(bgm, dest) {
		t.Fatalf("bgm 没改写：%s", bgm)
	}
	// 空字符串位不该被包塞进路径
	if got["ref_images"].([]interface{})[1].(string) != "" {
		t.Fatalf("空素材位被改动了：%v", got["ref_images"].([]interface{})[1])
	}
}

// 恶意包：路径穿越必须拒绝（不拒绝就能往工作区外面写文件）。
func TestPackImportRejectsTraversal(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "evil.fepack.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../evil.txt")
	_, _ = w.Write([]byte("pwned"))
	_ = zw.Close()
	_ = f.Close()

	if _, _, _, err := ImportPack(zipPath, t.TempDir()); err == nil {
		t.Fatal("含 ../ 的包必须被拒绝")
	}
}

func TestPackImportRejectsWrongFormat(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "other.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create(packManifestName)
	_, _ = w.Write([]byte(`{"format":"somebody-else","format_version":1}`))
	_ = zw.Close()
	_ = f.Close()

	if _, _, _, err := ImportPack(zipPath, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "不是 FlowEdge 导演包") {
		t.Fatalf("应报格式不符，实际 %v", err)
	}
}

func TestPackImportRejectsFutureVersion(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "future.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create(packManifestName)
	_, _ = w.Write([]byte(`{"format":"flowedge-pack","format_version":99,"nodes":[],"edges":[]}`))
	_ = zw.Close()
	_ = f.Close()

	if _, _, _, err := ImportPack(zipPath, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "比当前支持") {
		t.Fatalf("应报版本过新，实际 %v", err)
	}
}

// 包被截断/改过：sha256 对不上必须拒绝，不能"图能跑但素材是上一版"。
func TestPackImportDetectsCorruptAsset(t *testing.T) {
	src := t.TempDir()
	img := writeTmp(t, src, "a.png", "REAL")
	nodes := nodeTree(t, []string{img}, "")

	good := filepath.Join(t.TempDir(), "g.zip")
	if _, err := ExportPack("x", nodes, nil, good); err != nil {
		t.Fatal(err)
	}

	// 拆开重打，把素材内容换掉但保留原 sha
	zr, err := zip.OpenReader(good)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	bad := filepath.Join(t.TempDir(), "bad.zip")
	out, _ := os.Create(bad)
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		if strings.HasPrefix(f.Name, packAssetPrefix) {
			_, _ = w.Write([]byte("TAMPERED"))
			continue
		}
		rc, _ := f.Open()
		buf := make([]byte, 1<<20)
		n, _ := rc.Read(buf)
		rc.Close()
		_, _ = w.Write(buf[:n])
	}
	_ = zw.Close()
	_ = out.Close()

	if _, _, _, err := ImportPack(bad, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("素材被改动必须报校验失败，实际 %v", err)
	}
}

// 包内文件名折叠：中文/空格/特殊字符一律变 ASCII，且保留扩展名。
// 全中文的 stem 会折成空 → 退化成 asset（前面还有 001_ 序号保证唯一、不串位）。
func TestAsciiAssetName(t *testing.T) {
	cases := map[string]string{
		"角色 定妆.png":       "asset.png",
		"a b/c.mp4":       "c.mp4",
		"素材（定稿）2.wav":     "2.wav",
		"ok-name_1.webp":  "ok-name_1.webp",
		`C:\x\y\test.PNG`: "test.png",
	}
	for in, want := range cases {
		got := asciiAssetName(in)
		for _, r := range got {
			if r > 127 {
				t.Fatalf("asciiAssetName(%q) 结果里还有非 ASCII：%q", in, got)
			}
		}
		if got != want {
			t.Fatalf("asciiAssetName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// JSON Pointer 的转义：键名里带 / 或 ~ 时不能把路径解析错位。
func TestSetByPointerEscaping(t *testing.T) {
	tree := map[string]interface{}{
		"a/b": map[string]interface{}{"c~d": "old"},
	}
	setByPointer(tree, "/a~1b/c~0d", "new")
	got := tree["a/b"].(map[string]interface{})["c~d"]
	if got != "new" {
		t.Fatalf("带转义的指针没命中，实际 %v", got)
	}
}

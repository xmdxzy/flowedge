package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 文件源这条路以前根本不存在：外部音频/图片/视频只能写进下游节点的 data.xxx_path，
// 于是 `start → asr` 这种画法永远接不上（start 只输出 text），模型和人都会反复试错。
// 下面这些用例盯的是「把外部文件变成一条正常的边」这件事本身。

func srcNode(data map[string]interface{}) *model.NodeInstance {
	if data == nil {
		data = map[string]interface{}{}
	}
	return &model.NodeInstance{ID: "src", Type: "input_source", Data: data}
}

// 临时造一个非空文件
func touchFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("fake-media-bytes"), 0o644); err != nil {
		t.Fatalf("造文件失败：%v", err)
	}
	return p
}

// 显式 kind：只输出那一个媒体键，其它端口不出现。
//
// 这条是核心语义。如果四个键都填同一个路径，吃 audio 的节点就会拿到一个 mp4 路径，
// ffmpeg 报一句与真实原因无关的错，排查方向直接跑偏。
func TestInputSource_OnlyEmitsItsOwnPort(t *testing.T) {
	dir := t.TempDir()
	audio := touchFile(t, dir, "a.wav")

	e := &EngineV2{}
	out, err := e.execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "audio", "path": audio,
	}), nil)
	if err != nil {
		t.Fatalf("应成功：%v", err)
	}
	m := out.(map[string]interface{})
	if m["audio"] != audio {
		t.Fatalf("audio 端口应给出绝对路径 %q，实际 %v", audio, m["audio"])
	}
	for _, k := range []string{"image", "video", "text"} {
		if _, exists := m[k]; exists {
			t.Fatalf("kind=audio 时不该有 %q 端口（会让下游拿到错误类型的路径）", k)
		}
	}
	if m["kind"] != "audio" {
		t.Fatalf("要带 kind 元信息，实际 %v", m["kind"])
	}
	if b, ok := m["bytes"].(int64); !ok || b == 0 {
		t.Fatalf("要带文件字节数（任务书与血缘要用），实际 %v", m["bytes"])
	}
}

// kind 留空 → 按扩展名推断。人不填类型是常态（填了 .wav 就是音频，显然）
func TestInputSource_InfersKindFromExtension(t *testing.T) {
	dir := t.TempDir()
	img := touchFile(t, dir, "frame.PNG") // 大写扩展名也要认

	out, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{"path": img}), nil)
	if err != nil {
		t.Fatalf("应成功：%v", err)
	}
	if out.(map[string]interface{})["image"] != img {
		t.Fatalf("应推断为 image：%v", out)
	}
}

// 占位值：图先搭、文件后补是正常流程。
// 报错必须说"还没给文件"，而不是"路径不存在" —— 后者会让人去查拼写。
func TestInputSource_PlaceholderSaysNotProvided(t *testing.T) {
	for _, ph := range []string{"待提供", "TODO", "<path>", "{{file}}"} {
		_, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
			"kind": "video", "path": ph,
		}), nil)
		if err == nil {
			t.Fatalf("占位值 %q 应报错（否则会拿着占位路径往下跑）", ph)
		}
		msg := err.Error()
		if !strings.Contains(msg, "占位") && !strings.Contains(msg, "还没有指定") {
			t.Fatalf("占位值 %q 的错误要说明是「还没给文件」，实际：%s", ph, msg)
		}
	}
}

// 真实文件缺失 / 是目录 / 是空文件 —— 三种要分开报，不能都说"读不到"
func TestInputSource_RejectsBadPaths(t *testing.T) {
	dir := t.TempDir()

	_, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "audio", "path": filepath.Join(dir, "nope.wav"),
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺文件应报「不存在」，实际：%v", err)
	}

	_, err = (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "image", "path": dir,
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("给目录应报「要的是文件路径」，实际：%v", err)
	}

	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "video", "path": empty,
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("空文件应报出来（0 字节的视频跑下去只会得到一堆 ffmpeg 报错），实际：%v", err)
	}
}

// 声明的类型与扩展名冲突 → 明确报错，不猜。
// 猜错的代价是下游跑到一半才炸，那时错误信息已经离原因很远。
func TestInputSource_KindExtensionMismatch(t *testing.T) {
	dir := t.TempDir()
	mp3 := touchFile(t, dir, "voice.mp3")

	_, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "image", "path": mp3,
	}), nil)
	if err == nil {
		t.Fatal("kind=image 配 .mp3 应拒绝")
	}
	msg := err.Error()
	if !strings.Contains(msg, "audio") {
		t.Fatalf("要说清它看起来是 audio，实际：%s", msg)
	}
}

// text 类型：来自 text 参数，不需要文件
func TestInputSource_TextKind(t *testing.T) {
	out, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "text", "text": "这是一段外部给的文案",
	}), nil)
	if err != nil {
		t.Fatalf("应成功：%v", err)
	}
	if out.(map[string]interface{})["text"] != "这是一段外部给的文案" {
		t.Fatalf("文本没透出：%v", out)
	}

	// 空的 text 要报清楚（不是"缺少 path"，因为它压根不需要 path）
	_, err = (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{"kind": "text"}), nil)
	if err == nil || !strings.Contains(err.Error(), "内容") {
		t.Fatalf("空文本应报「没有内容」，实际：%v", err)
	}
}

// 相对路径要转绝对：下游 ffmpeg / ASR 的工作目录不一定与引擎相同
func TestInputSource_RelativePathBecomesAbsolute(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, dir, "rel.wav")

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	out, err := (&EngineV2{}).execInputSourceNode(srcNode(map[string]interface{}{
		"kind": "audio", "path": "rel.wav",
	}), nil)
	if err != nil {
		t.Fatalf("应成功：%v", err)
	}
	got := out.(map[string]interface{})["audio"].(string)
	if !filepath.IsAbs(got) {
		t.Fatalf("应转成绝对路径，实际 %q", got)
	}
}

// 契约里的四个输出端口都是 optional —— 它们互斥，没有"必填输出"这回事。
// 写成 Required 会让任务书报「必填输出无产出」，那是错的。
func TestInputSourceContract_AllOutputsOptional(t *testing.T) {
	c, ok := ModuleContractOf("input_source")
	if !ok {
		t.Fatal("input_source 没有契约 —— 任务书会缺这一段")
	}
	if c.Role != RoleSource {
		t.Fatalf("角色应为 source（无输入），实际 %v", c.Role)
	}
	if len(c.Outputs) != 4 {
		t.Fatalf("应有 4 个互斥输出端口，实际 %d", len(c.Outputs))
	}
	for _, p := range c.Outputs {
		if p.Required {
			t.Fatalf("端口 %q 不该标必填（四个互斥，只会有一个有值）", p.Name)
		}
	}
	// 不吃显存、不产新媒体 → 不该被算进重节点与血缘白名单
	spec := NodeSpecs["input_source"]
	if spec.Side != "" || spec.Heavy || spec.Media {
		t.Fatalf("文件源不该吃显存/算重节点/进媒体血缘：%+v", spec)
	}
}

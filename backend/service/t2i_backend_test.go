package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveT2IBackend(t *testing.T) {
	cases := []struct {
		name      string
		explicit  string
		prompt    string
		hasLayout bool
		want      T2IBackend
	}{
		{"默认走 flux", "", "一只猫坐在窗台上", false, BackendFlux},
		{"显式 flux", "flux", "海报，上面写着开业大吉", false, BackendFlux},
		{"显式 moss 覆盖一切", "moss", "一只猫", false, BackendMOSS},
		{"有布局必然 moss", "", "随便什么", true, BackendMOSS},
		{"中文文字信号", "", "木招牌上写着「清风茶馆」", false, BackendMOSS},
		{"英文文字信号", "", "a shop sign that reads OPEN", false, BackendMOSS},
		{"字幕", "", "画面下方带中文字幕", false, BackendMOSS},
		{"大小写不敏感", "MOSS", "一只猫", false, BackendMOSS},
	}
	for _, c := range cases {
		got, reason := ResolveT2IBackend(c.explicit, c.prompt, c.hasLayout)
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: 理由不能为空——用户要能看懂为什么慢了十倍", c.name)
		}
	}
}

// TestBuildMOSSLayoutMatchesOfficial 坐标换算必须与官方实现逐字一致。
//
// 官方在 comfy_extras/nodes_moss_image.py:41-42：
//
//	y0' = floor(round(y0*h/1000, 9)) - h/2   （其余同理，y1/x1 用 ceil）
//
// 换错 floor/ceil 方向或忘记减半，整张图的每个元素都会偏半格——
// 而且**不会报错**，只是字落在别的地方。这种错只能靠对着算一遍来防。
func TestBuildMOSSLayoutMatchesOfficial(t *testing.T) {
	boxes := []MOSSBoxSpec{
		{Type: "text", Text: "山海神兽", BBox: [4]int{80, 120, 220, 880}},
		{Type: "object", Desc: "一只白鹤", BBox: [4]int{400, 200, 900, 700}},
	}
	got, err := BuildMOSSLayoutJSON("一只白鹤站在石碑旁", boxes, 1024, 1024)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	var l MOSSLayout
	if err := json.Unmarshal([]byte(got), &l); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if l.BBoxCoordSpace != "abs_centralized" || l.BBoxFormat != "yxyx" {
		t.Fatalf("坐标空间/格式不对：%s / %s", l.BBoxCoordSpace, l.BBoxFormat)
	}
	if len(l.Items) != 2 {
		t.Fatalf("元素数 %d", len(l.Items))
	}
	// 第一个框：[80,120,220,880] @1024 → floor(80*1024/1000)-512 = 81-512 = -431
	want := [4]int{81 - 512, 122 - 512, 226 - 512, 902 - 512}
	if l.Items[0].BBox != want {
		t.Errorf("bbox 换算错：got %v want %v", l.Items[0].BBox, want)
	}
	if l.Items[0].Label != "text" || l.Items[0].Text != "山海神兽" {
		t.Errorf("text 元素丢了：%+v", l.Items[0])
	}
	if l.Items[1].Label != "object" {
		t.Errorf("object 元素类型错：%+v", l.Items[1])
	}
	if l.Summary.Caption != "一只白鹤站在石碑旁" {
		t.Errorf("summary 错：%q", l.Summary.Caption)
	}
}

// TestBuildMOSSLayoutEmpty 空布局必须返回空串而不是 "{}"。
// 空串意味着"退化成普通提示词"；返回空 JSON 会让 MOSS 拿到一个没有 items 的条件，
// 表现是图出得来但完全不听话——比报错更难查。
func TestBuildMOSSLayoutEmpty(t *testing.T) {
	got, err := BuildMOSSLayoutJSON("x", nil, 1024, 1024)
	if err != nil || got != "" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestParseMOSSBoxes(t *testing.T) {
	// 数组直给
	got := ParseMOSSBoxes(`[{"type":"text","text":"开业大吉","bbox":[100,200,300,800]}]`)
	if len(got) != 1 || got[0].Text != "开业大吉" {
		t.Fatalf("数组解析失败：%+v", got)
	}
	// 包一层 items
	got = ParseMOSSBoxes(`{"items":[{"type":"object","desc":"灯笼","bbox":[0,0,500,500]}]}`)
	if len(got) != 1 || got[0].Desc != "灯笼" {
		t.Fatalf("items 包裹解析失败：%+v", got)
	}
	// 上游 JSON 端口给的是已解析的对象
	got = ParseMOSSBoxes([]interface{}{
		map[string]interface{}{"type": "text", "text": "A", "bbox": []interface{}{float64(10), float64(20), float64(30), float64(40)}},
	})
	if len(got) != 1 || got[0].BBox != [4]int{10, 20, 30, 40} {
		t.Fatalf("对象解析失败：%+v", got)
	}
	// 垃圾输入必须返回 nil 而不是 panic（宽容降级，不让整张图失败）
	if got := ParseMOSSBoxes("这不是 JSON"); len(got) != 0 {
		t.Fatalf("垃圾输入应返回空：%+v", got)
	}
	if got := ParseMOSSBoxes(nil); len(got) != 0 {
		t.Fatalf("nil 应返回空：%+v", got)
	}
}

// TestBuildMOSSGraphShape MOSS 图的三个易错点必须有测试盯着。
// 这三个错都是「图能提交、但跑出来是糊图或 400」，靠肉眼 review 抓不住。
func TestBuildMOSSGraphShape(t *testing.T) {
	g := BuildMOSSGraph("p", "n", "u.safetensors", "c.safetensors", "v.safetensors", 1024, 1024, 50, 3.0, 1)
	loader := g["2"].(map[string]interface{})
	if loader["class_type"] != "CLIPLoader" {
		t.Fatalf("节点 2 应是 CLIPLoader")
	}
	in := loader["inputs"].(map[string]interface{})
	if in["type"] != "moss_image" {
		t.Errorf("CLIPLoader 的 type 必须是 moss_image（漏了会「检测不到模型类型」）：%v", in["type"])
	}
	if g["1"].(map[string]interface{})["class_type"] != "UNETLoader" {
		t.Errorf("MOSS 用 UNETLoader，不是 CheckpointLoaderSimple")
	}
	// latent 必须是 Flux2 的：MOSS 的 latent_format 是 Flux2，通道数与旧 latent 不同
	lat := g["6"].(map[string]interface{})
	if lat["class_type"] != "EmptyFlux2LatentImage" {
		t.Errorf("latent 节点错：%v（应为 EmptyFlux2LatentImage）", lat["class_type"])
	}
	// 不能插 ModelSamplingShift：模型定义自带 shift=5.0，再设一遍是叠乘
	for _, v := range g {
		if m, ok := v.(map[string]interface{}); ok {
			if m["class_type"] == "ModelSamplingShift" {
				t.Errorf("不要手动插 ModelSamplingShift：MOSS 自带 shift=5.0")
			}
		}
	}
	// 采样参数必须传进去而不是写死
	ks := g["7"].(map[string]interface{})["inputs"].(map[string]interface{})
	if ks["steps"] != 50 || ks["cfg"] != 3.0 {
		t.Errorf("steps/cfg 没传进去：%v / %v", ks["steps"], ks["cfg"])
	}
}

// TestBuildFluxGraphUnchanged flux 图必须与改造前逐节点一致。
// 升级不能悄悄改变既有出片结果——这条测试就是那条底线。
func TestBuildFluxGraphUnchanged(t *testing.T) {
	g := BuildFluxGraph("p", "", "flux1-schnell-fp8.safetensors", 1024, 768, 4, 1.0, 42)
	wantTypes := map[string]string{
		"4": "CheckpointLoaderSimple", "6": "CLIPTextEncode", "33": "CLIPTextEncode",
		"5": "EmptyLatentImage", "3": "KSampler", "8": "VAEDecode", "9": "SaveImage",
	}
	for id, ct := range wantTypes {
		node, ok := g[id].(map[string]interface{})
		if !ok {
			t.Fatalf("缺节点 %s", id)
		}
		if node["class_type"] != ct {
			t.Errorf("节点 %s: got %v want %s", id, node["class_type"], ct)
		}
	}
	lat := g["5"].(map[string]interface{})["inputs"].(map[string]interface{})
	if lat["width"] != 1024 || lat["height"] != 768 {
		t.Errorf("尺寸没传进去：%v", lat)
	}
}

// TestInstanceRoutesMOSS 模型名路由必须把 MOSS 权重送到 8190。
// 送错实例的报错是 ComfyUI 的 400，看不出是实例错了（AGENTS §0.2）。
func TestInstanceRoutesMOSS(t *testing.T) {
	r := LoadInstanceRegistry("", "")
	got := r.ResolveForModel("moss_image_sft_int8_convrot.safetensors")
	if got.Name != "moss" {
		t.Fatalf("MOSS 权重路由到 %q（应在 moss 实例）", got.Name)
	}
	if !strings.Contains(got.BaseURL, "8190") {
		t.Errorf("MOSS 实例地址错：%s", got.BaseURL)
	}
	// 既有路由不许被动过
	if r.ResolveForModel("flux1-schnell-fp8.safetensors").Name != "main" {
		t.Errorf("flux 不该被 MOSS 抢走")
	}
	if r.ResolveForModel("minimax_h3_fl2va_pruned_int8_convrot.safetensors").Name != "h3" {
		t.Errorf("H3 路由被动了")
	}
	if got.ExclusiveGroup == "" {
		t.Errorf("MOSS 必须进互斥组：11GB + flux 17GB > 24GB")
	}
}

// TestFirstExistingSkipsMissingPath 复用一个不存在的路径不能被当成"找到了"。
//
// 真实事故（2026-10-23）：MOSS 复用 H3 的 venv，代码里把 mossHome/.venv 写在前面，
// firstNonEmpty 选中它 → deps 面板对着一台能跑的机器报「venv 缺失」。
func TestFirstExistingSkipsMissingPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "python.exe")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope", "python.exe")
	if got := firstExisting(missing, real); got != real {
		t.Fatalf("got %q want %q", got, real)
	}
	if got := firstExisting(missing, ""); got != "" {
		t.Fatalf("全不存在时应返回空，got %q", got)
	}
}

// TestInstanceRegistryMigrationAddsMOSS 老配置文件必须自动补上新增实例。
//
// 真机复现（2026-10-23）：运行/data/instances.json 是老版本落盘的（只有 main + h3），
// 数据优先于内置默认 ⇒ moss 权重被路由到 8188，而 8188 上没有 MOSS 节点。
// 这个迁移不做，代码改得再对，用户的机器上也跑不起来。
func TestInstanceRegistryMigrationAddsMOSS(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "instances.json")
	old := `{"default":"main","instances":[
		{"name":"main","base_url":"http://127.0.0.1:8188","models":["flux*"],"enabled":true},
		{"name":"h3","base_url":"http://127.0.0.1:8189","models":["minimax_h3*"],"exclusive_group":"gpu-exclusive","enabled":true}
	]}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadInstanceRegistry(p, "")
	if got := r.ResolveForModel("moss_image_sft_int8_convrot.safetensors"); got.Name != "moss" {
		t.Fatalf("迁移后仍路由到 %q", got.Name)
	}
	// 落盘检查：迁移结果必须写回，否则下次启动又回到老状态
	raw, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(raw), `"moss"`) {
		t.Fatalf("迁移没有写回磁盘：%v", err)
	}
	// 用户已有配置不许被动
	if r.ResolveForModel("flux1-schnell-fp8.safetensors").Name != "main" {
		t.Errorf("main 被动了")
	}
	if r.ResolveForModel("minimax_h3_fl2va_pruned_int8_convrot.safetensors").Name != "h3" {
		t.Errorf("h3 被动了")
	}
}

// TestInstanceRegistryMigrationRespectsUserOverride 用户自己配过同族实例时不许再补一份。
func TestInstanceRegistryMigrationRespectsUserOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "instances.json")
	mine := `{"default":"main","instances":[
		{"name":"main","base_url":"http://127.0.0.1:8188","models":["flux*"],"enabled":true},
		{"name":"my-moss","base_url":"http://127.0.0.1:9999","models":["moss_image*"],"enabled":true}
	]}`
	if err := os.WriteFile(p, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadInstanceRegistry(p, "")
	if got := r.ResolveForModel("moss_image_sft_int8_convrot.safetensors"); got.Name != "my-moss" {
		t.Errorf("用户的同名族实例被内置的抢了：%q", got.Name)
	}
	for _, it := range r.All() {
		if it.Name == "moss" {
			t.Errorf("不该再补一份内置 moss")
		}
	}
}

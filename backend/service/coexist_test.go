package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// 显存共存判断的验收。
//
// 背景（实测）：卸载后重载 flux 要 183.3s，生成只要 3.0s。
// 所以「显存够就别卸载」是单项最值钱的优化。但判断错的代价是把已驻留的
// 扩散模型挤出去 → 下次出图又要 183s，比不优化更糟。
// 因此这里的每条断言都在守「不确定时必须退回保守卸载」。

// mockComfy 模拟 ComfyUI 的显存接口。
// freeMB 可在运行中改变，用来模拟「卸载后显存归还」。
type mockComfy struct {
	freeMB  atomic.Int64
	unloads atomic.Int32
	srv     *httptest.Server
}

func newMockComfy(t *testing.T, freeMB int64) *mockComfy {
	t.Helper()
	m := &mockComfy{}
	m.freeMB.Store(freeMB)
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			fmt.Fprintf(w, `{"devices":[{"vram_free":%d}]}`, m.freeMB.Load()<<20)
		case r.URL.Path == "/free" && r.Method == http.MethodPost:
			m.unloads.Add(1)
			m.freeMB.Store(23000) // 卸载后把显存交还，避免测试等满轮询
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// mockOllama 模拟 Ollama 的模型清单（size 单位为字节）
func mockOllama(t *testing.T, models map[string]int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		out := `{"models":[`
		first := true
		for name, size := range models {
			if !first {
				out += ","
			}
			first = false
			out += fmt.Sprintf(`{"name":%q,"size":%d}`, name, size<<20)
		}
		out += `]}`
		fmt.Fprint(w, out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func coexistEngine(t *testing.T, comfy *mockComfy, ollamaURL, model string) *EngineV2 {
	t.Helper()
	return NewEngineV2(
		NewLLMService(ollamaURL, model),
		NewText2ImageService(comfy.srv.URL, "flux1-schnell-fp8.safetensors"),
		nil, nil, nil, nil, nil,
	)
}

// TestCanCoexist_SmallModelFits 小模型装得下就该共存（省一次 183s）。
func TestCanCoexist_SmallModelFits(t *testing.T) {
	comfy := newMockComfy(t, 20000) // 20GB 空闲
	ollama := mockOllama(t, map[string]int64{"qwen2.5vl:7b": 6000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5vl:7b")

	if !e.canCoexistWithLanguage("qwen2.5vl:7b") {
		t.Fatal("20GB 空闲 + 6GB 模型应判定可共存")
	}
}

// TestCanCoexist_BigModelDoesNotFit 14B 与 flux 抢显存时必须让位。
func TestCanCoexist_BigModelDoesNotFit(t *testing.T) {
	comfy := newMockComfy(t, 6000) // flux 在里面，只剩 6GB
	ollama := mockOllama(t, map[string]int64{"qwen2.5:14b": 9000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5:14b")

	if e.canCoexistWithLanguage("qwen2.5:14b") {
		t.Fatal("只剩 6GB 时装不下 14B，必须判为不可共存")
	}
}

// TestCanCoexist_UnknownModelIsConservative 查不到模型体积就不动。
func TestCanCoexist_UnknownModelIsConservative(t *testing.T) {
	comfy := newMockComfy(t, 20000)
	ollama := mockOllama(t, map[string]int64{"other:7b": 6000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5:14b")

	if e.canCoexistWithLanguage("qwen2.5:14b") {
		t.Fatal("查不到模型体积时必须保守判为不可共存")
	}
}

// TestCanCoexist_UnreachableComfyIsConservative 拿不到真实空闲显存就不动。
func TestCanCoexist_UnreachableComfyIsConservative(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	url := dead.URL
	dead.Close()

	ollama := mockOllama(t, map[string]int64{"qwen2.5vl:7b": 6000})
	e := NewEngineV2(
		NewLLMService(ollama.URL, "qwen2.5vl:7b"),
		NewText2ImageService(url, "ckpt"),
		nil, nil, nil, nil, nil,
	)
	if e.canCoexistWithLanguage("qwen2.5vl:7b") {
		t.Fatal("查不到 ComfyUI 显存时必须保守判为不可共存")
	}
}

// TestCanCoexist_DisabledBySwitch 逃生阀门：关掉即回到「换侧必卸载」。
func TestCanCoexist_DisabledBySwitch(t *testing.T) {
	comfy := newMockComfy(t, 20000)
	ollama := mockOllama(t, map[string]int64{"qwen2.5vl:7b": 6000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5vl:7b")

	e.SetVRAMCoexist(false)
	if e.canCoexistWithLanguage("qwen2.5vl:7b") {
		t.Fatal("关闭开关后不该再判共存")
	}
}

// TestEnterLanguageSide_CoexistSkipsUnload 装得下时：不卸载、计 coexists。
func TestEnterLanguageSide_CoexistSkipsUnload(t *testing.T) {
	comfy := newMockComfy(t, 20000)
	ollama := mockOllama(t, map[string]int64{"qwen2.5vl:7b": 6000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5vl:7b")

	e.enterSide(sideDiffusion)() // 先在扩散侧
	e.enterLanguageSide("qwen2.5vl:7b")()

	if got := comfy.unloads.Load(); got != 0 {
		t.Fatalf("装得下时不该调 /free 卸载，实际调了 %d 次", got)
	}
	if got := e.CoexistCount(); got != 1 {
		t.Fatalf("应记 1 次共存省下的驱逐，实际 %d", got)
	}
	if got := e.SideUnloadCount(); got != 0 {
		t.Fatalf("不该记真实卸载，实际 %d", got)
	}
}

// TestEnterLanguageSide_UnloadsWhenNoSpace 装不下时照旧卸载。
func TestEnterLanguageSide_UnloadsWhenNoSpace(t *testing.T) {
	comfy := newMockComfy(t, 6000) // 剩余显存不够
	ollama := mockOllama(t, map[string]int64{"qwen2.5:14b": 9000})
	e := coexistEngine(t, comfy, ollama.URL, "qwen2.5:14b")

	e.enterSide(sideDiffusion)()
	e.enterLanguageSide("qwen2.5:14b")()

	if got := comfy.unloads.Load(); got != 1 {
		t.Fatalf("装不下时应调一次 /free，实际 %d 次", got)
	}
	if got := e.SideUnloadCount(); got != 1 {
		t.Fatalf("应记 1 次真实卸载，实际 %d", got)
	}
	if got := e.CoexistCount(); got != 0 {
		t.Fatalf("装不下时不该记共存，实际 %d", got)
	}
}

// TestModelSizeMB_Lookup 模型体积查询本身也要对（判断全靠它）。
func TestModelSizeMB_Lookup(t *testing.T) {
	ollama := mockOllama(t, map[string]int64{
		"qwen2.5:14b": 9000,
		"bge-m3":      1200,
	})
	svc := NewLLMService(ollama.URL, "qwen2.5:14b")

	if got := svc.ModelSizeMB("qwen2.5:14b"); got != 9000 {
		t.Fatalf("精确匹配应得 9000MB，实际 %d", got)
	}
	// 不带 tag 的名字应能回落匹配到同族模型
	if got := svc.ModelSizeMB("bge-m3"); got != 1200 {
		t.Fatalf("无 tag 模型应得 1200MB，实际 %d", got)
	}
	if got := svc.ModelSizeMB("not-installed"); got != 0 {
		t.Fatalf("不存在的模型应返回 0，实际 %d", got)
	}
	if got := svc.ModelSizeMB(""); got != 0 {
		t.Fatalf("空模型名应返回 0，实际 %d", got)
	}
}

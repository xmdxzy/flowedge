package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResidencyRoute_FallbackWhenMissing(t *testing.T) {
	r := LoadResidencyRoute(filepath.Join(t.TempDir(), "nope.json"))
	if len(r.Models) == 0 {
		t.Fatal("缺失文件时必须回退到内置画像，不能得到空表")
	}
	if r.TotalVRAMMB <= 0 {
		t.Fatalf("回退画像的总显存应 >0，实际 %d", r.TotalVRAMMB)
	}
}

func TestLoadResidencyRoute_ReadsFileAndKeepsSide(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	body := `{"total_vram_mb": 24576, "models": [
		{"name":"h3","type":"video","side":"diffusion","vram_mb":20000},
		{"name":"q14","type":"llm","side":"language","vram_mb":9000}
	]}`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	r := LoadResidencyRoute(path)
	if len(r.Models) != 2 {
		t.Fatalf("应读到 2 个模型，实际 %d", len(r.Models))
	}
	if r.HeaviestMB(sideDiffusion) != 20000 {
		t.Fatalf("diffusion 侧最重应为 20000，实际 %d", r.HeaviestMB(sideDiffusion))
	}
	if r.HeaviestMB(sideLanguage) != 9000 {
		t.Fatalf("language 侧最重应为 9000，实际 %d", r.HeaviestMB(sideLanguage))
	}
	desc := r.Describe()
	if desc == "" {
		t.Fatal("路线摘要不该为空（启动日志要打它）")
	}
}

func TestResidencyRoute_ApplyRegistersOnlyValid(t *testing.T) {
	s := NewGPUScheduler(24576)
	r := &ResidencyRoute{TotalVRAMMB: 24576, Models: []ResidencyModel{
		{Name: "ok", Type: "llm", Side: sideLanguage, VRAMMB: 9000},
		{Name: "", Side: sideLanguage, VRAMMB: 100},    // 无名：跳过
		{Name: "zero", Side: sideDiffusion, VRAMMB: 0}, // 无量：跳过
	}}
	if n := r.Apply(s); n != 1 {
		t.Fatalf("只应登记 1 个合法模型，实际 %d", n)
	}
}

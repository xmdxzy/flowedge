package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileStore_ExactMatchHighConfidence 同档位 3 次以上实测 → high
func TestProfileStore_ExactMatchHighConfidence(t *testing.T) {
	s := LoadProfileStore("", 24576)
	for i := 0; i < 3; i++ {
		s.Record(VRAMProfile{Kind: "image", Model: "flux", Width: 1024, Height: 1024, Steps: 4,
			PeakMB: 17000, WallMS: 3000, Samples: 1, Source: "sampled"})
	}
	p := s.Predict("image", "flux", 1024, 1024, 0, 4)
	if p.Confidence != "high" {
		t.Errorf("confidence = %q, want high (samples=%d)", p.Confidence, p.Samples)
	}
	if p.Extrapolated {
		t.Error("exact match must not be marked extrapolated")
	}
	if p.PeakMinMB >= p.PeakMB || p.PeakMaxMB <= p.PeakMB {
		t.Errorf("range must bracket the value: %d in [%d,%d]", p.PeakMB, p.PeakMinMB, p.PeakMaxMB)
	}
}

// TestProfileStore_SingleSampleIsMedium 样本不足必须降级 confidence——
// 一次实测不能当成"已验证"。
func TestProfileStore_SingleSampleIsMedium(t *testing.T) {
	s := LoadProfileStore("", 24576)
	s.Record(VRAMProfile{Kind: "h3", Width: 608, Height: 352, Frames: 124, Steps: 20,
		PeakMB: 21800, WallMS: 120000, Samples: 1})
	p := s.Predict("h3", "", 608, 352, 124, 20)
	if p.Confidence != "medium" {
		t.Errorf("confidence = %q, want medium", p.Confidence)
	}
}

// TestProfileStore_NoExactMatchIsLowAndExtrapolated 不同档位只能外推并明确标注
func TestProfileStore_NoExactMatchIsLowAndExtrapolated(t *testing.T) {
	s := LoadProfileStore("", 24576)
	s.Record(VRAMProfile{Kind: "video", Width: 608, Height: 352, Frames: 124, Steps: 20,
		PeakMB: 21800, WallMS: 120000, Samples: 5})
	// 问一个完全不同的档位（736x416 / 240 帧）
	p := s.Predict("video", "", 736, 416, 240, 20)
	if p.Confidence != "low" || !p.Extrapolated {
		t.Fatalf("expected low+extrapolated, got %q extrap=%v", p.Confidence, p.Extrapolated)
	}
	if p.PeakMinMB != p.PeakMB {
		t.Error("extrapolation must not pretend to know the lower bound")
	}
	if p.PeakMaxMB <= p.PeakMB {
		t.Error("extrapolation must give an upper bound")
	}
	if !strings.Contains(p.Basis, "外推") {
		t.Errorf("basis must disclose extrapolation: %s", p.Basis)
	}
}

// TestProfileStore_UnknownKindIsNone 一条样本都没有时必须说 none，不能编数
func TestProfileStore_UnknownKindIsNone(t *testing.T) {
	s := LoadProfileStore("", 24576)
	p := s.Predict("image", "", 1024, 1024, 0, 4)
	if p.Confidence != "none" || p.PeakMB != 0 {
		t.Errorf("expected none/no numbers, got %+v", p)
	}
}

// TestProfileStore_MergesSimilarBuckets 相近档位合并成一条，避免档位爆炸；
// 但参数差得远必须分开记。
func TestProfileStore_MergesSimilarBuckets(t *testing.T) {
	s := LoadProfileStore("", 24576)
	s.Record(VRAMProfile{Kind: "image", Width: 1024, Height: 1024, Steps: 4, PeakMB: 17000, WallMS: 3000})
	s.Record(VRAMProfile{Kind: "image", Width: 1024, Height: 1024, Steps: 4, PeakMB: 17400, WallMS: 3200})
	if len(s.All()) != 1 {
		t.Fatalf("similar buckets should merge, got %d", len(s.All()))
	}
	if got := s.All()[0].Samples; got != 2 {
		t.Errorf("samples = %d, want 2", got)
	}
	s.Record(VRAMProfile{Kind: "image", Width: 512, Height: 512, Steps: 20, PeakMB: 9000, WallMS: 1500})
	if len(s.All()) != 2 {
		t.Errorf("different bucket must stay separate, got %d", len(s.All()))
	}
}

// TestProfileStore_WarnsBeforeOOM 峰值超卡必须提前警告
func TestProfileStore_WarnsBeforeOOM(t *testing.T) {
	s := LoadProfileStore("", 24576)
	s.Record(VRAMProfile{Kind: "h3", Width: 1280, Height: 720, Frames: 124, Steps: 20,
		PeakMB: 24000, WallMS: 300000, Samples: 5})
	p := s.Predict("h3", "", 1280, 720, 124, 20)
	if p.Warning == "" {
		t.Error("expected OOM warning")
	}
	if !strings.Contains(p.Warning, "OOM") {
		t.Errorf("warning should name the failure mode: %s", p.Warning)
	}
}

// TestProfileStore_ModelSizeSourceIsLowConfidence 峰值来自模型体积估算时
// 必须降级并标注——"算出来的"不能当"测到的"。
func TestProfileStore_ModelSizeSourceIsLowConfidence(t *testing.T) {
	s := LoadProfileStore("", 24576)
	s.Record(VRAMProfile{Kind: "image", Width: 1024, Height: 1024, Steps: 4,
		PeakMB: 17000, WallMS: 1000, Samples: 3, Source: "model_size"})
	p := s.Predict("image", "", 1024, 1024, 0, 4)
	if p.Confidence != "low" || !p.PeakUnderestimated {
		t.Errorf("model_size source must be low+underestimated, got %q/%v", p.Confidence, p.PeakUnderestimated)
	}
}

// TestProfileStore_SaveRoundTrip 档案落盘后能读回来
func TestProfileStore_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vram_profiles.json")
	s := LoadProfileStore(path, 24576)
	s.Record(VRAMProfile{Kind: "video", Width: 832, Height: 480, Frames: 33, Steps: 20, PeakMB: 12000, WallMS: 64000})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	again := LoadProfileStore(path, 24576)
	if len(again.All()) != 1 || again.All()[0].PeakMB != 12000 {
		t.Errorf("round trip failed: %+v", again.All())
	}
}

// TestWithin_Tolerance 容差判定的边界
func TestWithin_Tolerance(t *testing.T) {
	if !within(1000, 1055, 0.10) {
		t.Error("5.5% should be within 10%")
	}
	if within(1000, 1200, 0.10) {
		t.Error("20% must be outside 10%")
	}
	if !within(0, 0, 0.10) {
		t.Error("both zero means same bucket")
	}
	if within(0, 100, 0.10) {
		t.Error("unspecified vs specified must not be treated as same bucket")
	}
}

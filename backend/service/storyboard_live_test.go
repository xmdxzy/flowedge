package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveStoryboardTwoStage 真机冒烟：走本机 Ollama 跑完整两段式剧本链。
// 平时跳过（不烧本地推理时间）；设 FLOWEDGE_LIVE_LLM=1 后在 backend 目录跑：
//
//	FLOWEDGE_LIVE_LLM=1 go test ./service/ -run TestLiveStoryboardTwoStage -v -timeout 600s
func TestLiveStoryboardTwoStage(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_LLM") == "" {
		t.Skip("真机冒烟需显式开启：FLOWEDGE_LIVE_LLM=1")
	}
	base := os.Getenv("FLOWEDGE_LIVE_LLM_BASE")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	model := os.Getenv("FLOWEDGE_LIVE_LLM_MODEL")
	if model == "" {
		model = "dsh-lens-8b:latest"
	}
	svc := NewLLMService(base, model)
	svc.client.Timeout = 240 * time.Second

	brief := "深夜便利店，店员小林发现每天凌晨 3:07 都有同一个穿灰色风衣的客人来买同一盒饭，从不开口说话；今晚店员决定跟出去看看。"
	plan, bp, err := planShotCreatively(svc, svc.Model, brief, "", "", 30, 6)
	if err != nil {
		t.Fatalf("真机两段式生成失败：%v", err)
	}
	if bp == nil {
		t.Fatal("结构层没跑成（bp=nil）：LLM 输出不可解析，先看引擎日志再下结论")
	}
	if len(plan.Shots) < 2 || len(plan.Shots) > 6 {
		t.Errorf("镜头数 %d 超出 2–6", len(plan.Shots))
	}
	if strings.TrimSpace(plan.Style) == "" {
		t.Error("风格锚为空")
	}
	for _, sh := range plan.Shots {
		if strings.TrimSpace(sh.Beat) == "" {
			t.Errorf("第 %d 镜缺 beat", sh.ID)
		}
		if strings.TrimSpace(sh.Action) == "" {
			t.Errorf("第 %d 镜缺 action", sh.ID)
		}
	}
	beats := map[string]bool{}
	for _, sh := range plan.Shots {
		beats[sh.Beat] = true
	}
	if len(beats) < 2 {
		t.Errorf("节拍只有 1 种：结构层没传导到分镜层")
	}
	// 产物落盘供人工审阅（go test 的工作目录是包目录，回仓库根 .tmp）
	out, _ := filepath.Abs(filepath.Join("..", "..", ".tmp", "storyboard-live.json"))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Logf("建 .tmp 目录失败：%v", err)
	}
	payload, _ := json.MarshalIndent(map[string]interface{}{"structure": bp, "plan": plan}, "", "  ")
	if err := os.WriteFile(out, payload, 0o644); err == nil {
		t.Logf("完整产物：%s", out)
	}
	t.Logf("title=%q logline=%q style=%q", plan.Title, bp.Logline, plan.Style)
}

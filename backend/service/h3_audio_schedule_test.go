package service

import "testing"

// 音频胡言乱语的第一层修复在采样调度上：官方明确「台词与音色需要 12+ 步，
// 8 步时音频是短板」，turbo LoRA 模型卡又要求 audio shift = 6。
// 这个测试锁住三件事：步数不再被压到 8、sigma 节点必须存在、低显存不能吞掉前两者。
func TestH3GraphAudioSchedule(t *testing.T) {
	if H3TurboLoRAFile() == "" {
		t.Skip("本机没有 turbo LoRA，跳过调度参数校验")
	}
	s := &Text2ImageService{}
	graph, err := s.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "test",
		Width: 608, Height: 352, Frames: 124,
		Turbo: true,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}

	shift, ok := graph["41"].(map[string]interface{})
	if !ok {
		t.Fatal("缺少 MiniMaxH3SigmaShift 节点：不接它就只能吃模型默认 12/3")
	}
	inputs := shift["inputs"].(map[string]interface{})
	if got := inputs["shift_audio"]; got != h3ShiftAudioTurbo {
		t.Fatalf("turbo 下 audio shift 应为 %.0f，实际 %v", h3ShiftAudioTurbo, got)
	}
	if got := inputs["shift_video"]; got != h3ShiftVideo {
		t.Fatalf("video shift 应为 %.0f，实际 %v", h3ShiftVideo, got)
	}

	// 步数：turbo 默认必须 ≥12（音频收敛线），不能被压回 8
	sched := graph["9"].(map[string]interface{})["inputs"].(map[string]interface{})
	if got, _ := sched["steps"].(int); got < h3TurboDefaultSteps {
		t.Fatalf("turbo 步数 %d 低于音频收敛线 %d", got, h3TurboDefaultSteps)
	}

	// 低显存不能把 LoRA / sigma 丢掉（旧写法接的是 UNETLoader 本体）
	low, err := s.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "test", Width: 608, Height: 352, Frames: 124,
		Turbo: true, LowVRAM: true,
	})
	if err != nil {
		t.Fatalf("低显存构图失败：%v", err)
	}
	att := low["30"].(map[string]interface{})["inputs"].(map[string]interface{})
	link, ok := att["model"].([]interface{})
	if !ok || len(link) != 2 {
		t.Fatalf("低显存节点的 model 输入异常：%v", att["model"])
	}
	if link[0].(string) == "1" {
		t.Fatal("低显存节点直接接了 UNETLoader：LoRA 与音频调度会被静默丢弃")
	}
}

// 非 turbo 时回到官方默认 audio shift 3（不是 6）
func TestH3GraphBaseAudioShift(t *testing.T) {
	s := &Text2ImageService{}
	graph, err := s.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "test", Width: 608, Height: 352, Frames: 124,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	inputs := graph["41"].(map[string]interface{})["inputs"].(map[string]interface{})
	if got := inputs["shift_audio"]; got != h3ShiftAudioBase {
		t.Fatalf("base 下 audio shift 应为 %.0f，实际 %v", h3ShiftAudioBase, got)
	}
}

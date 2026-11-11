package service

// VLM 判词接真机（V3 P0 ③ 的第二半）
//
// `vlm_judge.go` 只定了协议（模板 + 解析 + 门映射）。这里补上「抽帧 → 调本地 VLM → 出判词」，
// 并接进片尾门。默认**关闭**（`gates.vlm=true` 才开），理由：
// 一次 VLM 调用 10–30 秒、会占用 Ollama，不能悄悄改掉所有人的出片耗时。
//
// 三条纪律（与项目其他地方一致）：
//  1. **VLM 不可达 ≠ 通过**：拿不到判词记 warn 并写明原因，不静默放行；
//  2. **判词不可解析 ≠ 通过**：交给 VLMVerdict.GateCheck 处理（见 vlm_judge.go）；
//  3. **抽帧失败要能被看见**：报出 ffmpeg 的真实 stderr 首行，不写"画面质量不佳"。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	vlmFrameCount  = 3
	vlmFrameWidth  = 512 // 缩到 512 宽：VLM 看图足够，且显著省 token/显存
	vlmJudgeTimout = 120 * time.Second
)

// ExtractFramesUniform 在整段视频上均匀抽 n 帧 PNG。
//
// 用一次 ffmpeg 调用抽完（fps=1/step），而不是 n 次 seek —— 后者在长片上是 n 次解码启动，
// 且 seek 到关键帧之前会取到错的画面。
func ExtractFramesUniform(ctx context.Context, videoPath, outDir string, count int, durationSec float64) ([]string, error) {
	if count < 1 {
		count = vlmFrameCount
	}
	if durationSec <= 0 {
		durationSec = 5
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	// 每个间隔取一帧：count 帧落在 duration/(count+1) 的步长上，避开首尾黑帧
	step := durationSec / float64(count+1)
	if step < 0.04 {
		step = 0.04
	}
	pattern := filepath.Join(outDir, "vlmf_%02d.png")
	args := []string{
		"-hide_banner", "-nostats", "-y",
		"-i", videoPath,
		"-vf", fmt.Sprintf("fps=1/%.4f,scale=%d:-2", step, vlmFrameWidth),
		"-frames:v", strconv.Itoa(count),
		pattern,
	}
	cmd := hiddenExecContext(ctx, ffmpegBin(), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("抽帧失败（ffmpeg）：%s", firstNonEmptyStr(msg, err.Error()))
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "vlmf_") {
			continue
		}
		files = append(files, filepath.Join(outDir, e.Name()))
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("抽帧失败：ffmpeg 没产出任何帧（视频可能没有视频流）")
	}
	return files, nil
}

// judgeMedia 对一段媒体出 VLM 判词。图片直接送；视频抽 vlmFrameCount 帧。
func (e *EngineV2) judgeMedia(ctx context.Context, mediaPath, topic string) (VLMVerdict, error) {
	if e.llm == nil {
		return VLMVerdict{}, fmt.Errorf("LLM 服务未初始化")
	}
	model := strings.TrimSpace(e.visionModel)
	if model == "" {
		return VLMVerdict{}, fmt.Errorf("没有可用的本地视觉模型（Ollama 里没装能看图的模型）")
	}
	if mediaPath == "" {
		return VLMVerdict{}, fmt.Errorf("没有产物路径")
	}

	var images []string
	var labels []string
	if isStillImagePath(mediaPath) {
		images = []string{mediaPath}
		labels = []string{"帧1"}
	} else {
		info, err := probeGateMedia(mediaPath)
		if err != nil {
			return VLMVerdict{}, fmt.Errorf("探测媒体失败：%w", err)
		}
		dir, err := os.MkdirTemp("", "fe-vlm-")
		if err != nil {
			return VLMVerdict{}, err
		}
		defer os.RemoveAll(dir)
		frames, err := ExtractFramesUniform(ctx, mediaPath, dir, vlmFrameCount, info.seconds)
		if err != nil {
			return VLMVerdict{}, err
		}
		images = frames
		for i := range frames {
			labels = append(labels, fmt.Sprintf("帧%d", i+1))
		}
	}

	// 注意：LLMService.VisionChat 目前没有 ctx 入参，判词调用无法被 vlmJudgeTimout 兜住。
	// 这里曾建了 actx 却没用上（编译期的 declared and not used），先按现状直连；
	// 等 VisionChat 出 ctx 版本再把超时接回来，别用空变量糊过去。
	raw, _, err := e.llm.VisionChat(BuildJudgePrompt(topic, labels), images, model)
	if err != nil {
		return VLMVerdict{}, fmt.Errorf("VLM 调用失败：%w", err)
	}
	return ParseVLMVerdict(raw), nil
}

// appendVLMCheck 把 VLM 判词作为一条门检查追加进去（gates.vlm 未开时原样返回）。
//
// 刻意区分三种"没通过"：
//   - 判词说 fair/poor/bad → 内容问题（人要看片）
//   - 判词不可解析        → 协议问题（模板被动过 / 模型不听格式）
//   - 不可达/超时         → 环境问题（Ollama 没起 / 模型太大）
//
// 三者修复动作完全不同，混成一句"质量不佳"等于没报。
func (e *EngineV2) appendVLMCheck(ctx context.Context, mediaPath string, r gateRules, checks []gateCheck) []gateCheck {
	if !r.vlmEnabled {
		return checks
	}
	v, err := e.judgeMedia(ctx, mediaPath, r.vlmTopic)
	if err != nil {
		return append(checks, gateCheck{
			Name:   "vlm_content",
			OK:     false,
			Level:  "warn",
			Detail: "VLM 判词没做成（**不是通过**）：" + err.Error(),
			Fix: "确认 Ollama 在跑且装了视觉模型（`ollama list`），或用 gates.vlm=false 显式关掉这项；" +
				"不要因为它报错就当成画面没问题",
		})
	}
	return append(checks, v.GateCheck("vlm_content"))
}

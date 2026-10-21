package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// post_production 节点：竖屏重构 + BGM 混音（侧链闪避）+ 可选字幕，一次重编码做完。
//
// 为什么合成一个节点（docs/系统评估-2026-10-21.md §5.2）：
// 短视频交付的三道门槛各自做一个节点，就是三次 libx264 重编码——画质掉三轮。
// 只需要字幕时用更简单的 subtitles 节点；要竖屏/BGM 就用这个，字幕顺带一起出。
func (e *EngineV2) execPostProductionNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	in := gatherSubtitleInputs(node, upstream)
	if in.Video == "" {
		return nil, fmt.Errorf("后期节点缺少视频：连一条视频线，或填 video_path")
	}

	outPath := fmt.Sprintf("data/generated/post-%d.mp4", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outPath = p
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("输出目录无法创建：%w", err)
		}
	}

	spec := PostSpec{}

	// ---- 构图：按 aspect 给目标分辨率，width/height 可显式覆盖 ----
	aspect, _ := node.Data["aspect"].(string)
	switch aspect {
	case "9:16":
		spec.Width, spec.Height = 1080, 1920
	case "1:1":
		spec.Width, spec.Height = 1080, 1080
	case "16:9":
		spec.Width, spec.Height = 1920, 1080
	case "", "keep":
		// 不改构图
	default:
		return nil, fmt.Errorf("不认识的 aspect=%q（可选 9:16 / 1:1 / 16:9 / keep）", aspect)
	}
	if v, ok := node.Data["width"].(float64); ok && v > 0 {
		spec.Width = int(v)
	}
	if v, ok := node.Data["height"].(float64); ok && v > 0 {
		spec.Height = int(v)
	}
	if fit, _ := node.Data["fit"].(string); fit == "blur" {
		spec.Fit = "blur"
	}

	// ---- BGM ----
	if p, ok := node.Data["bgm"].(string); ok && p != "" {
		spec.BGM = p
		// 默认 -12dB：先按"垫底"给，嫌小的自己往上调
		spec.BGMGainDB = -12
		if v, ok := node.Data["bgm_gain"].(float64); ok && v != 0 {
			spec.BGMGainDB = v
		}
		// duck 默认开：短视频里人声必须压得住 BGM，显式关才关
		spec.Duck = true
		if v, ok := node.Data["duck"].(bool); ok {
			spec.Duck = v
		}
	}

	// ---- 响度归一（两遍法）----
	// 默认关：开了就会重编音频，且会改变成片"听感"。需要的场景显式给目标值。
	if v, ok := node.Data["loudness"].(float64); ok && v != 0 {
		spec.LoudnessTarget = v
	}
	if v, ok := node.Data["true_peak"].(float64); ok && v != 0 {
		spec.TruePeak = v
	}

	// ---- 字幕：只有真的给了来源才做，避免"没字幕需求却报错" ----
	strField := func(k string) string {
		v, _ := node.Data[k].(string)
		return strings.TrimSpace(v)
	}
	wantSubs := strField("srt_path") != "" || strField("plan") != "" ||
		strField("dialogues") != "" || in.PlanJSON != ""

	lines, source := 0, ""
	if wantSubs {
		srtPath := fmt.Sprintf("%s.srt", strings.TrimSuffix(outPath, filepath.Ext(outPath)))
		n, src, err := buildSubtitleFile(node, in, srtPath)
		if err != nil {
			return nil, err
		}
		lines, source, spec.SRT = n, src, srtPath
		if v, ok := node.Data["font_size"].(float64); ok && v > 0 {
			spec.FontSize = int(v)
		}
		if v, ok := node.Data["margin_v"].(float64); ok && v > 0 {
			spec.MarginV = int(v)
		}
	}

	// 什么都没配就不该白付一次重编码
	if spec.Width == 0 && spec.Height == 0 && spec.BGM == "" && spec.SRT == "" && spec.LoudnessTarget == 0 {
		return nil, fmt.Errorf("后期节点没有可做的动作：至少配 aspect（竖屏）、bgm（配乐）、字幕来源或响度归一之一")
	}

	if err := e.video.PostProcess(in.Video, outPath, spec); err != nil {
		return nil, err
	}

	absOut, _ := filepath.Abs(outPath)
	out := map[string]interface{}{
		"video":  absOut,
		"kind":   "video",
		"aspect": aspect,
		"width":  spec.Width,
		"height": spec.Height,
	}
	if spec.BGM != "" {
		out["bgm"] = filepath.Base(spec.BGM)
		out["duck"] = spec.Duck
	}
	if spec.LoudnessTarget != 0 {
		out["loudness"] = spec.LoudnessTarget
	}
	if spec.SRT != "" {
		absSRT, _ := filepath.Abs(spec.SRT)
		out["srt"] = absSRT
		out["lines"] = lines
		out["source"] = source
	}
	return out, nil
}

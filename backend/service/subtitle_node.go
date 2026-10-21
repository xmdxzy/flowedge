package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// 字幕来源的解析与落盘：subtitles 与 post_production 两个节点共用。
//
// 抽出来的原因：两者都要按「现成 srt → 分镜表 → 逐行文本」的优先级找字幕，
// 各写一份必然漂移（一处改了「无对白」的判定、另一处没改，同一份分镜会出两种字幕）。

// subtitleInputs 从节点数据与上游收集到的原始素材
type subtitleInputs struct {
	Video    string // 成片路径
	PlanJSON string // 分镜表 JSON
	Text     string // 逐行文本（上游 text 或节点 dialogues）
}

// gatherSubtitleInputs 收集素材。节点 Data 里的显式字段优先于上游连线。
func gatherSubtitleInputs(node *model.NodeInstance, upstream map[string]interface{}) subtitleInputs {
	var in subtitleInputs
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := m["video"].(string); ok && v != "" {
			in.Video = v
		}
		if v, ok := m["plan"].(string); ok && v != "" {
			in.PlanJSON = v
		}
		if v, ok := m["text"].(string); ok && v != "" {
			in.Text = v
		}
	}
	if v, ok := node.Data["video_path"].(string); ok && v != "" {
		in.Video = v
	}
	if v, ok := node.Data["plan"].(string); ok && v != "" {
		in.PlanJSON = v
	}
	if v, ok := node.Data["dialogues"].(string); ok && strings.TrimSpace(v) != "" {
		in.Text = v
	}
	return in
}

// buildSubtitleFile 按优先级生成 srt 文件，返回条数与来源标识。
func buildSubtitleFile(node *model.NodeInstance, in subtitleInputs, srtOut string) (int, string, error) {
	// 1. 现成字幕文件
	if p, ok := node.Data["srt_path"].(string); ok && p != "" {
		if _, err := os.Stat(p); err != nil {
			return 0, "", fmt.Errorf("srt_path 指向的文件不存在：%s", p)
		}
		if err := copyFile(p, srtOut); err != nil {
			return 0, "", fmt.Errorf("复制字幕失败：%w", err)
		}
		n := strings.Count(strings.TrimSpace(readFileOrEmpty(srtOut)), "\n\n") + 1
		return n, "srt_path", nil
	}

	// 2. 分镜表：按每镜 seconds 排轴，台词取「身份：原话」里的原话
	if in.PlanJSON != "" {
		plan, err := ParseShotPlan(in.PlanJSON)
		if err != nil {
			return 0, "", fmt.Errorf("plan 不是合法的分镜表：%w", err)
		}
		subs := SubtitlesFromPlan(*plan)
		if len(subs) == 0 {
			return 0, "", fmt.Errorf("分镜表里没有台词（每镜 dialogue 全是「无对白」）——" +
				"要么填 dialogues，要么确认分镜确实不需要字幕")
		}
		if err := os.WriteFile(srtOut, []byte(BuildSRT(subs)), 0o644); err != nil {
			return 0, "", fmt.Errorf("写字幕文件失败：%w", err)
		}
		return len(subs), "plan", nil
	}

	// 3. 逐行文本：按成片总时长均分（粗糙但比没有强）
	if strings.TrimSpace(in.Text) != "" {
		info, err := probeMedia(in.Video)
		if err != nil {
			return 0, "", fmt.Errorf("读不出视频时长，无法均分字幕：%w", err)
		}
		subs := SubtitlesFromLines(strings.Split(in.Text, "\n"), info.dur)
		if len(subs) == 0 {
			return 0, "", fmt.Errorf("dialogues 全为空")
		}
		if err := os.WriteFile(srtOut, []byte(BuildSRT(subs)), 0o644); err != nil {
			return 0, "", fmt.Errorf("写字幕文件失败：%w", err)
		}
		return len(subs), "dialogues", nil
	}

	return 0, "", fmt.Errorf("没有字幕来源：填 srt_path（现成字幕）、plan（分镜表）或 dialogues（逐行文本）")
}

// execSubtitlesNode 只做字幕：视频 + 字幕来源 → 硬字幕成片（并留下 .srt）。
//
// 需要同时做竖屏/BGM 时用 post_production —— 三条都做只重编码一次，
// 各做一个节点会连掉三轮画质。
func (e *EngineV2) execSubtitlesNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	in := gatherSubtitleInputs(node, upstream)
	if in.Video == "" {
		return nil, fmt.Errorf("字幕节点缺少视频：连一条视频线，或填 video_path")
	}

	outPath := fmt.Sprintf("data/generated/subtitled-%d.mp4", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outPath = p
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("输出目录无法创建：%w", err)
		}
	}
	srtPath := fmt.Sprintf("%s.srt", strings.TrimSuffix(outPath, filepath.Ext(outPath)))

	lines, source, err := buildSubtitleFile(node, in, srtPath)
	if err != nil {
		return nil, err
	}

	style := SubtitleStyle{}
	if v, ok := node.Data["font_size"].(float64); ok && v > 0 {
		style.FontSize = int(v)
	}
	if v, ok := node.Data["margin_v"].(float64); ok && v > 0 {
		style.MarginV = int(v)
	}

	if err := e.video.BurnSubtitles(in.Video, srtPath, outPath, style); err != nil {
		return nil, err
	}

	absOut, _ := filepath.Abs(outPath)
	absSRT, _ := filepath.Abs(srtPath)
	return map[string]interface{}{
		"video":  absOut,
		"srt":    absSRT,
		"lines":  lines,
		"source": source,
		"kind":   "video",
	}, nil
}

func readFileOrEmpty(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// subtitles 节点：视频 + 字幕来源 → 烧好字幕的成片（并留下 .srt 便于复查）。
//
// 字幕来源按优先级：
//  1. srt_path：现成的字幕文件（外部工具或人工校对过的版本）
//  2. plan：分镜表 JSON（来自 storyboard 或手填），按每镜时长排时间轴，台词取「身份：原话」里的原话
//  3. dialogues：逐行文本，按成片总时长均分（粗糙但比没有强）
//
// 不做的事：不做语音识别对齐（ASR 对齐是另一个动作，需要已经配好 ASR 服务）。
func (e *EngineV2) execSubtitlesNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	videoPath, planJSON, upstreamText := "", "", ""
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := m["video"].(string); ok && v != "" {
			videoPath = v
		}
		if v, ok := m["plan"].(string); ok && v != "" {
			planJSON = v
		}
		if v, ok := m["text"].(string); ok && v != "" {
			upstreamText = v
		}
	}
	if v, ok := node.Data["video_path"].(string); ok && v != "" {
		videoPath = v
	}
	if videoPath == "" {
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
	lines := 0
	source := ""

	// 1. 现成字幕文件
	if p, ok := node.Data["srt_path"].(string); ok && p != "" {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("srt_path 指向的文件不存在：%s", p)
		}
		if err := copyFile(p, srtPath); err != nil {
			return nil, fmt.Errorf("复制字幕失败：%w", err)
		}
		lines = strings.Count(strings.TrimSpace(readFileOrEmpty(srtPath)), "\n\n") + 1
		source = "srt_path"
	}

	// 2. 分镜表
	if source == "" {
		raw := planJSON
		if v, ok := node.Data["plan"].(string); ok && v != "" {
			raw = v
		}
		if raw != "" {
			plan, err := ParseShotPlan(raw)
			if err != nil {
				return nil, fmt.Errorf("plan 不是合法的分镜表：%w", err)
			}
			subs := SubtitlesFromPlan(*plan)
			if len(subs) == 0 {
				return nil, fmt.Errorf("分镜表里没有台词（每镜 dialogue 全是「无对白」）——" +
					"要么填 dialogues，要么确认分镜确实不需要字幕")
			}
			if err := os.WriteFile(srtPath, []byte(BuildSRT(subs)), 0o644); err != nil {
				return nil, fmt.Errorf("写字幕文件失败：%w", err)
			}
			lines, source = len(subs), "plan"
		}
	}

	// 3. 逐行文本均分
	if source == "" {
		var texts []string
		if v, ok := node.Data["dialogues"].(string); ok && strings.TrimSpace(v) != "" {
			texts = strings.Split(v, "\n")
		} else if strings.TrimSpace(upstreamText) != "" {
			texts = strings.Split(upstreamText, "\n")
		}
		if len(texts) > 0 {
			info, err := probeMedia(videoPath)
			if err != nil {
				return nil, fmt.Errorf("读不出视频时长，无法均分字幕：%w", err)
			}
			subs := SubtitlesFromLines(texts, info.dur)
			if len(subs) == 0 {
				return nil, fmt.Errorf("dialogues 全为空")
			}
			if err := os.WriteFile(srtPath, []byte(BuildSRT(subs)), 0o644); err != nil {
				return nil, fmt.Errorf("写字幕文件失败：%w", err)
			}
			lines, source = len(subs), "dialogues"
		}
	}

	if source == "" {
		return nil, fmt.Errorf("没有字幕来源：填 srt_path（现成字幕）、plan（分镜表）或 dialogues（逐行文本）")
	}

	style := SubtitleStyle{}
	if v, ok := node.Data["font_size"].(float64); ok && v > 0 {
		style.FontSize = int(v)
	}
	if v, ok := node.Data["margin_v"].(float64); ok && v > 0 {
		style.MarginV = int(v)
	}

	if err := e.video.BurnSubtitles(videoPath, srtPath, outPath, style); err != nil {
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

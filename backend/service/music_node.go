package service

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// defaultMusicModel 日常用的 DiT：XL turbo（4B，8 步）。
//
// 选 turbo 不是妥协：xl-sft 质量略高但要 50 步（慢一个数量级），而 BGM
// 是在成片里当底、且会被 loudnorm 与 ducking 再处理一遍，8 步的差距听不出来。
// 需要「主力曲」时把节点 data.model 改成 acestep-v15-xl-sft 即可。
const defaultMusicModel = "acestep-v15-xl-turbo"

// defaultMusicSteps turbo 官方推荐 8 步（base/sft 才要 32–64）。
const defaultMusicSteps = 8

// musicTimeout 音乐生成超时。
//
// 组成（2026-11-03 本机 3090 实测，比 README 的乐观口径长很多）：
//
//	冷加载 15–18 分钟  进程 + 4 个 XL shard（约 19GB 磁盘）+ LM tokenizer 49s
//	                   + LM 权重；其中「自己回头去 ModelScope 校验 29 个文件」占大头
//	生成     每 30s 音频约 5–15s（XL turbo 8 步，热态）
//
// 所以固定部分给 20 分钟。冷加载慢是**这个上游实现的现状**，不是本机配置问题
// （同一台机器上 H3 冷加载只要 50s，磁盘不是瓶颈），别用 8 分钟赌它。
func musicTimeout(n *model.NodeInstance) time.Duration {
	dur := fieldFloat(n, "duration", 30)
	if dur <= 0 {
		dur = 30
	}
	return 20*time.Minute + time.Duration(dur)*time.Second
}

// execMusicNode 用 ACE-Step 生成一段音乐（BGM / 纯音乐为主，也能带歌词）。
//
// 输出 {"audio": 绝对路径, "duration": 秒, "model": 模型名, "prompt": 实际用的提示词}。
// 与 tts 节点一样：服务没起就按需拉起，拉不起来就显式失败——
// 绝不返回空文件让下游「成功」出片（§0.12 ②）。
func (e *EngineV2) execMusicNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt := fieldString(node, "prompt")
	if prompt == "" {
		// 上游文本线兜底：画布上常见「storyboard → music」直连
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
					prompt = t
				}
			}
		}
	}
	lyrics := fieldString(node, "lyrics")
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(lyrics) == "" {
		return nil, fmt.Errorf("music 节点缺少 prompt：至少写一句风格/氛围（例：\"warm lo-fi piano, soft vinyl noise, 80 BPM\"）")
	}

	if e.music == nil {
		return nil, fmt.Errorf("music 节点不可用：引擎没有配置音乐服务（data/deps.json 的 acestep 段）")
	}

	// 服务没起就按需拉起。autostart=false 的语义是「不常驻、不白占显存」，
	// 不是「必须手动点启动」——和 tts 节点同一套纪律。
	if e.deps != nil && !e.deps.Healthy("acestep") && e.deps.Startable("acestep") {
		if startErr := e.deps.Start("acestep"); startErr != nil {
			log.Printf("[music] 按需启动失败：%v", startErr)
		} else if waitErr := e.deps.WaitReady("acestep", 300*time.Second); waitErr != nil {
			log.Printf("[music] 依赖已启动但未就绪：%v", waitErr)
		}
	}

	// 音乐生成在 GPU 上跑（XL 权重约 9GB），必须走显存仲裁：
	// 它进来之前要卸掉 H3 / Ollama / TTS，否则 24GB 卡上必然 OOM 或部分加载。
	release := e.enterSide(sideMusic)
	defer release()

	outputPath := fmt.Sprintf("data/generated/music-%d.wav", time.Now().UnixNano())
	if p := fieldString(node, "output_path"); p != "" {
		outputPath = p
	}

	modelName := fieldString(node, "model")
	if modelName == "" {
		modelName = defaultMusicModel
	}
	steps := int(fieldFloat(node, "steps", defaultMusicSteps))
	if steps <= 0 {
		steps = defaultMusicSteps
	}

	opt := MusicOptions{
		Prompt:   strings.TrimSpace(prompt),
		Lyrics:   lyrics,
		Duration: fieldFloat(node, "duration", 0),
		Model:    modelName,
		Steps:    steps,
		Language: fieldString(node, "language"),
		Thinking: paramBoolNode(node, "thinking"),
		Seed:     -1,
	}
	if v, ok := node.Data["seed"].(float64); ok && v >= 0 {
		opt.Seed = int64(v)
	}
	// 带歌词时默认英语发音不准是常见坑：没显式指定语言就按歌词里有没有中文猜
	if opt.Lyrics != "" && opt.Language == "" {
		if containsCJK(opt.Lyrics) {
			opt.Language = "zh"
		} else {
			opt.Language = "en"
		}
	}

	res, err := e.music.Generate(context.Background(), opt, outputPath)
	if err != nil {
		return nil, err
	}
	// 统一绝对路径：下游混音与画廊都直接用
	if abs, absErr := filepath.Abs(res.Path); absErr == nil {
		res.Path = abs
	}
	if res.Duration <= 0 {
		return nil, fmt.Errorf("音乐生成完成但探测不到时长（产物可能损坏）：%s", res.Path)
	}

	return map[string]interface{}{
		"audio":    res.Path,
		"duration": res.Duration,
		"model":    modelName,
		"prompt":   opt.Prompt,
		"seed":     res.Seed,
	}, nil
}

// paramBoolNode 读节点 data 里的布尔值（JSON bool；字符串 "true" 也认）。
func paramBoolNode(n *model.NodeInstance, key string) bool {
	if n == nil {
		return false
	}
	switch v := n.Data[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// containsCJK 判断文本里有没有中日韩字符（用来猜歌词语言）。
func containsCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3040 && r <= 0x30FF) {
			return true
		}
	}
	return false
}

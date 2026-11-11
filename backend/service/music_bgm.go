package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// 自动 BGM（2026-11-03）的默认参数。
//
// 设计取舍：BGM 是**底**不是主角，所以默认 -18dB 并开 ducking（人声一起来就压）；
// 想当氛围片用时把 bgm_gain 调到 -10、bgm_duck=false。
const (
	defaultBGMGainDB = -18.0
	// defaultBGMPrompt 用户只勾了「自动配乐」但没写风格时的兜底。
	// 挑「最不抢戏」的一档：无鼓点、无旋律主导、无人声。
	defaultBGMPrompt = "cinematic ambient underscore, warm pads, soft piano, no drums, no vocals, loopable"
	// bgmMaxGenSeconds ACE-Step 单次生成上限（服务端 10–600s）。
	bgmMaxGenSeconds = 600.0
)

// ApplyAutoBGM 给这次运行的成片自动生成并混入一条 BGM。
//
// 与字幕（ApplyTextOverlay）同一层级、同一纪律：**做不成就不许报成功**。
// 用户勾了「自动配乐」却拿到一支没配乐的片子，和字幕丢失是同一种假绿。
//
// 参数（都在 produce 的 params 里，不落到任何节点 data）：
//
//	bgm_auto     bool    勾了就生成，默认 false
//	bgm_prompt   string  风格描述；留空用 defaultBGMPrompt
//	bgm_gain     number  BGM 电平 dB，默认 -18（0 合法，>0 会被压回 0）
//	bgm_duck     bool    旁链压缩，默认 true
//	bgm_duration number  生成时长（秒）；0 = 跟成片等长（上限 600，超出由混音循环补齐）
//
// 返回新的成片路径（未请求时返回 ""，nil）。
func (e *EngineV2) ApplyAutoBGM(states []model.NodeInstance, params map[string]interface{}) (string, error) {
	if !paramBool(params, "bgm_auto") {
		return "", nil
	}
	// 用户自己给了 BGM 文件就按用户的来：参数化绝不覆盖显式输入。
	// （sl-1 已经在装配时用 bgm/bgm_gain/bgm_duck 混过一遍，这里再混就是两层 BGM。）
	if strings.TrimSpace(paramString(params, "bgm")) != "" {
		log.Printf("[bgm] 已指定 bgm 文件（%s），跳过自动配乐", paramString(params, "bgm"))
		return "", nil
	}
	if e.music == nil {
		return "", fmt.Errorf("自动配乐失败：引擎没有配置音乐服务（data/deps.json 的 acestep 段）")
	}

	src := pickArtifactPathFromStates(states)
	if src == "" {
		return "", fmt.Errorf("自动配乐失败：这次运行没有产出可混音的视频文件")
	}
	if ext := strings.ToLower(filepath.Ext(src)); !videoExts[ext] {
		return "", fmt.Errorf("自动配乐只支持视频产物，当前是 %q（%s）", ext, src)
	}
	dur, err := mediaSeconds(src)
	if err != nil || dur <= 0 {
		return "", fmt.Errorf("自动配乐失败：读不到成片时长（%s）：%v", filepath.Base(src), err)
	}

	prompt := paramString(params, "bgm_prompt")
	usedDefault := false
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultBGMPrompt
		usedDefault = true
	}
	gainDB := defaultBGMGainDB
	if v, ok := params["bgm_gain"].(float64); ok {
		gainDB = v
	}
	if gainDB > 0 {
		gainDB = 0 // 0dB 合法（环境声床），正增益一定是误填
	}
	duck := true
	if _, ok := params["bgm_duck"]; ok {
		duck = paramBool(params, "bgm_duck")
	}
	genSeconds := dur + 5 // 多给 5s，混音再按片长截断；少给会被循环听出接缝
	if v, ok := params["bgm_duration"].(float64); ok && v > 0 {
		genSeconds = v
	}
	if genSeconds > bgmMaxGenSeconds {
		genSeconds = bgmMaxGenSeconds
	}

	// 依赖按需拉起（与 music 节点同一套：autostart=false ≠ 必须手动点）
	if e.deps != nil && !e.deps.Healthy("acestep") && e.deps.Startable("acestep") {
		if startErr := e.deps.Start("acestep"); startErr != nil {
			return "", fmt.Errorf("自动配乐失败：拉起 ACE-Step 失败：%w", startErr)
		}
		if waitErr := e.deps.WaitReady("acestep", 600*time.Second); waitErr != nil {
			return "", fmt.Errorf("自动配乐失败：ACE-Step 未就绪：%w", waitErr)
		}
	}

	// 音乐侧独占显存（与扩散/语音换侧）：H3 的 20.5GB 和 XL 的 9GB 装不下同一张卡
	release := e.enterSide(sideMusic)
	bgmPath := fmt.Sprintf("data/generated/bgm-%d.wav", time.Now().UnixNano())
	res, genErr := e.music.Generate(context.Background(), MusicOptions{
		Prompt:   prompt,
		Duration: genSeconds,
		Steps:    8,
		Model:    defaultMusicModel,
	}, bgmPath)
	release()
	if genErr != nil {
		return "", fmt.Errorf("自动配乐失败：%w", genErr)
	}
	if usedDefault {
		log.Printf("[bgm] 未填 bgm_prompt，使用默认风格：%s", defaultBGMPrompt)
	}
	log.Printf("[bgm] 生成完成：%s（%.1fs，模型 %s）", filepath.Base(res.Path), res.Duration, res.Model)

	dst := strings.TrimSuffix(src, filepath.Ext(src)) + "-bgm.mp4"
	if err := mixAutoBGM(src, res.Path, dst, gainDB, duck); err != nil {
		return "", err
	}
	// 先把新文件验一遍再替换：混音失败却改了指针 = 产物清单指向坏文件
	if d2, derr := mediaSeconds(dst); derr != nil || d2 < dur-0.5 {
		_ = os.Remove(dst)
		return "", fmt.Errorf("自动配乐失败：混音产物时长异常（期望 %.1fs，实测 %.1fs）", dur, d2)
	}
	rewriteArtifactPath(states, src, dst)
	log.Printf("[bgm] 已混入成片：%s（电平 %.1fdB，duck=%v）", filepath.Base(dst), gainDB, duck)
	return dst, nil
}

// mixAutoBGM 混音；成片本身没有音轨时退化成「直接把 BGM 当音轨」。
//
// mixBGM 要求输入有音轨（它拿原音轨做旁链），而 code_video / 无声片默认
// 就是无音轨——直接抛错会让「自动配乐」在最需要它的场景失效。
func mixAutoBGM(in, bgm, out string, gainDB float64, duck bool) error {
	info, err := inspectMedia(in)
	if err != nil {
		return fmt.Errorf("自动配乐失败：读不到输入媒体信息：%w", err)
	}
	if info.HasAudio {
		return mixBGM(in, bgm, out, gainDB, duck)
	}
	if dur := info.VSeconds; dur > 0 {
		// 无音轨：把 BGM 铺满全片，视频流原样 copy
		fc := fmt.Sprintf("[1:a]volume=%.1fdB,atrim=0:%.3f,asetpts=PTS-STARTPTS[a]", gainDB, dur)
		args := []string{"-hide_banner", "-loglevel", "error", "-y",
			"-i", in,
			"-stream_loop", "-1", "-i", bgm,
			"-filter_complex", fc,
			"-map", "0:v:0", "-map", "[a]",
			"-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ar", "48000",
			"-t", fmt.Sprintf("%.3f", dur),
			out,
		}
		if b, cerr := hiddenExec(ffmpegBin(), args...).CombinedOutput(); cerr != nil {
			_ = os.Remove(out)
			return fmt.Errorf("自动配乐失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
		}
		return nil
	}
	return fmt.Errorf("自动配乐失败：输入既没有音轨也没有可读时长")
}

// paramString 读 produce 参数里的字符串（前后空白去掉）。
func paramString(params map[string]interface{}, key string) string {
	if v, ok := params[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

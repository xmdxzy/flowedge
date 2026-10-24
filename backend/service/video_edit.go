package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 视频片段拼接
//
// 为什么需要：H3 单次生成上限是 124 帧（5.17s @24fps），要出 15 秒镜头只能
// 「分段生成 + 用上一段末帧当下一段首帧」再拼起来。这条链路不依赖新模型，
// 只用已经装好的 H3 与系统 ffmpeg，是 24GB 上拉长时长的唯一务实做法。

// ffmpegBin 找 ffmpeg 可执行文件（PATH 或常见安装位置）
func ffmpegBin() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, c := range []string{`C:\ffmpeg\bin\ffmpeg.exe`, `D:\ffmpeg\bin\ffmpeg.exe`} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "ffmpeg"
}

// extractLastFrame 取视频最后一帧存成 png（用于下一段的首帧引导）
func extractLastFrame(videoPath, outPNG string) error {
	args := []string{
		"-y", "-v", "error",
		"-sseof", "-0.35", "-i", videoPath,
		"-vsync", "0", "-update", "1",
		"-frames:v", "1", outPNG,
	}
	cmd := exec.Command(ffmpegBin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("取末帧失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	if st, serr := os.Stat(outPNG); serr != nil || st.Size() < 1024 {
		return fmt.Errorf("末帧文件无效：%s", outPNG)
	}
	return nil
}

// concatVideos 把多段 mp4 拼成一个**带短交叉淡化**的连续镜头。
//
// 为什么不能直接 concat：
//  1. 每段末帧会作为下一段首帧 → 边界上有一帧重复，读起来是「卡一下」
//  2. 每段音频是独立生成的，环境声/音乐在接缝处硬切，听感突兀
//  3. 各段首帧的曝光/颗粒略有差异，硬切时会有闪一下的感觉
//
// 做法：视频 xfade 0.2s + 音频 acrossfade 0.2s（同长度，保证音画同步）。
// 0.2 秒在动作戏里读作运动模糊，不会像转场；再长就会变成「叠化」。
//
// 若任一段没有音轨（Wan 侧是无声音），退回旧的纯拼接，避免 acrossfade 失败。
func concatVideos(inputs []string, outPath string) error {
	if len(inputs) == 0 {
		return fmt.Errorf("没有可拼接的片段")
	}
	if len(inputs) == 1 {
		return copyFile(inputs[0], outPath)
	}

	infos := make([]mediaInfo, len(inputs))
	allAudio := true
	for i, p := range inputs {
		info, err := probeMedia(p)
		if err != nil {
			return err
		}
		if info.dur <= 0.05 {
			return fmt.Errorf("片段时长异常：%s", filepath.Base(p))
		}
		infos[i] = info
		if !info.hasAudio {
			allAudio = false
		}
	}
	if !allAudio {
		return concatSimple(inputs, outPath)
	}

	const xf = 0.20 // 交叉淡化时长（秒），视频与音频必须一致
	parts := make([]string, 0, len(inputs)*2)
	for i := range inputs {
		parts = append(parts, fmt.Sprintf("[%d:v]settb=AVTB,setpts=PTS-STARTPTS[v%d]", i, i))
		parts = append(parts, fmt.Sprintf("[%d:a]asetpts=PTS-STARTPTS[a%d]", i, i))
	}
	vcur, acur := "[v0]", "[a0]"
	total := infos[0].dur
	for i := 1; i < len(inputs); i++ {
		offset := total - xf
		if offset < 0 {
			offset = 0
		}
		vout := fmt.Sprintf("[vx%d]", i)
		aout := fmt.Sprintf("[ax%d]", i)
		parts = append(parts, fmt.Sprintf("%s[v%d]xfade=transition=fade:duration=%.3f:offset=%.3f%s",
			vcur, i, xf, offset, vout))
		parts = append(parts, fmt.Sprintf("%s[a%d]acrossfade=d=%.3f:c1=tri:c2=tri%s",
			acur, i, xf, aout))
		vcur, acur = vout, aout
		total += infos[i].dur - xf
	}

	args := []string{"-y", "-v", "error"}
	for _, p := range inputs {
		args = append(args, "-i", p)
	}
	args = append(args,
		"-filter_complex", strings.Join(parts, ";"),
		"-map", vcur, "-map", acur,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ar", "32000",
		"-movflags", "+faststart",
		outPath,
	)
	out, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("拼接失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	if st, serr := os.Stat(outPath); serr != nil || st.Size() < 4096 {
		return fmt.Errorf("拼接结果无效：%s", outPath)
	}
	return nil
}

// mediaInfo 用于拼接前探测时长与音轨
type mediaInfo struct {
	dur      float64
	hasAudio bool
}

// mediaCheck 成片自检结果（装配完成后自动跑一次，替代「看起来没问题」）
//
// 为什么必须在引擎里做：2026-10-19 出过一次「时长正常、音频完整、但中间整段画面没有」，
// 原因是 xfade 的 d 小于一帧。这种缺陷肉眼看总时长发现不了，必须数帧。
// 引擎自己产出就必须自己验收，不能依赖调用方记得手动检查。
type mediaCheck struct {
	OK       bool     `json:"ok"`
	Frames   int      `json:"frames"`
	FPS      float64  `json:"fps"`
	EffFPS   float64  `json:"effective_fps"`
	VSeconds float64  `json:"video_seconds"`
	ASeconds float64  `json:"audio_seconds"`
	HasAudio bool     `json:"has_audio"` // 显式化：原来只在 Note 里隐性携带（quality_gate 节点要消费）
	Size     string   `json:"size,omitempty"`
	Issues   []string `json:"issues,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// ffprobeBin 找 ffprobe。注意：它是可选二进制，缺失时自检降级为「未校验」，
// 绝不能让成片流程因此失败。
func ffprobeBin() string {
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	// ffprobe 通常与 ffmpeg 同目录
	if ff := ffmpegBin(); ff != "ffmpeg" {
		cand := filepath.Join(filepath.Dir(ff), "ffprobe.exe")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	for _, c := range []string{`C:\ffmpeg\bin\ffprobe.exe`, `D:\ffmpeg\bin\ffprobe.exe`} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// inspectMedia 数帧 + 比对音画时长，抓「丢段」这类时长看不出来的缺陷。
// replaceAudioTrack 用给定音轨 1:1 覆盖成片音轨，且**保证不丢帧**。
//
// 为什么需要它（AGENTS §5.8 ⚠）：本实例的 ref2va 不执行 fully_copy，模型会自己
// 生成一段音频。要交付「台词跨镜连续、音景不重启」，只能后期把准备好的那条 wav
// 铺上去——它必须整条覆盖，跨切点才不断。
//
// 两个必须遵守的细节：
//  1. **不能用 -shortest**：音频比视频短几毫秒就会把视频尾帧砍掉（实测 362 → 361），
//     而总时长只差 5ms，按「时长正常」验收永远发现不了。改用 apad 把音频补满，
//     再按视频精确时长 -t 截断。
//  2. 视频流用 -c:v copy：重编码既有代际损失又慢。
//
// 函数自己在返回前数一次帧：掉了就报错，而不是把一个少了帧的成片交出去。
func replaceAudioTrack(video, audio string) (string, error) {
	before, err := inspectMedia(video)
	if err != nil {
		return "", fmt.Errorf("替换音轨前读取视频信息失败：%w", err)
	}
	if before.VSeconds <= 0 {
		return "", fmt.Errorf("替换音轨失败：读不到视频时长（%s）", video)
	}
	out := strings.TrimSuffix(video, filepath.Ext(video)) + "-dub.mp4"
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-i", video, "-i", audio,
		"-map", "0:v", "-map", "1:a",
		"-c:v", "copy",
		"-af", "apad",
		"-c:a", "aac", "-b:a", "192k",
		"-t", fmt.Sprintf("%.3f", before.VSeconds),
		out,
	)
	if b, cerr := cmd.CombinedOutput(); cerr != nil {
		return "", fmt.Errorf("替换音轨失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
	}
	after, aerr := inspectMedia(out)
	if aerr == nil && before.Frames > 0 && after.Frames > 0 && after.Frames < before.Frames {
		return "", fmt.Errorf("替换音轨后帧数从 %d 掉到 %d（音频短于视频时若用了 -shortest 就会砍尾帧）",
			before.Frames, after.Frames)
	}
	return out, nil
}

func inspectMedia(path string) (mediaCheck, error) {
	chk := mediaCheck{OK: true}
	bin := ffprobeBin()
	if bin == "" {
		chk.Note = "未找到 ffprobe，跳过帧数校验"
		return chk, nil
	}
	args := []string{
		"-v", "error", "-print_format", "json",
		"-count_frames", "-show_entries",
		"stream=codec_type,width,height,r_frame_rate,duration,nb_read_frames:format=duration",
		path,
	}
	raw, err := exec.Command(bin, args...).Output()
	if err != nil {
		chk.Note = "ffprobe 执行失败，跳过自检"
		return chk, nil
	}
	var out struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			RFrameRate   string `json:"r_frame_rate"`
			Duration     string `json:"duration"`
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		chk.Note = "ffprobe 输出无法解析，跳过自检"
		return chk, nil
	}
	atof := func(s string) float64 {
		var f float64
		fmt.Sscanf(strings.TrimSpace(s), "%f", &f)
		return f
	}
	var vdur, adur float64
	haveAudio := false
	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			chk.Size = fmt.Sprintf("%dx%d", s.Width, s.Height)
			vdur = atof(s.Duration)
			chk.VSeconds = math.Round(vdur*1000) / 1000
			// r_frame_rate 形如 "24/1"
			if parts := strings.SplitN(s.RFrameRate, "/", 2); len(parts) == 2 {
				num, den := atof(parts[0]), atof(parts[1])
				if den != 0 {
					chk.FPS = num / den
				}
			}
			fmt.Sscanf(s.NbReadFrames, "%d", &chk.Frames)
		case "audio":
			haveAudio = true
			chk.HasAudio = true
			adur = atof(s.Duration)
			chk.ASeconds = math.Round(adur*1000) / 1000
		}
	}
	if chk.Size == "" {
		chk.OK = false
		chk.Issues = append(chk.Issues, "没有视频流：产物是纯音频")
		return chk, nil
	}
	// 1) 有效帧率 vs 标称帧率：有片段被吃掉时这两个对不上
	if chk.Frames > 0 && chk.FPS > 0 && vdur > 0.05 {
		chk.EffFPS = math.Round(float64(chk.Frames)/vdur*100) / 100
		if dev := math.Abs(chk.EffFPS-chk.FPS) / chk.FPS; dev > 0.03 {
			chk.OK = false
			chk.Issues = append(chk.Issues, fmt.Sprintf(
				"有效帧率 %.2f 与标称 %.2f 差 %.1f%%，疑似有片段画面丢失",
				chk.EffFPS, chk.FPS, dev*100))
		}
	}
	// 2) 音画时长差：xfade 吃掉整段时，音频仍是全长
	if haveAudio && vdur > 0 {
		if d := math.Abs(adur - vdur); d > 0.2 {
			chk.OK = false
			chk.Issues = append(chk.Issues, fmt.Sprintf(
				"音视频时长差 %.2fs（视频 %.2fs / 音频 %.2fs），拼接丢段或音轨未同步",
				d, vdur, adur))
		}
	}
	if !haveAudio {
		chk.Note = "无音轨"
	}
	return chk, nil
}

func probeMedia(path string) (mediaInfo, error) {
	// 只用 ffmpeg -i 解析：ffprobe 不一定在 PATH（本机就是这种情况），
	// 而拼接是必须成功的环节，不能依赖一个可选二进制。
	raw, _ := exec.Command(ffmpegBin(), "-i", path, "-hide_banner").CombinedOutput()

	txt := string(raw)
	var info mediaInfo
	for _, ln := range strings.Split(txt, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.Contains(ln, "Audio:") {
			info.hasAudio = true
		}
		if strings.Contains(ln, "Duration:") && info.dur == 0 {
			seg := strings.SplitN(ln, "Duration:", 2)[1]
			seg = strings.SplitN(seg, ",", 2)[0]
			var h, m int
			var sec float64
			if _, err := fmt.Sscanf(strings.TrimSpace(seg), "%d:%d:%f", &h, &m, &sec); err == nil {
				info.dur = float64(h*3600+m*60) + sec
			}
		}
	}
	if info.dur == 0 {
		return info, fmt.Errorf("无法读取片段时长：%s", filepath.Base(path))
	}
	return info, nil
}

// concatSimple 无音轨时的纯拼接（旧行为）
func concatSimple(inputs []string, outPath string) error {
	listPath := filepath.Join(os.TempDir(), fmt.Sprintf("flowedge-concat-%d.txt", time.Now().UnixNano()))
	var sb strings.Builder
	for _, p := range inputs {
		escaped := strings.ReplaceAll(filepath.ToSlash(p), "'", `'\''`)
		sb.WriteString(fmt.Sprintf("file '%s'\n", escaped))
	}
	if err := os.WriteFile(listPath, []byte(sb.String()), 0644); err != nil {
		return err
	}
	defer os.Remove(listPath)
	args := []string{"-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", listPath,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart", outPath}
	out, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("拼接失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// interpolateVideo 用 ffmpeg minterpolate 把视频帧率翻倍（CPU，不占显存）。
//
// 为什么不用 RIFE：权重镜像源 404 且依赖 cupy，装一套只为提平滑度不划算；
// minterpolate(mci+aobmc) 对慢速镜头（推镜/摇镜）效果足够，速度实测
// 10.4 秒视频 36 秒完成，全程 CPU。
func interpolateVideo(in, out string, fps int) error {
	args := []string{
		"-y", "-v", "error",
		"-i", in,
		"-vf", fmt.Sprintf("minterpolate=fps=%d:mi_mode=mci:mc_mode=aobmc:vsbmc=1", fps),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "copy",
		out,
	}
	cmdOut, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("插帧失败：%v %s", err, strings.TrimSpace(string(cmdOut)))
	}
	if st, serr := os.Stat(out); serr != nil || st.Size() < 4096 {
		return fmt.Errorf("插帧结果无效：%s", out)
	}
	return nil
}

// assembleShots 按每镜声明的衔接方式装配成片。
//
// 为什么不统一用 concat：分镜里「换机位」是硬切，「同一镜头延续」需要融一帧，
// 「时间跳跃」需要叠化。全部用同一种会毁掉节奏。
// 做法：所有边界都走 xfade，只是时长不同——
//
//	cut      d=0.04（1 帧，视觉上就是硬切）
//	continue d=0.08（融掉重复的那一帧）
//	dissolve d=0.30（真正的叠化）
//
// 视频与音频用同一个 d，保证音画同步；最后统一重编码 + 响度归一。
func assembleShots(clips []string, transitions []string, outPath string) error {
	if len(clips) == 0 {
		return fmt.Errorf("没有可装配的镜头")
	}
	if len(clips) == 1 {
		return copyFile(clips[0], outPath)
	}
	infos := make([]mediaInfo, len(clips))
	for i, c := range clips {
		info, err := probeMedia(c)
		if err != nil {
			return err
		}
		infos[i] = info
	}

	parts := make([]string, 0, len(clips)*2)
	for i := range clips {
		parts = append(parts, fmt.Sprintf("[%d:v]settb=AVTB,setpts=PTS-STARTPTS[v%d]", i, i))
		parts = append(parts, fmt.Sprintf("[%d:a]asetpts=PTS-STARTPTS[a%d]", i, i))
	}
	vcur, acur := "[v0]", "[a0]"
	total := infos[0].dur
	// 帧间隔（24fps≈0.0417s）。xfade 的叠加窗口必须能装下至少一帧，
	// 否则第二段一帧都不会输出——实测「画面只有第一镜、音频却是全长」就是这个原因。
	const frameInterval = 0.05
	for i := 1; i < len(clips); i++ {
		d := 0.08 // cut：2 帧融合，视觉上仍是硬切
		switch transitions[i-1] {
		case "dissolve":
			d = 0.30
		case "continue":
			d = 0.12
		}
		if d >= infos[i].dur-2*frameInterval {
			d = 0.08
		}
		// 留出 1 帧余量，保证 offset+d < 第一段时长，否则尾帧被吞
		offset := total - d - frameInterval
		if offset < 0 {
			offset = 0
		}
		vout := fmt.Sprintf("[vx%d]", i)
		aout := fmt.Sprintf("[ax%d]", i)
		parts = append(parts, fmt.Sprintf("%s[v%d]xfade=transition=fade:duration=%.3f:offset=%.3f%s",
			vcur, i, d, offset, vout))
		parts = append(parts, fmt.Sprintf("%s[a%d]acrossfade=d=%.3f:c1=tri:c2=tri%s",
			acur, i, d, aout))
		vcur, acur = vout, aout
		total += infos[i].dur - d
	}
	// 响度归一：不同镜头的生成音量差异明显，归一后成片才像一条。
	// 注意：acur 本身已是一个输出标签（如 [ax1]），后面直接接滤镜名；
	// 再写一个 [afin] 会被 ffmpeg 当成第二个输入 → "More input link labels ... 2 > 1"。
	parts = append(parts, fmt.Sprintf("%sloudnorm=I=-16:TP=-1.5:LRA=11[amix]", acur))

	args := []string{"-y", "-v", "error"}
	for _, c := range clips {
		args = append(args, "-i", c)
	}
	args = append(args,
		"-filter_complex", strings.Join(parts, ";"),
		"-map", vcur, "-map", "[amix]",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ar", "32000",
		"-movflags", "+faststart",
		outPath,
	)
	raw, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("装配失败：%v %s", err, strings.TrimSpace(string(raw)))
	}
	if st, serr := os.Stat(outPath); serr != nil || st.Size() < 4096 {
		return fmt.Errorf("装配结果无效：%s", outPath)
	}
	return nil
}

// duckAudio 原地把音量压到 20%，作为中置相消失败时的兜底。
//
// 为什么不到万不得已不用：它只是把整条音轨调小，语音仍然可辨
// （用户的原话就是「听不清但确实在说话」）。优先用 stripSpeechCenter。
//
// 为什么不直接静音：环境声和人声是混在一起的，静音会丢掉整个音景。
func duckAudio(path string) error {
	tmp := strings.TrimSuffix(path, ".mp4") + "-duck.mp4"
	args := []string{
		"-y", "-v", "error", "-i", path,
		"-af", "volume=0.20",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "128k",
		tmp,
	}
	if raw, err := exec.Command(ffmpegBin(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("压音失败：%v %s", err, strings.TrimSpace(string(raw)))
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() < 4096 {
		return fmt.Errorf("压音结果无效")
	}
	return os.Rename(tmp, path)
}

// stripSpeechCenter 中置相消：把「居中人声」从立体声里消掉，保留宽声场环境音。
//
// 依据（2026-10-19 实测）：H3 的乱语人声几乎都居中（等相位于左右声道），
// 而风声、雷电、碎石这类环境音是去相关的宽声场。所以 L−R 就能把台词抵消、
// 把环境留下来。实测一条被判为「枉年 赏我提用海鲜」的片段，相消后 ASR 转写为空，
// 平均响度只从 -14.1dB 掉到 -17.1dB（环境声仍在）。
//
// 为什么优于 duckAudio：压音量只是让人声变小，仍然"在说话"；相消是把它从信号里减掉。
// 为什么不用 UVR/Demucs 分轨：那要引入 onnxruntime + 模型（数百 MB），
// 而这条 ffmpeg 滤镜零依赖、纯 CPU、秒级完成，对一个 5 秒片段完全够用。
//
// 只处理音频（-c:v copy），不动视频帧，避免二次编码损伤画面。
func stripSpeechCenter(path string) error {
	tmp := strings.TrimSuffix(path, ".mp4") + "-center.mp4"
	args := []string{
		"-y", "-v", "error", "-i", path,
		"-af", "pan=stereo|c0=c0-c1|c1=c1-c0",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		tmp,
	}
	if raw, err := exec.Command(ffmpegBin(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("中置相消失败：%v %s", err, strings.TrimSpace(string(raw)))
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() < 4096 {
		os.Remove(tmp)
		return fmt.Errorf("中置相消结果无效")
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// audioHasSpeech 用本地 ASR 判断片段里是否有人在说话（CPU，几秒）。
// ASR 不可达时返回 (false, false) —— 调用方必须把它当作「没验」，不是「没人声」。
func audioHasSpeech(asr *ASRService, path string) (speech bool, checked bool) {
	if asr == nil {
		return false, false
	}
	text, err := asr.Transcribe(path)
	if err != nil {
		return false, false
	}
	return len([]rune(strings.TrimSpace(text))) >= 4, true
}

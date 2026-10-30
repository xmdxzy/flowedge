package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 音频后期：响度归一 + 可选 BGM 混音。
//
// 为什么这一步必须有（2026-10-25 实测）：装配出来的成片均值 -22.9 dB，
// 而 film 门里写的 loudness_db 是 [-22,-11]——成片踩在门限下沿，
// 换手机外放就听不清。响度不归一，前面所有画面功夫都折在最后一步。
//
// 做法上刻意保持「单遍 loudnorm + 只重编码音轨」：
//   - 视频流 -c:v copy，不产生代际损失
//   - 单遍 loudnorm 是动态归一，比两遍快一个数量级，损失在 ±1 LU 内（够用）
//   - 失败返回错误但**不改用原文件**：调用方决定是降级还是报错

// masterLoudness 把音轨整体归一到 targetLUFS（视频流原样 copy）。
func masterLoudness(in, out string, targetLUFS, truePeak, lra float64) error {
	info, err := inspectMedia(in)
	if err != nil {
		return err
	}
	if !info.HasAudio {
		return fmt.Errorf("响度归一失败：%s 没有音轨", filepath.Base(in))
	}
	if targetLUFS > 0 {
		targetLUFS = -16 // 传正数视为「没配」：LUFS 天然为负
	}
	if truePeak > 0 {
		truePeak = -1.5
	}
	if lra <= 0 {
		lra = 11
	}
	dur := info.VSeconds
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-map", "0:v:0?", "-map", "0:a:0",
		"-c:v", "copy",
		"-af", fmt.Sprintf("loudnorm=I=%.1f:TP=%.1f:LRA=%.0f,alimiter=limit=0.8:level=false", targetLUFS, truePeak, lra),
		"-c:a", "aac", "-b:a", "192k",
		// loudnorm 内部升到 192kHz 处理；不显式指定采样率就会产出 96kHz 的 AAC
		// （2026-10-25 真实成片验证时抓到：音轨变成 96000Hz，部分播放器不认）。
		"-ar", "48000",
	}
	if dur > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", dur))
	}
	args = append(args, out)
	if b, cerr := exec.Command(ffmpegBin(), args...).CombinedOutput(); cerr != nil {
		_ = os.Remove(out)
		return fmt.Errorf("响度归一失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
	}
	return nil
}

// mixBGM 混入一条背景音乐，并按需做 sidechain ducking（人声/环境声起时自动压 BGM）。
//
// 参数：
//   - gainDB：BGM 相对原音轨的电平（默认 -18dB，BGM 是底不是主角）
//   - duck：true 时用原音轨做旁链，BGM 在有人声/强环境声处自动让位
//
// 输出同样只重编码音轨；时长按视频流截断，保证不引入新的音画漂移。
func mixBGM(in, bgm, out string, gainDB float64, duck bool) error {
	info, err := inspectMedia(in)
	if err != nil {
		return err
	}
	if !info.HasAudio {
		return fmt.Errorf("BGM 混音失败：%s 没有音轨", filepath.Base(in))
	}
	if gainDB > 0 {
		// 0dB 是合法的“环境声床”档位；只有正增益才视为误填，压回 0。
		gainDB = 0
	}
	dur := info.VSeconds
	if dur <= 0 {
		return fmt.Errorf("BGM 混音失败：读不到视频时长")
	}
	// BGM 用 -stream_loop -1 补到片长；末段由 -t 截断。
	fc := fmt.Sprintf("[1:a]volume=%.1fdB,atrim=0:%.3f,asetpts=PTS-STARTPTS[bg];", gainDB, dur)
	if duck {
		fc += "[0:a]asplit=2[main][sc];" +
			"[bg][sc]sidechaincompress=threshold=0.05:ratio=8:attack=20:release=400[bgd];" +
			"[main][bgd]amix=inputs=2:duration=first:normalize=0,alimiter=limit=0.8:level=false[a]"
	} else {
		fc += "[0:a][bg]amix=inputs=2:duration=first:normalize=0,alimiter=limit=0.8:level=false[a]"
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-stream_loop", "-1", "-i", bgm,
		"-filter_complex", fc,
		"-map", "0:v:0", "-map", "[a]",
		"-c:v", "copy",
		"-c:a", "aac", "-b:a", "192k",
		"-ar", "48000",
		"-t", fmt.Sprintf("%.3f", dur),
		out,
	}
	if b, cerr := exec.Command(ffmpegBin(), args...).CombinedOutput(); cerr != nil {
		_ = os.Remove(out)
		return fmt.Errorf("BGM 混音失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
	}
	return nil
}

// roomToneGeneratorMeanDB brown 噪声（anoisesrc amplitude=1）的实测平均电平。
// 2026-10-30 本机 ffmpeg 实测 mean -13.7dB / max -1.6dB（3s 采样）。
// 「静音输入 + 声床 ≈ floorDB」的换算由 TestMixRoomTone_Level 钉住：
// ffmpeg 换版本导致生成器电平漂了，测试会红，而不是成片静音缺口悄悄回来。
const roomToneGeneratorMeanDB = -13.7

// mixRoomTone 在成片底下铺一层连续低电平噪声床（room tone）。
//
// 为什么必须铺底（2026-10-29 多镜实测）：TTS 每句前导静音已裁（dub_track 留
// 0.03–0.05s），接缝已有 acrossfade 0.2s，但镜内环境声时强时弱，窗口一叠加
// 仍出现 0.85s 可听静音（verify-media silence_gap，判据 -38dB / ≥0.45s）。
// 裁剪修不了「没有声音的时间段」，只能铺底。
//
// floorDB 是期望的底噪电平（dBFS，默认 -36：刚好盖过 -38dB 静音判据，又比
// -16 LUFS 的节目声低 20dB，听感是「房间里有底噪」而不是「多了噪音」）。
// 视频流原样 copy；输出与 mixBGM 同为 48k，避免后续链路再采样。
func mixRoomTone(in, out string, floorDB float64) error {
	if floorDB >= 0 {
		return fmt.Errorf("声床电平必须是负 dBFS（0 表示关闭，不该调到这里）：%v", floorDB)
	}
	info, err := inspectMedia(in)
	if err != nil {
		return err
	}
	if !info.HasAudio {
		return fmt.Errorf("声床铺底失败：%s 没有音轨", filepath.Base(in))
	}
	// 底噪电平 = 生成器电平 + volume 补偿；fixed seed 让同参数重跑结果可复现
	fc := fmt.Sprintf(
		"[0:a]aformat=sample_fmts=fltp:sample_rates=48000:channel_layouts=stereo[main];"+
			"[1:a]volume=%.1fdB,aformat=sample_fmts=fltp:sample_rates=48000:channel_layouts=stereo[bed];"+
			"[main][bed]amix=inputs=2:duration=first:normalize=0,alimiter=limit=0.8:level=false[a]",
		floorDB-roomToneGeneratorMeanDB)
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-f", "lavfi", "-i", "anoisesrc=color=brown:seed=2026:sample_rate=48000:amplitude=1",
		"-filter_complex", fc,
		"-map", "0:v:0?", "-map", "[a]",
		"-c:v", "copy",
		"-c:a", "aac", "-b:a", "192k",
		"-ar", "48000",
		out,
	}
	if b, cerr := exec.Command(ffmpegBin(), args...).CombinedOutput(); cerr != nil {
		_ = os.Remove(out)
		return fmt.Errorf("声床铺底失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
	}
	return nil
}

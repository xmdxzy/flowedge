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

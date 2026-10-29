package service

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 音画对齐：装配完的成片，音轨必须与视频流等长。
//
// 为什么这是「修根因」而不是「加验收」（2026-10-25 实测）：
// xfade 装配出的成片，视频流 11.79s、音轨 12.00s，容器时长取长的那个（12.00s），
// 于是容器看起来"正常"，而播放器在末尾有 0.21s 画面停住/音画错位。
// 之前的做法是在外面跑 verify-media.py 抓它——那是验收，不是修复：
// 每一条成片都得人记得跑一次，忘了就交付出去。
//
// 这里在装配后立刻按**视频流**时长重封一次音轨：
//   - apad 把短音轨补满（不能用 -shortest：音频短几毫秒就会砍掉视频尾帧）
//   - -t 按视频流精确时长截断
//   - 视频流 -c:v copy（零代际损失、零重编码成本）
//
// 复用的纪律与 replaceAudioTrack 一致（AGENTS §0.12 ⑤：mux 后立刻核 nb_frames）。
func alignAVToVideo(path string, tolerance float64) (driftBefore float64, fixed bool, err error) {
	if tolerance <= 0 {
		tolerance = 0.05
	}
	before, ierr := inspectMedia(path)
	if ierr != nil {
		return 0, false, ierr
	}
	if !before.HasAudio || before.VSeconds <= 0 || before.ASeconds <= 0 {
		return 0, false, nil // 无音轨 / 读不到时长：不猜，原样交回
	}
	driftBefore = math.Abs(before.VSeconds - before.ASeconds)
	if driftBefore <= tolerance {
		return driftBefore, false, nil
	}

	ext := filepath.Ext(path)
	out := strings.TrimSuffix(path, ext) + "-avsync" + ext
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-i", path,
		"-map", "0:v:0", "-map", "0:a:0",
		"-c:v", "copy",
		"-af", "apad",
		"-c:a", "aac", "-b:a", "192k",
		"-t", fmt.Sprintf("%.3f", before.VSeconds),
		out,
	)
	if b, cerr := cmd.CombinedOutput(); cerr != nil {
		_ = os.Remove(out)
		return driftBefore, false, fmt.Errorf("音画对齐重封失败：%w（%s）", cerr, strings.TrimSpace(string(b)))
	}

	after, aerr := inspectMedia(out)
	if aerr != nil {
		_ = os.Remove(out)
		return driftBefore, false, aerr
	}
	// 硬约束：对齐不许丢帧（-shortest 那类事故的防呆）
	if before.Frames > 0 && after.Frames > 0 && after.Frames < before.Frames {
		_ = os.Remove(out)
		return driftBefore, false, fmt.Errorf("音画对齐后帧数从 %d 掉到 %d（视频流被截断）", before.Frames, after.Frames)
	}
	driftAfter := math.Abs(after.VSeconds - after.ASeconds)
	if driftAfter > tolerance {
		_ = os.Remove(out)
		return driftBefore, false, fmt.Errorf("音画对齐后仍有 %.3fs 偏差（原 %.3fs），不改用坏件", driftAfter, driftBefore)
	}
	if err := os.Rename(out, path); err != nil {
		_ = os.Remove(out)
		return driftBefore, false, fmt.Errorf("音画对齐产物落地失败：%w", err)
	}
	return driftBefore, true, nil
}

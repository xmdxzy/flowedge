package service

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 回归：单图 + 配音必须产出「有画面也有声音」的 mp4。
//
// 旧实现只在多图时 -map [v]，单图却 -map N:a，ffmpeg 一旦看到显式 -map
// 就只输出被映射的流 → 视频流被静默丢弃，得到一个纯音频 mp4。
// 这条测试直接问 ffprobe 要流列表，不靠肉眼看文件大小。
func TestComposeVideoSingleImageKeepsVideoStream(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("未安装 ffprobe，跳过")
	}

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "in.png")
	wavPath := filepath.Join(dir, "in.wav")
	outPath := filepath.Join(dir, "out.mp4")

	writeTestPNG(t, imgPath)
	writeTestWAV(t, wavPath, 1.0)

	svc := NewVideoService()
	if err := svc.ComposeVideoMulti([]string{imgPath}, wavPath, outPath, 1); err != nil {
		t.Fatalf("合成失败：%v", err)
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "default=noprint_wrappers=1:nokey=1", outPath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe 失败：%v", err)
	}
	streams := string(out)
	if !strings.Contains(streams, "video") {
		t.Fatalf("单图合成丢失了视频流，实际流：%q", streams)
	}
	if !strings.Contains(streams, "audio") {
		t.Fatalf("音频流丢失，实际流：%q", streams)
	}
}

func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 120, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// writeTestWAV 写一个 16bit 单声道 8kHz 的正弦波（ffmpeg 能直接吃）
func writeTestWAV(t *testing.T, path string, seconds float64) {
	t.Helper()
	const sampleRate = 8000
	n := int(seconds * sampleRate)
	var buf bytes.Buffer
	dataLen := n * 2

	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for i := 0; i < n; i++ {
		v := int16(3000 * ((i % 40) - 20))
		binary.Write(&buf, binary.LittleEndian, v)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

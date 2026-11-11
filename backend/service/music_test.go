package service

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"flowedge/backend/model"
)

// TestParseAudioURL ACE-Step 的 result 是**被转义的 JSON 字符串**，
// 解析错就直接拿不到音频（表现为「生成完成但产物丢失」）。
func TestParseAudioURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "escaped-json-string（API.md §5.3 真实形状）",
			in:   `[{"file": "/v1/audio?path=%2Ftmp%2Fa.wav", "status": 1, "prompt": "x"}]`,
			want: "/v1/audio?path=%2Ftmp%2Fa.wav",
		},
		{
			name: "single-object",
			in:   `{"file": "/v1/audio?path=%2Ftmp%2Fb.wav"}`,
			want: "/v1/audio?path=%2Ftmp%2Fb.wav",
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
		{
			name:    "null",
			in:      "null",
			wantErr: true,
		},
		{
			name:    "no-file-field",
			in:      `[{"wave": "", "status": 0}]`,
			wantErr: true,
		},
		{
			name:    "garbage",
			in:      "<html>502</html>",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseAudioURL(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际拿到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got != c.want {
				t.Fatalf("地址不符：%q != %q", got, c.want)
			}
		})
	}
}

// TestMusicNodeSideIsOwnSide 音乐必须独占一侧：
// 并进 audio 会让「先配音再出 BGM」共用一侧、不触发换侧，24GB 卡直接 OOM。
func TestMusicNodeSideIsOwnSide(t *testing.T) {
	spec, ok := NodeSpecOf("music")
	if !ok {
		t.Fatal("music 节点没有登记 NodeSpecs")
	}
	if spec.Side != sideMusic {
		t.Fatalf("music 节点侧别应为 %q，实际 %q", sideMusic, spec.Side)
	}
	if spec.Side == sideAudio {
		t.Fatal("music 不能和 tts 共用 audio 侧（会跳过换侧卸载）")
	}
	if !spec.Media {
		t.Fatal("music 产出音频文件，Media 必须为 true（否则血缘与产物清单漏掉它）")
	}
	found := false
	for _, e := range NodeCatalog() {
		if e.Type == "music" {
			found = true
		}
	}
	if !found {
		t.Fatal("music 没有出现在 nodecatalog（前端调色板/检查器由 catalog 生成）")
	}
}

// TestMusicTimeoutCoversColdLoad 冷加载 3–5 分钟，固定 180s 会误杀。
func TestMusicTimeoutCoversColdLoad(t *testing.T) {
	n := &model.NodeInstance{}
	n.Data = map[string]interface{}{"duration": float64(30)}
	got := musicTimeout(n)
	if got < 5*time.Minute {
		t.Fatalf("超时 %v 覆盖不了冷加载（实测 3–5 分钟）", got)
	}
}

// TestMixAutoBGM_NoAudioTrack 无音轨成片（code_video / 静音片）必须能配上 BGM。
//
// mixBGM 要求输入有音轨（它拿原音轨做旁链），直接套会报「没有音轨」——
// 而「自动配乐」最需要用它的恰是无音轨的片。这里钉住退化分支：
// 动作 = 把 BGM 铺满全片，视频流 copy，时长不变。
func TestMixAutoBGM_NoAudioTrack(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	// 只造视频流、不带音轨
	silent := filepath.Join(dir, "silent.mp4")
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=24:duration=1.5",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", silent)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("合成无声素材失败：%v（%s）", err, string(b))
	}
	bgm := filepath.Join(dir, "bgm.wav")
	cmd = exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:sample_rate=44100:duration=2",
		"-c:a", "pcm_s16le", bgm)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("合成 BGM 失败：%v（%s）", err, string(b))
	}

	out := filepath.Join(dir, "out.mp4")
	if err := mixAutoBGM(silent, bgm, out, -18, true); err != nil {
		t.Fatalf("无音轨分支失败：%v", err)
	}
	info, err := inspectMedia(out)
	if err != nil {
		t.Fatalf("读产物失败：%v", err)
	}
	if !info.HasAudio {
		t.Fatal("混完仍然没有音轨：BGM 没进去")
	}
	if d, _ := mediaSeconds(out); d < 1.4 || d > 1.7 {
		t.Fatalf("时长被改动：%.2fs（期望 ~1.5s）", d)
	}
}

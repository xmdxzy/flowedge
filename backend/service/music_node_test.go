package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// fakeAceStep 用 httptest 复刻 ACE-Step 的三段式协议，用来钉住节点的
// 「提交 → 轮询 → 下载 → 落盘」这条链路——它不需要 GPU，也不需要真模型。
func fakeAceStep(t *testing.T, wavPath string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"ok"},"code":200,"error":null}`))
	})
	mux.HandleFunc("/release_task", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 纯音乐必须带 cot 关闭标记，否则会走 LM 的 CoT（实测 5 分钟+ 卡在 phase 1）
		if body["lyrics"] == "" {
			if body["use_cot_caption"] != false || body["use_cot_language"] != false {
				t.Errorf("纯音乐请求没有关掉 LM CoT：%v", body)
			}
		}
		_, _ = w.Write([]byte(`{"data":{"task_id":"t-1","status":"queued"},"code":200,"error":null}`))
	})
	mux.HandleFunc("/query_result", func(w http.ResponseWriter, _ *http.Request) {
		res, _ := json.Marshal([]map[string]interface{}{{
			"file": "/v1/audio?path=ignored", "status": 1,
		}})
		payload, _ := json.Marshal(map[string]interface{}{
			"data": []map[string]interface{}{
				{"task_id": "t-1", "status": 1, "result": string(res)},
			},
			"code": 200, "error": nil,
		})
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/v1/audio", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(wavPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(b)
	})
	return httptest.NewServer(mux)
}

// TestExecMusicNode_FullPath 节点必须产出真实可探测的音频文件。
func TestExecMusicNode_FullPath(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	wav := filepath.Join(dir, "fake.wav")
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=48000:duration=3",
		"-ac", "2", "-c:a", "pcm_s16le", wav)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造测试音频失败：%v（%s）", err, string(b))
	}

	srv := fakeAceStep(t, wav)
	defer srv.Close()

	e := &EngineV2{music: NewMusicService(srv.URL)}
	node := &model.NodeInstance{ID: "m-1", Type: "music", Data: map[string]interface{}{
		"prompt":   "warm lo-fi piano",
		"duration": float64(12),
	}}
	out, err := e.execMusicNode(node, nil)
	if err != nil {
		t.Fatalf("节点执行失败：%v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("输出类型不对：%T", out)
	}
	audio, _ := m["audio"].(string)
	if audio == "" {
		t.Fatalf("没有 audio 输出：%+v", m)
	}
	st, serr := os.Stat(audio)
	if serr != nil || st.Size() == 0 {
		t.Fatalf("产物不存在或为空：%v size=%d", serr, st.Size())
	}
	if d, _ := m["duration"].(float64); d <= 0 {
		t.Fatalf("时长没有探测出来：%+v", m)
	}
	if model, _ := m["model"].(string); model != defaultMusicModel {
		t.Fatalf("默认模型不对：%q（期望 %q）", model, defaultMusicModel)
	}
}

// TestExecMusicNode_NoPromptFailsFast 没提示词时不许静默产出。
func TestExecMusicNode_NoPromptFailsFast(t *testing.T) {
	e := &EngineV2{music: NewMusicService("http://127.0.0.1:1")}
	node := &model.NodeInstance{ID: "m-2", Type: "music", Data: map[string]interface{}{}}
	_, err := e.execMusicNode(node, nil)
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("期望 prompt 缺失的显式错误，实际 %v", err)
	}
}

// TestApplyAutoBGM_SkipsWhenFileGiven 显式给了 bgm 文件时不许再自动生成。
func TestApplyAutoBGM_SkipsWhenFileGiven(t *testing.T) {
	e := &EngineV2{music: NewMusicService("http://127.0.0.1:1")}
	states := []model.NodeInstance{}
	params := map[string]interface{}{"bgm_auto": true, "bgm": `C:\some\bgm.mp3`}
	got, err := e.ApplyAutoBGM(states, params)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got != "" {
		t.Fatalf("不该生成，返回了 %q", got)
	}
}

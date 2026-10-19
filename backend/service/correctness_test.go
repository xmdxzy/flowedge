package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ===== 未配置服务必须显式失败，严禁伪造成功（P0 核心）=====

func TestASR_NotConfiguredFails(t *testing.T) {
	s := NewASRService("")
	text, err := s.Transcribe("whatever.wav")

	if err == nil {
		t.Fatal("ASR 未配置时必须返回错误，不能伪造转写结果")
	}
	if text != "" {
		t.Fatalf("ASR 未配置时不得返回文本，got %q", text)
	}
	if !errors.Is(err, ErrServiceNotConfigured) {
		t.Fatalf("错误必须可被 ErrServiceNotConfigured 判定，got %v", err)
	}
	if strings.Contains(err.Error(), "transcribed from") {
		t.Fatalf("不得出现伪造文本残留：%v", err)
	}
	if classifyError(err) != "config_missing" {
		t.Fatalf("错误码应为 config_missing，got %s", classifyError(err))
	}
}

// TestTTS_FallbackNeverFakesAudio 未配置服务时：能用 SAPI 就产出真音频，不能就显式失败。
// 两种结果都不允许出现「假音频」——这是这条测试守的不变量。
func TestTTS_FallbackNeverFakesAudio(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.wav")

	s := NewTTSService("")
	backend, err := s.SynthesizeWith("hello", out, TTSOptions{})
	t.Logf("SAPI 可用=%v backend=%q err=%v", SAPIAvailable(), backend, err)

	if SAPIAvailable() {
		if err != nil {
			t.Fatalf("SAPI 可用时应成功合成，got %v", err)
		}
		if backend != "sapi" {
			t.Fatalf("后端应为 sapi，got %s", backend)
		}
		st, statErr := os.Stat(out)
		if statErr != nil || st.Size() < 1000 {
			t.Fatalf("SAPI 应产出真实音频文件（>1KB），statErr=%v", statErr)
		}
		return
	}

	if err == nil {
		t.Fatal("TTS 完全不可用时必须返回错误，不能写假音频文件")
	}
	if !errors.Is(err, ErrServiceNotConfigured) {
		t.Fatalf("错误必须可被 ErrServiceNotConfigured 判定，got %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		data, _ := os.ReadFile(out)
		if strings.Contains(string(data), "fake audio") {
			t.Fatal("不得写出 fake audio data")
		}
		t.Fatal("不可用时不得创建输出文件")
	}
	if classifyError(err) != "config_missing" {
		t.Fatalf("错误码应为 config_missing，got %s", classifyError(err))
	}
}

// ===== 向量库 ID 不得碰撞（旧实现 3380 个取值后会误删）=====

func TestGenerateIDUnique(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ {
		id := generateID(i)
		if seen[id] {
			t.Fatalf("ID 碰撞：第 %d 次生成了重复的 %q", i, id)
		}
		seen[id] = true
	}
}

func TestVectorStore_DeleteOnlyTarget(t *testing.T) {
	vs := NewVectorStore(t.TempDir())

	id1, err := vs.Add(VectorRecord{Content: "target"})
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if _, err := vs.Add(VectorRecord{Content: "other"}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if _, err := vs.Add(VectorRecord{Content: "another"}); err != nil {
		t.Fatalf("add failed: %v", err)
	}

	if err := vs.Delete(id1); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if vs.Count() != 2 {
		t.Fatalf("删除后应剩 2 条，got %d", vs.Count())
	}
	for _, r := range vs.GetAll() {
		if r.Content == "target" {
			t.Fatal("误删了非目标记录")
		}
	}
}

// ===== 节点级超时可配置 =====

func TestNodeTimeoutOverride(t *testing.T) {
	nodes := []byte(`[{"id":"a","type":"start","data":{"prompt":"x"}},{"id":"b","type":"data_transform","data":{"template":"{{input}}","timeout_ms":1}}]`)
	edges := []byte(`[{"id":"e","source":"a","target":"b"}]`)

	eng := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	out, err := eng.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("data_transform 不应因超时而失败：%v", err)
	}
	_ = out
	// 1ms 超时的存在意义：验证 timeout_ms 被读取且不破坏正常路径。
	// 真正触发超时的用例依赖外部服务，此处只保证解析不报错。
}

// ===== 节点错误码 =====

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrServiceNotConfigured, "config_missing"},
		{&nodeExecError{Code: "timeout", Err: fmt.Errorf("boom")}, "timeout"},
		{fmt.Errorf("plain failure"), "execution_error"},
		{fmt.Errorf("wrapped: %w", ErrServiceNotConfigured), "config_missing"},
	}
	for i, c := range cases {
		if got := classifyError(c.err); got != c.want {
			t.Fatalf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

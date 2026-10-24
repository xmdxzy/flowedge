package service

import (
	"os"
	"path/filepath"
	"testing"
)

// Reload 必须让「改了 deps.json 的面板字段」立刻生效。
//
// 反面案例：2026-10-23 把 tts.autostart 改成 false 后，`/api/deps` 还是报 true，
// 因为 autostart 只在 NewSupervisor 时读一次。当时只能靠重启引擎兜回来——
// 而重启引擎在这台机器上要停服务、等就绪，代价远大于改一行配置。
func TestSupervisorReloadPicksUpAutostartChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(autostart bool) {
		body := `{"tts":{"script":"tts_audio8_pt_server.py","python":"C:/fake/python.exe",` +
			`"port":"8103","autostart":` +
			map[bool]string{true: "true", false: "false"}[autostart] + `,"idle_unload":600}}`
		if err := os.WriteFile(filepath.Join(dir, "data", "deps.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeCfg(true)
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd) //nolint:errcheck

	sup := NewSupervisor("http://127.0.0.1:8188", "http://127.0.0.1:8101", "http://127.0.0.1:8103")
	if spec, ok := sup.Spec("tts"); !ok || !spec.Autostart {
		t.Fatalf("初始 autostart 应为 true，得到 %+v", spec)
	}

	writeCfg(false)
	changes := sup.Reload()

	spec, ok := sup.Spec("tts")
	if !ok {
		t.Fatal("tts 规格丢了")
	}
	if spec.Autostart {
		t.Fatalf("Reload 后 autostart 仍为 true，配置没生效")
	}
	if _, changed := changes["tts.autostart"]; !changed {
		t.Errorf("变更清单里没有 tts.autostart，changes=%v", changes)
	}
	if spec.Binary == "" || len(spec.Args) == 0 {
		// 只报字段，别整份 spec（含完整 Env，打出来几百行没法看）
		t.Errorf("Reload 不该把启动方式清空：binary=%q args=%v", spec.Binary, spec.Args)
	}
}

// 配置没变时 Reload 应返回空清单（面板不该无端刷出「有变更」）。
func TestSupervisorReloadNoChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"tts":{"script":"tts_audio8_pt_server.py","port":"8103","autostart":false}}`
	if err := os.WriteFile(filepath.Join(dir, "data", "deps.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd) //nolint:errcheck

	sup := NewSupervisor("http://127.0.0.1:8188", "http://127.0.0.1:8101", "http://127.0.0.1:8103")
	changes := sup.Reload()
	for k, v := range changes {
		t.Errorf("配置未改却报告变更 %s: %s", k, v)
	}
}

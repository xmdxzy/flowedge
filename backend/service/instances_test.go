package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInstanceRegistry_DefaultsMatchLegacy 默认路由必须与改造前一致：
// h3* → 8189，其余 → 8188。不一致等于升级后悄悄换实例。
func TestInstanceRegistry_DefaultsMatchLegacy(t *testing.T) {
	r := LoadInstanceRegistry("", "http://127.0.0.1:8189")
	cases := []struct{ model, want string }{
		{"minimax_h3_fl2va_pruned_int8_convrot.safetensors", "http://127.0.0.1:8189"},
		{"minimax_h3_ref2va_pruned_int8_convrot.safetensors", "http://127.0.0.1:8189"},
		{"qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors", "http://127.0.0.1:8189"},
		{"flux1-schnell-fp8.safetensors", "http://127.0.0.1:8188"},
		{"wan2.1_t2v_1.3B_fp16.safetensors", "http://127.0.0.1:8188"},
		{"RealESRGAN_x4plus.pth", "http://127.0.0.1:8188"},
		{"some_unknown_model.safetensors", "http://127.0.0.1:8188"}, // 未知 → 默认实例
	}
	for _, c := range cases {
		if got := r.ResolveForModel(c.model).BaseURL; got != c.want {
			t.Errorf("route %s = %q, want %q", c.model, got, c.want)
		}
	}
}

// TestInstanceRegistry_PrefixWithoutWildcard 用户不会去记通配符语法，
// 不带 * 的模板按前缀匹配。
func TestInstanceRegistry_PrefixWithoutWildcard(t *testing.T) {
	if !matchModel("minimax_h3", "minimax_h3_fl2va.safetensors") {
		t.Error("bare pattern should match as prefix")
	}
	if !matchModel("flux*", "models/flux1-schnell-fp8.safetensors") {
		t.Error("wildcard should match basename")
	}
	if matchModel("flux*", "wan2.1.safetensors") {
		t.Error("must not match unrelated model")
	}
	if matchModel("", "anything") {
		t.Error("empty pattern must not match")
	}
}

// TestInstanceRegistry_ExclusiveGroupSerializes 同组实例必须串行。
// 这是「单卡不能同时驻留两套环境」的机器化表达。
func TestInstanceRegistry_ExclusiveGroupSerializes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "instances.json")
	cfg := `{"default":"a","instances":[
		{"name":"a","base_url":"http://127.0.0.1:8188","exclusive_group":"gpu","enabled":true},
		{"name":"b","base_url":"http://127.0.0.1:8189","exclusive_group":"gpu","enabled":true}
	]}`
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadInstanceRegistry(p, "")

	unlockA := r.LockForBase("http://127.0.0.1:8188")
	// 不用 defer：下面要手动解锁来观察 b 是否被放行，double unlock 会直接 panic

	acquired := make(chan struct{})
	go func() {
		u := r.LockForBase("http://127.0.0.1:8189")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("instance b must wait while a holds the same exclusive group")
	case <-time.After(150 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("instance b should proceed after a releases")
	}
}

// TestInstanceRegistry_NoGroupNoBlocking 不在互斥组的实例互不影响
func TestInstanceRegistry_NoGroupNoBlocking(t *testing.T) {
	r := LoadInstanceRegistry("", "")
	unlock := r.LockForBase("http://127.0.0.1:8188")
	defer unlock()
	done := make(chan struct{})
	go func() {
		u := r.LockForBase("http://127.0.0.1:8188")
		u()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no exclusive group means no blocking")
	}
}

// TestInstanceRegistry_ConcurrentLocksAreSafe 并发加锁不能互相踩
func TestInstanceRegistry_ConcurrentLocksAreSafe(t *testing.T) {
	r := LoadInstanceRegistry("", "")
	var active, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := r.LockForBase("http://127.0.0.1:8188")
			cur := atomic.AddInt32(&active, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			u()
		}()
	}
	wg.Wait()
	if peak > 8 {
		t.Errorf("unexpected concurrency: %d", peak)
	}
}

// TestInstanceRegistry_ReplaceValidation 配置错误必须显式报出来
func TestInstanceRegistry_ReplaceValidation(t *testing.T) {
	r := LoadInstanceRegistry("", "")
	cases := [][]Instance{
		{{Name: "", BaseURL: "http://x"}},
		{{Name: "a", BaseURL: ""}},
		{{Name: "a", BaseURL: "http://x"}, {Name: "a", BaseURL: "http://y"}},
	}
	for i, items := range cases {
		if err := r.Replace(items); err == nil {
			t.Errorf("case %d should fail validation", i)
		}
	}
	if err := r.Replace([]Instance{{Name: "a", BaseURL: "http://x", Enabled: true}}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

// TestInstanceRegistry_SaveLoad 配置落盘后能读回
func TestInstanceRegistry_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "instances.json")
	r := LoadInstanceRegistry(p, "http://127.0.0.1:8189") // 首次会落默认
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("default config should be written: %v", err)
	}
	if err := r.Replace([]Instance{{Name: "only", BaseURL: "http://127.0.0.1:9999", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	r.SetDefault("only")
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	again := LoadInstanceRegistry(p, "")
	if len(again.All()) != 1 || again.All()[0].Name != "only" {
		t.Errorf("round trip failed: %+v", again.All())
	}
	if again.DefaultInstance().BaseURL != "http://127.0.0.1:9999" {
		t.Errorf("default not restored: %+v", again.DefaultInstance())
	}
}

// TestBaseForKind_NilRegistryMatchesLegacy 没有 registry 时行为必须与改造前完全一致
func TestBaseForKind_NilRegistryMatchesLegacy(t *testing.T) {
	s := &Text2ImageService{BaseURL: "http://main:8188", H3BaseURL: "http://h3:8189"}
	if got := s.baseForKind("h3"); got != "http://h3:8189" {
		t.Errorf("h3 base = %q", got)
	}
	if got := s.baseForKind("h3_r2v"); got != "http://h3:8189" {
		t.Errorf("h3_r2v base = %q", got)
	}
	if got := s.baseForKind("t2v"); got != "http://main:8188" {
		t.Errorf("t2v base = %q", got)
	}
	if got := s.baseForKind("img2img"); got != "http://main:8188" {
		t.Errorf("img2img base = %q", got)
	}
}

// TestBaseForKind_RoutesByActualModel 有 registry 时按真实权重名路由，
// 而不是按节点类型硬编码。
func TestBaseForKind_RoutesByActualModel(t *testing.T) {
	store := LoadBundleStore("")
	store.Merge(RoleVideoH3Fl2va, map[string]string{"unet": "minimax_h3_custom.safetensors"}, nil)
	s := &Text2ImageService{
		BaseURL: "http://main:8188", H3BaseURL: "http://unused:1",
		Bundles: store, Instances: LoadInstanceRegistry("", "http://127.0.0.1:8189"),
	}
	if got := s.baseForKind("h3"); got != "http://127.0.0.1:8189" {
		t.Errorf("h3 should route to 8189 by model prefix, got %q", got)
	}
	// 把 H3 权重改名成主实例模式，路由应跟着变
	store.Merge(RoleVideoH3Fl2va, map[string]string{"unet": "flux_special.safetensors"}, nil)
	if got := s.baseForKind("h3"); got != "http://127.0.0.1:8188" {
		t.Errorf("routing must follow the model name, got %q", got)
	}
}

// TestInstanceRegistry_BrokenFileKeepsDefaults 坏配置不能让引擎失去实例
func TestInstanceRegistry_BrokenFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "instances.json")
	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadInstanceRegistry(p, "http://127.0.0.1:8189")
	if !strings.Contains(r.LoadNote(), "解析失败") {
		t.Errorf("broken config must be reported: %q", r.LoadNote())
	}
	if len(r.All()) == 0 {
		t.Error("must fall back to builtin instances")
	}
}

package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 同样的输入必须得到同样的键——否则"重跑复用"永远命中不了。
func TestShotCacheKey_Stable(t *testing.T) {
	a := shotCacheKey("prompt-A", 608, 352, 56, 12, "h3", true, false)
	b := shotCacheKey("prompt-A", 608, 352, 56, 12, "h3", true, false)
	if a != b {
		t.Fatalf("同输入得到不同键：%s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("键长度 %d，want 16", len(a))
	}
}

// 任何一个影响画面的参数变了，都不能复用（否则会静默交出错误的镜头）。
func TestShotCacheKey_ChangesWithParams(t *testing.T) {
	base := shotCacheKey("p", 608, 352, 56, 12, "h3", true, false)
	cases := map[string]string{
		"prompt": shotCacheKey("p2", 608, 352, 56, 12, "h3", true, false),
		"width":  shotCacheKey("p", 768, 352, 56, 12, "h3", true, false),
		"height": shotCacheKey("p", 608, 448, 56, 12, "h3", true, false),
		"frames": shotCacheKey("p", 608, 352, 124, 12, "h3", true, false),
		"steps":  shotCacheKey("p", 608, 352, 56, 20, "h3", true, false),
		"turbo":  shotCacheKey("p", 608, 352, 56, 12, "h3", false, false),
		"pdd":    shotCacheKey("p", 608, 352, 56, 12, "h3", true, true),
		"backnd": shotCacheKey("p", 608, 352, 56, 12, "wan", true, false),
	}
	for name, got := range cases {
		if got == base {
			t.Fatalf("%s 变了但键没变，会错误复用旧镜头", name)
		}
	}
}

// 命中要求文件真实存在且像个视频；空文件必须当没命中。
func TestShotCacheHit(t *testing.T) {
	dir := t.TempDir()
	key := "abcdef0123456789"
	if got := shotCacheHit(dir, key); got != "" {
		t.Fatalf("没有缓存却命中：%s", got)
	}
	path := filepath.Join(dir, key+".mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := shotCacheHit(dir, key); got != "" {
		t.Fatalf("1 字节的坏产物不该命中：%s", got)
	}
	big := make([]byte, 4096)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := shotCacheHit(dir, key); got != path {
		t.Fatalf("命中路径 = %q，want %q", got, path)
	}
}

// 存入后能命中；同名键不覆盖（内容等价）。
func TestShotCacheStoreThenHit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(src, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "cache")
	shotCacheStore(store, "k1", src)
	if got := shotCacheHit(store, "k1"); got == "" {
		t.Fatal("存进去之后应该能命中")
	}
	// 源文件太小（坏产物）时不缓存
	bad := filepath.Join(dir, "bad.mp4")
	_ = os.WriteFile(bad, []byte("x"), 0o644)
	shotCacheStore(store, "k2", bad)
	if got := shotCacheHit(store, "k2"); got != "" {
		t.Fatalf("坏产物被缓存了：%s", got)
	}
}

// 缓存目录不能无限长：超过上限时删最旧的。
func TestPruneShotCache(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.mp4")
	older := filepath.Join(dir, "older.mp4")
	newer := filepath.Join(dir, "newer.mp4")
	for _, p := range []string{older, old, newer} {
		if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	_ = os.Chtimes(older, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	_ = os.Chtimes(old, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	_ = os.Chtimes(newer, now, now)

	pruneShotCache(dir, 2)
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("最旧的应该被删掉")
	}
	for _, p := range []string{old, newer} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("最近的 %s 不该被删：%v", filepath.Base(p), err)
		}
	}
}

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 镜头级复用缓存：**同一镜的提示词与参数没变，就不再烧一次 GPU**。
//
// 由来（2026-10-26 审计）：报错文案一直写着「已完成镜已落盘，重跑会复用」，
// 但代码里 `reusedCount` 只初始化、从来没查过任何存储；DB 的 `shot_jobs` 表
// 建好了（prompt_hash / seed / elapsed_ms / vram_peak_mb）却一行没写过。
// 实测一晚 6 次失败，每次重跑都从第 1 镜重来：已经合格的镜头被反复重烧。
//
// 键 = 提示词 + 宽高帧步 + 后端 + turbo。**任何一项变了都不复用**——
// 宁可重烧，也不能交一条和分镜表不符的画面。
// 台词段不缓存：它的画面取决于当次 TTS 音轨，而提示词里并不包含音频内容，
// 同一个提示词配不同念稿，复用会得到口型对不上的片段。
const (
	shotCacheMaxFiles = 200
	shotCacheVersion  = "v1"
)

// shotCacheKey 计算一镜的复用键（16 位十六进制）。
func shotCacheKey(prompt string, width, height, frames, steps int, backend string, turbo, pdd bool) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00%d\x00%s\x00%t\x00%t\x00%s",
		prompt, width, height, frames, steps, backend, turbo, pdd, shotCacheVersion)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// shotCacheDir 缓存目录（跟着产物目录走，跨 run 稳定）。
func shotCacheDir(outDir string) string {
	if outDir == "" {
		outDir = "data/generated"
	}
	return filepath.Join(outDir, "shot-cache")
}

// shotCacheHit 命中返回缓存文件路径，否则空串。
// 小于 1KB 的文件视为坏产物——空文件比"没缓存"更危险（会静默交一条没画面的片子）。
func shotCacheHit(dir, key string) string {
	if dir == "" || key == "" {
		return ""
	}
	path := filepath.Join(dir, key+".mp4")
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() < 1024 {
		return ""
	}
	return path
}

// shotCacheStore 把一镜产物放进缓存。失败只记日志，绝不影响出片主流程。
func shotCacheStore(dir, key, src string) {
	if dir == "" || key == "" || src == "" {
		return
	}
	st, err := os.Stat(src)
	if err != nil || st.IsDir() || st.Size() < 1024 {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[shotcache] 建缓存目录失败：%v", err)
		return
	}
	dst := filepath.Join(dir, key+".mp4")
	if _, err := os.Stat(dst); err == nil {
		return // 同键已存在：内容等价，不覆盖
	}
	if err := copyFile(src, dst); err != nil {
		log.Printf("[shotcache] 写缓存失败：%v", err)
		return
	}
	log.Printf("[shotcache] 已缓存一镜产物（重跑同一分镜会直接复用）：%s", filepath.Base(dst))
	pruneShotCache(dir, shotCacheMaxFiles)
}

// pruneShotCache 只保留最近的 keep 个文件，避免缓存目录无限长。
// 按修改时间排序而不是按名字：键是哈希，名字排序没有时间含义。
func pruneShotCache(dir string, keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= keep {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".mp4" {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	if len(items) <= keep {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for _, it := range items[keep:] {
		if rerr := os.Remove(it.path); rerr == nil {
			log.Printf("[shotcache] 清理旧缓存：%s", filepath.Base(it.path))
		}
	}
}

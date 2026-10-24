package service

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 模型资产清点
//
// 目标：一眼看清「本机有哪些模型、多大、在哪」。
// 权重统一放在一个根目录（默认 D:/AI/Models），引擎只记根路径，
// 不把几十 GB 塞进项目目录，也不在各处硬编码某个工具的私有目录。

// ModelFile 单个权重文件
type ModelFile struct {
	Name   string `json:"name"`
	SizeMB int64  `json:"size_mb"`
	Path   string `json:"path"`
}

// ModelFamily 一类模型（文生图 / 视频 / 语音 / 语言 …）
type ModelFamily struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Root   string      `json:"root"`
	Count  int         `json:"count"`
	SizeMB int64       `json:"size_mb"`
	Files  []ModelFile `json:"files"`
	Note   string      `json:"note,omitempty"`
}

// ModelInventory 全量清点结果
type ModelInventory struct {
	Root     string        `json:"root"`
	TotalGB  float64       `json:"total_gb"`
	Families []ModelFamily `json:"families"`
	Missing  []string      `json:"missing,omitempty"`
}

// ModelsRoot 解析模型根目录：环境变量 > D:/AI/Models > 项目内 models/
func ModelsRoot(projectDir string) string {
	if v := strings.TrimSpace(os.Getenv("FLOWEDGE_MODELS_DIR")); v != "" {
		return filepath.ToSlash(v)
	}
	candidates := []string{"D:/AI/Models"}
	if projectDir != "" {
		candidates = append(candidates, filepath.ToSlash(filepath.Join(projectDir, "models")))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "D:/AI/Models"
}

// ScanModels 扫描模型根，按家族汇总
func ScanModels(root string) ModelInventory {
	inv := ModelInventory{Root: root}

	addFamily := func(key, label, rel, note string, exts []string) {
		dir := filepath.Join(root, rel)
		fam := ModelFamily{Key: key, Label: label, Root: dir, Note: note}
		entries, err := os.ReadDir(dir)
		if err != nil {
			inv.Missing = append(inv.Missing, key)
			inv.Families = append(inv.Families, fam)
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if len(exts) > 0 && !hasAnyExt(e.Name(), exts) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() < 1<<20 {
				continue
			}
			fam.Files = append(fam.Files, ModelFile{
				Name:   e.Name(),
				SizeMB: info.Size() >> 20,
				Path:   filepath.ToSlash(filepath.Join(dir, e.Name())),
			})
			fam.SizeMB += info.Size() >> 20
		}
		sort.Slice(fam.Files, func(i, j int) bool { return fam.Files[i].SizeMB > fam.Files[j].SizeMB })
		fam.Count = len(fam.Files)
		inv.TotalGB += float64(fam.SizeMB) / 1024
		inv.Families = append(inv.Families, fam)
	}

	weights := []string{".safetensors", ".ckpt", ".pt", ".pth", ".bin", ".gguf", ".onnx"}

	// 递归版本：ASR/TTS 的权重藏在 huggingface 缓存子目录里（models--X/snapshots/...），
	// 只扫顶层文件会漏掉它们，面板上就显示 0 个。
	addFamilyRec := func(key, label, rel, note string, exts []string) {
		dir := filepath.Join(root, rel)
		fam := ModelFamily{Key: key, Label: label, Root: dir, Note: note}
		if _, err := os.Stat(dir); err != nil {
			inv.Missing = append(inv.Missing, key)
			inv.Families = append(inv.Families, fam)
			return
		}
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			// 限制层级，避免把无关的大目录扫进来
			if relPath, rerr := filepath.Rel(dir, path); rerr == nil {
				if strings.Count(relPath, string(os.PathSeparator)) > 6 {
					return nil
				}
			}
			if len(exts) > 0 && !hasAnyExt(d.Name(), exts) {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil || info.Size() < 20<<20 {
				return nil
			}
			fam.Files = append(fam.Files, ModelFile{
				Name:   filepath.Base(path),
				SizeMB: info.Size() >> 20,
				Path:   filepath.ToSlash(path),
			})
			fam.SizeMB += info.Size() >> 20
			return nil
		})
		sort.Slice(fam.Files, func(i, j int) bool { return fam.Files[i].SizeMB > fam.Files[j].SizeMB })
		fam.Count = len(fam.Files)
		inv.TotalGB += float64(fam.SizeMB) / 1024
		inv.Families = append(inv.Families, fam)
	}

	addFamily("checkpoints", "文生图整包（checkpoints）", "comfyui/checkpoints", "ComfyUI 直接加载的单文件模型", weights)
	addFamily("diffusion_models", "扩散主干（UNET / DiT）", "comfyui/diffusion_models", "Qwen-Image、MiniMax-H3 等", weights)
	addFamily("text_encoders", "文本/视觉编码器", "comfyui/text_encoders", "T5、Qwen3-VL 等", weights)
	addFamily("vae", "VAE（编解码）", "comfyui/vae", "图像/视频/音频 VAE", weights)
	addFamily("loras", "LoRA（微调）", "comfyui/loras", "加速 / 风格 / 角色 LoRA", weights)
	addFamily("upscale", "超分模型", "comfyui/upscale_models", "RealESRGAN 等", weights)
	// HuggingFace 缓存是按仓库组织的（models--Org--Name/snapshots/.../blobs/<sha>），
	// blob 没有扩展名，用扩展名过滤会一个都统计不到。这里按仓库聚合。
	addHFCache := func(key, label, rel, note string) {
		dir := filepath.Join(root, rel)
		fam := ModelFamily{Key: key, Label: label, Root: dir, Note: note}
		entries, err := os.ReadDir(dir)
		if err != nil {
			inv.Missing = append(inv.Missing, key)
			inv.Families = append(inv.Families, fam)
			return
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
				continue
			}
			repoDir := filepath.Join(dir, e.Name())
			var size int64
			_ = filepath.WalkDir(repoDir, func(p string, d os.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return nil
				}
				if info, ierr := d.Info(); ierr == nil {
					size += info.Size()
				}
				return nil
			})
			if size < 20<<20 {
				continue
			}
			repo := strings.TrimPrefix(e.Name(), "models--")
			repo = strings.ReplaceAll(repo, "--", "/")
			fam.Files = append(fam.Files, ModelFile{
				Name:   repo,
				SizeMB: size >> 20,
				Path:   filepath.ToSlash(repoDir),
			})
			fam.SizeMB += size >> 20
		}
		sort.Slice(fam.Files, func(i, j int) bool { return fam.Files[i].SizeMB > fam.Files[j].SizeMB })
		fam.Count = len(fam.Files)
		inv.TotalGB += float64(fam.SizeMB) / 1024
		inv.Families = append(inv.Families, fam)
	}

	addHFCache("asr", "语音识别（ASR）", "asr", "HuggingFace 仓库（faster-whisper）")
	addHFCache("tts", "语音合成（TTS）", "tts/hub", "HuggingFace 仓库（Kokoro）")
	addHFCache("vision", "视觉理解（VLM）", "vision", "HuggingFace 仓库")
	_ = addFamilyRec

	sort.SliceStable(inv.Families, func(i, j int) bool { return inv.Families[i].SizeMB > inv.Families[j].SizeMB })
	return inv
}

func hasAnyExt(name string, exts []string) bool {
	lower := strings.ToLower(name)
	for _, e := range exts {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}

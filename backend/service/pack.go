package service

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 导演包（.fepack.zip）：一张图 + 它引用的所有本地素材，打成一个可交接的文件。
//
// 为什么需要：现在一条片子要换机器/交接/复现，能拿到的只有 workflows 表里一坨
// JSON，素材散在磁盘各处的绝对路径上。换台机器整张图就全是死路径。
//
// 两条硬约束（来自 AIMixer 导演包踩过的坑）：
//  1. **槽位显式记在清单里**（JSON Pointer），不能靠数组顺序。公共素材与分镜素材
//     合并时按下标对齐必然串位，而且串位不会报错，只会让第 2 镜长出第 1 镜的脸。
//  2. **包内路径只用 ASCII**。中文/空格/盘符在跨机器解压时会变乱码，
//     而乱码路径在 Windows 上大概率是"文件在但打不开"。
const (
	PackFormat        = "flowedge-pack"
	PackFormatVersion = 1

	packManifestName = "pack.json"

	// 上限照它那套来（16GB/8000 条太重，这里按出片素材的实际体量收一档）。
	packMaxAssets   = 500
	packMaxTotal    = 4 << 30 // 4 GiB
	packMaxPerFile  = 1 << 30 // 1 GiB
	packAssetPrefix = "assets/"
)

// PackAsset 一条素材在包里的位置，以及它在图里被哪一段 JSON 引用。
type PackAsset struct {
	Pointer string `json:"pointer"` // RFC6901，如 /3/data/ref_images/0
	File    string `json:"file"`    // assets/001_ref.png（包内相对路径，全 ASCII）
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"` // 导入时校验，防止包被截断/篡改后静默换脸
}

// PackManifest 包的清单。nodes/edges 里指向素材的值已经换成包内相对路径，
// 所以"打开 pack.json 就能看懂包里有什么"，不依赖导出时的绝对路径。
type PackManifest struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	Name          string          `json:"name"`
	CreatedAt     string          `json:"created_at"`
	SchemaVersion int             `json:"schema_version"`
	Nodes         json.RawMessage `json:"nodes"`
	Edges         json.RawMessage `json:"edges"`
	Assets        []PackAsset     `json:"assets"`
}

// ExportPack 把一张图连同它引用的本地素材打成 zip。
//
// 返回的清单里，素材路径已经换成包内相对路径；图本身（nodes/edges）也一并改写，
// 所以导出不会动到数据库里的原始图。
func ExportPack(name string, nodes, edges json.RawMessage, outPath string) (*PackManifest, error) {
	var tree interface{}
	if len(nodes) > 0 {
		if err := json.Unmarshal(nodes, &tree); err != nil {
			return nil, fmt.Errorf("节点 JSON 解析失败：%w", err)
		}
	}

	refs := collectFileRefs(tree)
	if len(refs) > packMaxAssets {
		return nil, fmt.Errorf("素材太多（%d 个，上限 %d）：请先清理图里不再使用的引用", len(refs), packMaxAssets)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("建包失败：%w", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	manifest := &PackManifest{
		Format: PackFormat, FormatVersion: PackFormatVersion,
		Name: name, CreatedAt: time.Now().Format(time.RFC3339),
		SchemaVersion: 1, Edges: edges,
	}

	var total int64
	// 按指针排序：同一张图导两次，包里文件顺序稳定（便于 diff 两个包）
	sort.Slice(refs, func(i, j int) bool { return refs[i].pointer < refs[j].pointer })
	for i, ref := range refs {
		st, serr := os.Stat(ref.value)
		if serr != nil || st.IsDir() {
			continue
		}
		if st.Size() > packMaxPerFile {
			return nil, fmt.Errorf("素材过大：%s（%.1f MB，单文件上限 %d MB）",
				ref.value, float64(st.Size())/1e6, packMaxPerFile>>20)
		}
		total += st.Size()
		if total > packMaxTotal {
			return nil, fmt.Errorf("包体积超限（>%d MB）", packMaxTotal>>20)
		}
		rel := fmt.Sprintf("%s%03d_%s", packAssetPrefix, i+1, asciiAssetName(ref.value))
		sum, cerr := copyFileToZip(zw, rel, ref.value)
		if cerr != nil {
			return nil, cerr
		}
		setByPointer(tree, ref.pointer, rel) // 图里换成包内相对路径
		manifest.Assets = append(manifest.Assets, PackAsset{
			Pointer: ref.pointer, File: rel, Bytes: st.Size(), SHA256: sum,
		})
	}

	rewritten, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("改写后的图序列化失败：%w", err)
	}
	manifest.Nodes = rewritten

	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	w, err := zw.Create(packManifestName)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(mb); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("收尾失败：%w", err)
	}
	return manifest, nil
}

// ImportPack 解开一个包：素材落到 destDir，图里的相对路径改写成 destDir 下的绝对路径。
//
// 校验顺序是刻意的：先验 format/版本，再验每个素材的 sha256，最后才改写图。
// 任何一步失败都不留半成品——半个包比没包更危险（图能跑，素材是上一版的）。
func ImportPack(zipPath, destDir string) (*PackManifest, json.RawMessage, json.RawMessage, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("打不开包：%w", err)
	}
	defer zr.Close()

	var manifestRaw []byte
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return nil, nil, nil, fmt.Errorf("包内路径非法（可能是恶意包）：%s", f.Name)
		}
		if name == packManifestName {
			rc, oerr := f.Open()
			if oerr != nil {
				return nil, nil, nil, oerr
			}
			manifestRaw, oerr = io.ReadAll(io.LimitReader(rc, 32<<20))
			rc.Close()
			if oerr != nil {
				return nil, nil, nil, oerr
			}
			continue
		}
		if strings.HasPrefix(name, packAssetPrefix) {
			files[name] = f
		}
	}
	if len(manifestRaw) == 0 {
		return nil, nil, nil, fmt.Errorf("包里没有 %s（不是 FlowEdge 导演包？）", packManifestName)
	}
	var m PackManifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		return nil, nil, nil, fmt.Errorf("清单解析失败：%w", err)
	}
	if m.Format != PackFormat {
		return nil, nil, nil, fmt.Errorf("不是 FlowEdge 导演包（format=%q）", m.Format)
	}
	if m.FormatVersion > PackFormatVersion {
		return nil, nil, nil, fmt.Errorf("包版本 %d 比当前支持（%d）新：请升级 FlowEdge 再导入",
			m.FormatVersion, PackFormatVersion)
	}
	if len(m.Assets) > packMaxAssets {
		return nil, nil, nil, fmt.Errorf("素材条目过多（%d）", len(m.Assets))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, nil, nil, err
	}

	var tree interface{}
	if len(m.Nodes) > 0 {
		if err := json.Unmarshal(m.Nodes, &tree); err != nil {
			return nil, nil, nil, fmt.Errorf("图解析失败：%w", err)
		}
	}

	for _, a := range m.Assets {
		zf, ok := files[a.File]
		if !ok {
			return nil, nil, nil, fmt.Errorf("清单里写了 %s，包里却没有这个文件（包不完整）", a.File)
		}
		dest := filepath.Join(destDir, filepath.FromSlash(a.File))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, nil, nil, err
		}
		sum, werr := extractZipFile(zf, dest)
		if werr != nil {
			return nil, nil, nil, werr
		}
		if a.SHA256 != "" && sum != a.SHA256 {
			return nil, nil, nil, fmt.Errorf("素材校验失败：%s（包里内容与清单不符）", a.File)
		}
		if tree != nil {
			abs, _ := filepath.Abs(dest)
			setByPointer(tree, a.Pointer, abs)
		}
	}

	var nodes json.RawMessage
	if tree != nil {
		nodes, err = json.Marshal(tree)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return &m, nodes, m.Edges, nil
}

// ptrRef 一处「这个 JSON 位置存的是一个真实文件的路径」。
type ptrRef struct {
	pointer string
	value   string
}

// collectFileRefs 递归找出图里所有"本地已存在的文件"。
//
// 判据刻意保守：非空、不是 URL、路径存在且是普通文件。宁可漏掉（导入后是个死路径，
// 用户立刻看得见）也不要误收（把一个提示词当成文件名塞进包里）。
func collectFileRefs(tree interface{}) []ptrRef {
	var out []ptrRef
	var walk func(v interface{}, pointer string)
	walk = func(v interface{}, pointer string) {
		switch node := v.(type) {
		case map[string]interface{}:
			for k, child := range node {
				walk(child, pointer+"/"+escapePointerToken(k))
			}
		case []interface{}:
			for i, child := range node {
				walk(child, fmt.Sprintf("%s/%d", pointer, i))
			}
		case string:
			s := strings.TrimSpace(node)
			if s == "" || strings.Contains(s, "://") {
				return
			}
			st, err := os.Stat(s)
			if err != nil || st.IsDir() {
				return
			}
			out = append(out, ptrRef{pointer: pointer, value: s})
		}
	}
	walk(tree, "")
	return out
}

func escapePointerToken(k string) string {
	k = strings.ReplaceAll(k, "~", "~0")
	return strings.ReplaceAll(k, "/", "~1")
}

func unescapePointerToken(k string) string {
	k = strings.ReplaceAll(k, "~1", "/")
	return strings.ReplaceAll(k, "~0", "~")
}

// setByPointer 按 RFC6901 把某个位置的值改掉（只改已存在的位置，不建新节点）。
func setByPointer(tree interface{}, pointer, value string) {
	if pointer == "" || tree == nil {
		return
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	cur := tree
	for i, raw := range parts {
		token := unescapePointerToken(raw)
		last := i == len(parts)-1
		switch node := cur.(type) {
		case map[string]interface{}:
			if last {
				if _, ok := node[token]; ok {
					node[token] = value
				}
				return
			}
			next, ok := node[token]
			if !ok {
				return
			}
			cur = next
		case []interface{}:
			idx := 0
			if _, err := fmt.Sscanf(token, "%d", &idx); err != nil || idx < 0 || idx >= len(node) {
				return
			}
			if last {
				node[idx] = value
				return
			}
			cur = node[idx]
		default:
			return
		}
	}
}

// asciiAssetName 生成包内安全文件名：只留 ASCII 字母数字与 ._-，其余折成下划线。
//
// 为什么不留原名：中文名在 zip 里靠 UTF-8 标志位，解压工具各不相同（Windows 资源
// 管理器、7-Zip、macOS 归档实用工具三家三种结果），折成 ASCII 就永远不会错。
func asciiAssetName(p string) string {
	base := filepath.Base(p)
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := strings.Trim(b.String(), "._")
	if s == "" {
		s = "asset"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s + ext
}

func copyFileToZip(zw *zip.Writer, rel, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("读素材失败：%w", err)
	}
	defer in.Close()
	w, err := zw.Create(rel)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), in); err != nil {
		return "", fmt.Errorf("写素材失败：%w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractZipFile(f *zip.File, dest string) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), rc); err != nil {
		return "", fmt.Errorf("解素材失败：%w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PackAssetRelPath 把包内相对路径（assets/001_x.png）转成落盘路径。
// 单独抽出来是为了让调用方不必再写一遍 path/filepath 转换（Windows 上反斜杠是坑）。
func PackAssetRelPath(rel string) string {
	return filepath.Join(filepath.FromSlash(path.Clean("/" + rel)))
}

package service

// 内容寻址与产物血缘（V3 P0）
//
// 为什么需要：见 docs/架构升级-FlowEdge-V3-总纲-20261031.md §2.3。
// 现状是 shot-cache 只哈希 prompt/宽高/帧/步数/backend/turbo/pdd，
// 不含权重版本、seed、参考图 hash —— 于是「换权重」「换定妆图」都不会让缓存失效，
// 拿旧片当新片；而且跑完没有任何一份「这次用了什么」的清单，三个月后无法复现。
//
// 本文件只做两件事，都不改变现有出片行为：
//  1. ArtifactKey：把「全量输入」压成一个确定性 hash（不含任何缓存语义）。
//  2. Manifest：把一次运行用到的模型指纹、参数、产物 hash、耗时落成一份可离线复现的清单。
//
// 刻意不做：不默认启用产物缓存。AGENTS 已定「缓存命中会让重跑变成假重跑」，
// 这里只提供血缘；缓存是血缘的一个可选消费者，由调用方显式开启。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ArtifactKey 内容寻址键：同一个 key 表示「输入完全一致」。
//
// 注意 Params 里必须放**节点 data 全量**（含默认值合并后的结果），
// 只放用户显式传入的字段会让「改默认值」不失效。
type ArtifactKey struct {
	NodeType string            `json:"node_type"`
	Pipeline string            `json:"pipeline,omitempty"`
	Params   map[string]any    `json:"params"`
	Models   map[string]string `json:"models,omitempty"` // role/slot -> 指纹
	Seed     int64             `json:"seed"`
	Inputs   []string          `json:"inputs,omitempty"` // 上游产物摘要，按入边顺序
	Code     string            `json:"code"`             // 引擎版本 + 缓存算法版本
}

// Hash 返回 key 的稳定摘要（32 位十六进制）。
// 用 encoding/json 序列化 map 时 Go 会对键排序，因此结果确定。
func (k ArtifactKey) Hash() string {
	// Inputs 顺序影响语义（多前驱汇合时不同来源不能混），保留原序。
	b, err := json.Marshal(k)
	if err != nil {
		// 理论上不会发生（字段都是可序列化类型）。真发生就退化成时间戳，
		// 宁可「不命中」也不要「错误命中」。
		return fmt.Sprintf("unhashable-%d", time.Now().UnixNano())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32]
}

// ModelFingerprint 权重指纹。
//
// 为什么不是完整 sha256：H3 权重单文件 20GB，全量哈希要几十秒，
// 每次出片都算等于把交付时间拉长。用「首 1MB + 尾 1MB + 文件大小」的快速摘要，
// 足以发现权重替换（换量化版本几乎必然改大小与尾部），且 <1s。
// 需要强校验时用 Method 区分：quick / full / unavailable。
type ModelFingerprint struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	ModUnix int64  `json:"mod_nano"` // 纳秒：秒级精度会让"同秒内同尺寸替换"漏检
	Digest  string `json:"digest"`
	Method  string `json:"method"`
}

const quickDigestWindow = 1 << 20 // 1MB

// quickFileDigest 首尾各 1MB + 大小 的 sha256（前 16 位十六进制）。
func quickFileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := st.Size()
	h := sha256.New()
	head := size
	if head > quickDigestWindow {
		head = quickDigestWindow
	}
	if head > 0 {
		if _, err := io.CopyN(h, f, head); err != nil && err != io.EOF {
			return "", size, err
		}
	}
	if size > quickDigestWindow*2 {
		if _, err := f.Seek(size-quickDigestWindow, io.SeekStart); err != nil {
			return "", size, err
		}
		if _, err := io.CopyN(h, f, quickDigestWindow); err != nil && err != io.EOF {
			return "", size, err
		}
	}
	fmt.Fprintf(h, "\x00%d\x00", size)
	return hex.EncodeToString(h.Sum(nil))[:16], size, nil
}

// FingerprintCache 指纹缓存：按 (path, size, mtime) 复用，落盘避免重启重算。
//
// 大文件哈希是「一次性成本」，缓存后同一台机器上的出片不再付第二次。
type FingerprintCache struct {
	path    string
	mu      sync.Mutex
	entries map[string]ModelFingerprint
	index   *ModelIndex
	dirty   bool
}

// LoadFingerprintCache 读缓存文件；不存在或损坏都返回空缓存（不阻断出片）。
func LoadFingerprintCache(path string) *FingerprintCache {
	c := &FingerprintCache{path: path, entries: map[string]ModelFingerprint{}}
	if path == "" {
		return c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var raw struct {
		Entries map[string]ModelFingerprint `json:"entries"`
	}
	if json.Unmarshal(b, &raw) == nil && raw.Entries != nil {
		c.entries = raw.Entries
	}
	return c
}

// SetIndex 注入「模型文件名 -> 绝对路径」索引（nil 时按路径直取）。
func (c *FingerprintCache) SetIndex(idx *ModelIndex) { c.index = idx }

// Resolve 把 bundle 里的文件名解析成绝对路径（已存在则原样返回）。
func (c *FingerprintCache) Resolve(name string) string {
	if name == "" {
		return ""
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			return name
		}
	}
	if c.index != nil {
		if p := c.index.Lookup(name); p != "" {
			return p
		}
	}
	return name
}

// Of 返回某个权重文件的指纹（带缓存）。
func (c *FingerprintCache) Of(path string) ModelFingerprint {
	if path == "" {
		return ModelFingerprint{Method: "unavailable"}
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return ModelFingerprint{Name: filepath.Base(path), Path: path, Method: "unavailable"}
	}

	c.mu.Lock()
	if e, ok := c.entries[path]; ok && e.Bytes == st.Size() && e.ModUnix == st.ModTime().UnixNano() && e.Digest != "" {
		c.mu.Unlock()
		return e
	}
	c.mu.Unlock()

	digest, size, derr := quickFileDigest(path)
	fp := ModelFingerprint{
		Name:    filepath.Base(path),
		Path:    filepath.ToSlash(path),
		Bytes:   size,
		ModUnix: st.ModTime().UnixNano(),
		Method:  "quick",
	}
	if derr != nil {
		fp.Method = "unavailable"
	} else {
		fp.Digest = digest
	}

	c.mu.Lock()
	c.entries[path] = fp
	c.dirty = true
	c.mu.Unlock()
	return fp
}

// OfNames 批量解析一组「role/slot -> 文件名」，返回 role/slot -> digest。
// 解析不到的记 "unavailable"，不静默丢弃（否则 manifest 会看起来「没有模型」）。
func (c *FingerprintCache) OfNames(names map[string]string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for slot, name := range names {
		fp := c.Of(c.Resolve(name))
		if fp.Digest == "" {
			out[slot] = "unavailable:" + name
		} else {
			out[slot] = fp.Digest
		}
	}
	return out
}

// Save 落盘（只在有变化时写）。
func (c *FingerprintCache) Save() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]ModelFingerprint, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.dirty = false
	c.mu.Unlock()

	b, err := json.MarshalIndent(struct {
		Version int                         `json:"version"`
		Entries map[string]ModelFingerprint `json:"entries"`
	}{Version: 1, Entries: snapshot}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// ModelIndex 模型文件名 -> 路径索引。
//
// 只建一次（惰性），文件数在千级、目录深度有限，成本可忽略；
// 相比之下每次出片遍历一遍 D:/AI/Models 才是浪费。
type ModelIndex struct {
	root  string
	once  sync.Once
	files map[string]string // base name -> path（同名冲突保留先遇到的，见 note）
}

// NewModelIndex 创建索引（root 为空时自动按 ModelsRoot 规则解析）。
func NewModelIndex(root string) *ModelIndex {
	if strings.TrimSpace(root) == "" {
		root = ModelsRoot("")
	}
	return &ModelIndex{root: root}
}

// Root 返回索引根目录。
func (m *ModelIndex) Root() string { return m.root }

// Lookup 按文件名查路径（惰性建索引）。
func (m *ModelIndex) Lookup(name string) string {
	if m == nil || name == "" {
		return ""
	}
	m.once.Do(m.build)
	return m.files[name]
}

func (m *ModelIndex) build() {
	m.files = map[string]string{}
	if m.root == "" {
		return
	}
	// 深度限制 6：模型目录结构最多 diffusion_models/xxx/yyy 这种，不需要全盘递归。
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 6 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(p, depth+1)
				continue
			}
			// 同名冲突：保留路径更短的那个（通常是主目录版本），
			// 并在 manifest 里能看出实际用的是哪个绝对路径。
			if prev, ok := m.files[e.Name()]; ok && len(prev) <= len(p) {
				continue
			}
			m.files[e.Name()] = filepath.ToSlash(p)
		}
	}
	walk(m.root, 0)
}

// ArtifactRef 单个产物引用。
type ArtifactRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Kind   string `json:"kind"` // image / video / audio / other
}

// NodeManifest 单个节点的血缘。
type NodeManifest struct {
	NodeID     string            `json:"node_id"`
	NodeType   string            `json:"node_type"`
	Status     string            `json:"status"`
	Key        string            `json:"key,omitempty"`
	Seed       int64             `json:"seed,omitempty"`
	Models     map[string]string `json:"models,omitempty"`
	Artifacts  []ArtifactRef     `json:"artifacts,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	ErrorCode  string            `json:"error_code,omitempty"`
}

// ManifestResources 资源对账（V3 预算门的数据基础）。
type ManifestResources struct {
	WallSeconds float64 `json:"wall_seconds"`
	PeakVRAMMB  int64   `json:"peak_vram_mb,omitempty"`
	GPUSeconds  float64 `json:"gpu_seconds,omitempty"`
}

// Manifest 一次运行的完整清单：回答「这次用了什么、产出是什么、能不能复现」。
type Manifest struct {
	Version    int               `json:"version"`
	RunID      uint              `json:"run_id"`
	PipelineID string            `json:"pipeline_id,omitempty"`
	Engine     string            `json:"engine"`
	InvokedBy  string            `json:"invoked_by,omitempty"`
	Params     map[string]any    `json:"params,omitempty"`
	StartedAt  string            `json:"started_at,omitempty"`
	EndedAt    string            `json:"ended_at,omitempty"`
	Nodes      []NodeManifest    `json:"nodes"`
	Resources  ManifestResources `json:"resources"`
	// Quality 五维质量向量（L3 回填；未计算时为空）。
	Quality map[string]any `json:"quality,omitempty"`
	// Budget 预估与实耗的对账（V3 预算门；未估算时为空）。
	Budget *BudgetReconcile `json:"budget,omitempty"`
	// Notes 生成过程中的判定说明（不参与复现，只供人读）。
	Notes []string `json:"notes,omitempty"`
}

// artifactKindOf 按扩展名判类型（只用于展示与统计，不参与复现）。
func artifactKindOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp":
		return "image"
	case ".mp4", ".mov", ".mkv", ".webm", ".avi":
		return "video"
	case ".wav", ".mp3", ".m4a", ".flac", ".aac":
		return "audio"
	case ".srt", ".ass", ".vtt", ".json", ".txt", ".md":
		return "text"
	}
	return "other"
}

// ArtifactRefOf 为一个产物生成引用。文件不存在时返回 ok=false（不编造）。
// 大文件（>512MB）用快速摘要并标注 kind，避免拖慢运行收尾。
func ArtifactRefOf(path string) (ArtifactRef, bool) {
	if path == "" {
		return ArtifactRef{}, false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return ArtifactRef{}, false
	}
	ref := ArtifactRef{Path: filepath.ToSlash(path), Bytes: st.Size(), Kind: artifactKindOf(path)}
	if digest, _, derr := quickFileDigest(path); derr == nil {
		ref.SHA256 = digest
	}
	return ref, true
}

// BuildManifest 从运行结果构造清单。
//
// 参数刻意保持「全是显式输入」：不做任何隐式读取，
// 这样单测可以完全离线构造，也避免 handler 层与 service 层互相猜。
func BuildManifest(run RunMeta, nodes []NodeInstanceLite, fps *FingerprintCache, engineVersion string) *Manifest {
	m := &Manifest{
		Version:    1,
		RunID:      run.ID,
		PipelineID: run.PipelineID,
		Engine:     engineVersion,
		InvokedBy:  run.InvokedBy,
		Params:     run.Params,
	}
	if run.StartedAt != nil {
		m.StartedAt = run.StartedAt.Format(time.RFC3339)
	}
	if run.EndedAt != nil {
		m.EndedAt = run.EndedAt.Format(time.RFC3339)
		if run.StartedAt != nil {
			m.Resources.WallSeconds = run.EndedAt.Sub(*run.StartedAt).Seconds()
		}
	}

	for _, n := range nodes {
		nm := NodeManifest{
			NodeID:    n.ID,
			NodeType:  n.Type,
			Status:    n.Status,
			ErrorCode: n.ErrorCode,
			Seed:      n.Seed,
			Models:    n.ModelNames,
		}
		if n.DurationMS > 0 {
			nm.DurationMS = n.DurationMS
		}
		// 模型指纹：把节点声明用到的模型名解析成 digest。
		if fps != nil && len(n.ModelNames) > 0 {
			nm.Models = fps.OfNames(n.ModelNames)
		}
		// 产物：只登记真实存在的文件，不编造路径。
		for _, p := range n.ArtifactPaths {
			if ref, ok := ArtifactRefOf(p); ok {
				nm.Artifacts = append(nm.Artifacts, ref)
			}
		}
		// 内容寻址键：参数 + 模型 + seed + 上游产物摘要。
		if len(n.Params) > 0 || len(nm.Models) > 0 || len(n.ArtifactPaths) > 0 {
			key := ArtifactKey{
				NodeType: n.Type,
				Pipeline: run.PipelineID,
				Params:   n.Params,
				Models:   nm.Models,
				Seed:     n.Seed,
				Inputs:   n.InputDigests,
				Code:     engineVersion + "|" + shotCacheVersion,
			}
			nm.Key = key.Hash()
		}
		m.Nodes = append(m.Nodes, nm)
	}
	sort.SliceStable(m.Nodes, func(i, j int) bool { return m.Nodes[i].NodeID < m.Nodes[j].NodeID })
	return m
}

// WriteManifest 落盘到 <dir>/run-<id>.json，返回路径。
func WriteManifest(dir string, m *Manifest) (string, error) {
	if dir == "" || m == nil {
		return "", fmt.Errorf("manifest: 目录或内容为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("run-%d.json", m.RunID))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// ReadManifest 读回清单。
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// RunMeta 清单需要的运行元信息（避免 service 反向依赖 model 包）。
type RunMeta struct {
	ID         uint
	PipelineID string
	InvokedBy  string
	Params     map[string]any
	StartedAt  *time.Time
	EndedAt    *time.Time
}

// ArtifactPathOf 从节点输出里取产物路径（导出版，供 handler 组装 manifest）。
// 与 quality_loop.go 内部的 artifactPathOf 语义一致：只认第一个非空路径字段。
func ArtifactPathOf(out interface{}) string { return artifactPathOf(out) }

// modelRolesByNodeType 节点类型 -> 可能用到的 bundle 角色。
//
// 刻意「宽」而不是「精确」：宁可多记一个模型的指纹（成本 = 一次 1MB 读），
// 也不要漏记（漏记 = manifest 声称没用到某权重，复现时直接跑偏）。
var modelRolesByNodeType = map[string][]string{
	"text2image":  {RoleT2IPrimary, RoleT2IBatch, RoleT2IMultiPrompt, RoleT2IMOSS},
	"image2image": {RoleImg2Img},
	"inpaint":     {RoleInpaint},
	"upscale":     {RoleUpscale},
	"text2video":  {RoleVideoH3Fl2va, RoleVideoH3Ref2va},
	"shotlist":    {RoleVideoH3Fl2va, RoleVideoH3Ref2va},
}

// ModelRolesForNodes 汇总一组节点类型会用到的角色（去重、稳定顺序）。
func ModelRolesForNodes(nodeTypes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range nodeTypes {
		for _, r := range modelRolesByNodeType[t] {
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// ModelNamesForRoles 把角色展开成「role.slot -> 文件名」，供 FingerprintCache.OfNames 解析。
// bs 为 nil 时返回空（manifest 里模型段留空，而不是编造）。
func ModelNamesForRoles(bs *BundleStore, roles []string) map[string]string {
	if bs == nil || len(roles) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, role := range roles {
		b := bs.Resolve(role)
		if b == nil || len(b.Files) == 0 {
			continue
		}
		slots := make([]string, 0, len(b.Files))
		for slot := range b.Files {
			slots = append(slots, slot)
		}
		sort.Strings(slots)
		for _, slot := range slots {
			out[role+"."+slot] = b.Files[slot]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NodeInstanceLite 清单需要的节点信息（同上，保持 service 层无 model 依赖）。
type NodeInstanceLite struct {
	ID            string
	Type          string
	Status        string
	ErrorCode     string
	Seed          int64
	DurationMS    int64
	Params        map[string]any
	ModelNames    map[string]string
	ArtifactPaths []string
	InputDigests  []string
}

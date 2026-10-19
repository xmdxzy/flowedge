package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 凭据加密存储（Credentials）
//
// 目标：HTTP 节点不再把 API Key 明文写在节点配置里（工作流一导出就泄密）。
// 做法：
//   - 值用 AES-256-GCM 加密后落 data/credentials.json（密文 + 每次独立 nonce）
//   - 主密钥来自 FLOWEDGE_MASTER_KEY（hex/base64），未设置则首次启动在
//     data/.master_key 生成 32 字节随机密钥（0600）
//   - 列表接口只回密钥名与 header 名，永不回值；值只在校验时解密
//
// 边界：这是「本机单人」场景的防护（防止导出泄漏 / 误截图 / 备份明文），
// 不是对抗本机管理员的方案——能读 data 目录的人本来就能读密钥文件。

// CredentialSecret 一条凭据的实际内容
type CredentialSecret struct {
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Note    string            `json:"note,omitempty"`
}

// Credential 明文凭据（仅内部使用，永不直接回给前端）
type Credential struct {
	Name      string
	Secret    CredentialSecret
	UpdatedAt time.Time
}

// MaskedCredential 脱敏视图
type MaskedCredential struct {
	Name       string    `json:"name"`
	Note       string    `json:"note,omitempty"`
	HeaderKeys []string  `json:"header_keys"`
	QueryKeys  []string  `json:"query_keys"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type credEntry struct {
	Nonce     string    `json:"nonce"`
	Data      string    `json:"data"`
	UpdatedAt time.Time `json:"updated_at"`
}

type credFile struct {
	Version int                  `json:"version"`
	Items   map[string]credEntry `json:"items"`
}

// CredentialStore 加密凭据仓库（文件持久化 + 内存缓存）
type CredentialStore struct {
	mu    sync.RWMutex
	path  string
	key   []byte
	items map[string]Credential
}

// NewCredentialStore 打开/初始化凭据仓库；dir 通常是 data/
func NewCredentialStore(dir string) (*CredentialStore, error) {
	if dir == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	key, err := loadMasterKey(dir)
	if err != nil {
		return nil, err
	}
	s := &CredentialStore{
		path:  filepath.Join(dir, "credentials.json"),
		key:   key,
		items: map[string]Credential{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadMasterKey 读取或生成主密钥
func loadMasterKey(dir string) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv("FLOWEDGE_MASTER_KEY")); raw != "" {
		if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
			return b, nil
		}
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
			return b, nil
		}
		// 任意长度的口令：用 SHA-256 派生，避免因为格式不对直接拒绝启动
		sum := sha256.Sum256([]byte(raw))
		return sum[:], nil
	}

	keyPath := filepath.Join(dir, ".master_key")
	if b, err := os.ReadFile(keyPath); err == nil && len(b) == 32 {
		return b, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成主密钥失败：%w", err)
	}
	if err := os.WriteFile(keyPath, buf, 0600); err != nil {
		return nil, fmt.Errorf("写入主密钥失败：%w", err)
	}
	return buf, nil
}

func (s *CredentialStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 首次启动
		}
		return err
	}
	var f credFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("凭据文件损坏（%s）：%w", s.path, err)
	}
	for name, e := range f.Items {
		plain, err := s.decrypt(e.Nonce, e.Data)
		if err != nil {
			// 单条坏了不影响其他条：跳过并保留原文，用户可以重新录入
			continue
		}
		var secret CredentialSecret
		if json.Unmarshal(plain, &secret) != nil {
			continue
		}
		s.items[name] = Credential{Name: name, Secret: secret, UpdatedAt: e.UpdatedAt}
	}
	return nil
}

func (s *CredentialStore) persistLocked() error {
	f := credFile{Version: 1, Items: make(map[string]credEntry, len(s.items))}
	for name, c := range s.items {
		plain, err := json.Marshal(c.Secret)
		if err != nil {
			return err
		}
		nonce, cipherText, err := s.encrypt(plain)
		if err != nil {
			return err
		}
		f.Items[name] = credEntry{Nonce: nonce, Data: cipherText, UpdatedAt: c.UpdatedAt}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // 原子替换，避免写一半断电留下坏文件
}

func (s *CredentialStore) encrypt(plain []byte) (string, string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *CredentialStore) decrypt(nonceB64, dataB64 string) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("nonce 长度异常")
	}
	return gcm.Open(nil, nonce, data, nil)
}

// Set 新增或覆盖一条凭据
func (s *CredentialStore) Set(name string, secret CredentialSecret) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("凭据名称不能为空")
	}
	if strings.ContainsAny(name, " \t/\\") {
		return errors.New("凭据名称不能包含空格或斜杠")
	}
	if len(secret.Headers) == 0 && len(secret.Query) == 0 {
		return errors.New("凭据至少要包含一个 header 或 query 参数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[name] = Credential{Name: name, Secret: secret, UpdatedAt: time.Now()}
	return s.persistLocked()
}

// Delete 删除凭据
func (s *CredentialStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[name]; !ok {
		return fmt.Errorf("凭据 %s 不存在", name)
	}
	delete(s.items, name)
	return s.persistLocked()
}

// Get 取一条明文凭据（仅供执行期注入，禁止直接回给前端）
func (s *CredentialStore) Get(name string) (Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.items[name]
	return c, ok
}

// List 脱敏列表
func (s *CredentialStore) List() []MaskedCredential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]MaskedCredential, 0, len(s.items))
	for _, c := range s.items {
		m := MaskedCredential{Name: c.Name, Note: c.Secret.Note, UpdatedAt: c.UpdatedAt}
		for k := range c.Secret.Headers {
			m.HeaderKeys = append(m.HeaderKeys, k)
		}
		for k := range c.Secret.Query {
			m.QueryKeys = append(m.QueryKeys, k)
		}
		sort.Strings(m.HeaderKeys)
		sort.Strings(m.QueryKeys)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve 解析成可直接用的 header/query 映射；未找到返回明确错误
func (s *CredentialStore) Resolve(name string) (map[string]string, map[string]string, error) {
	if name == "" {
		return nil, nil, nil
	}
	c, ok := s.Get(name)
	if !ok {
		return nil, nil, fmt.Errorf("凭据 %q 不存在（请在「凭据」面板新建，或检查节点里的名称）", name)
	}
	h := make(map[string]string, len(c.Secret.Headers))
	for k, v := range c.Secret.Headers {
		h[k] = v
	}
	q := make(map[string]string, len(c.Secret.Query))
	for k, v := range c.Secret.Query {
		q[k] = v
	}
	return h, q, nil
}

// appendQuery 把凭据里的 query 参数拼到 URL 上（已存在的同名参数不覆盖）
func appendQuery(rawURL string, params map[string]string) string {
	if len(params) == 0 {
		return rawURL
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	var sb strings.Builder
	sb.WriteString(rawURL)
	sb.WriteString(sep)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(url.QueryEscape(k))
		sb.WriteString("=")
		sb.WriteString(url.QueryEscape(params[k]))
	}
	return sb.String()
}

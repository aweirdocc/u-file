// Package source 文件源的生命周期管理：增、删、改、测试连接、持久化与热加载。
//
// M1 决策（在 M0 基础上演进）：
//   - 凭据 AES-256-GCM 加密落库：内存中保持明文（驱动实例化需要），
//     仅在 sources.json 读写边界对敏感字段加解密；旧明文文件首次保存时自动加密迁移；
//   - 主密钥存 data/master.key（0600，首次自动生成），丢失则旧密文不可恢复；
//   - config.yaml 仅作为首次启动的播种来源（文件不存在时导入其中定义）；
//     之后以 sources.json 为准，避免改写用户 YAML 破坏注释与格式；
//   - 增删改即时生效：变更立刻注册/摘除驱动，无需重启。
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"ufile/internal/crypt"
	"ufile/internal/driver"
)

var (
	ErrNotFound   = errors.New("source: not found")
	ErrDuplicate  = errors.New("source: duplicate id")
	ErrBadID      = errors.New("source: invalid id")
	ErrTypeUnkown = errors.New("source: unknown source type")
	ErrNoCipher   = errors.New("source: cipher not configured")

	idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Source 一个已注册文件源的运行时表示。
type Source struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Type   string              `json:"type"`
	Config map[string]any      `json:"config"` // 内存中为明文；HTTP 响应必须脱敏（见 Sanitized()）
	Caps   driver.Capabilities `json:"capabilities"`
	drv    driver.FSDriver
}

// Driver 返回底层驱动实例（仅限服务端内部使用）。
func (s *Source) Driver() driver.FSDriver { return s.drv }

// Sanitized 返回给前端的视图：config 中的敏感字段（key/secret/password）打码。
func (s *Source) Sanitized() *Source {
	out := *s
	cfg := make(map[string]any, len(s.Config))
	for k, v := range s.Config {
		if isSecretKey(k) && v != "" {
			cfg[k] = redactedMark
		} else {
			cfg[k] = v
		}
	}
	out.Config = cfg
	return &out
}

func isSecretKey(k string) bool {
	switch k {
	case "access_key", "secret_key", "password":
		return true
	}
	return false
}

// TestResult 连接测试结果。
type TestResult struct {
	OK      bool   `json:"ok"`
	Latency int64  `json:"latency_ms"` // 建立驱动 + List 根目录的耗时
	Error   string `json:"error,omitempty"`
}

// Manager 持有全部源，串行化变更。
type Manager struct {
	mu     sync.RWMutex
	cipher *crypt.Cipher // nil 表示未启用加密（单测场景），落库降级为明文
	sources map[string]*Source
	order   []string
	file    string // data/sources.json

	// OnRemove 源删除后的回调（可选；索引缓存联动清理等）。
	// 在锁外调用，签名 params 为被删源 ID。
	OnRemove func(id string)
}

// NewManager 构造管理器。seed 为 config.yaml 中的初始定义：仅当持久化文件
// 不存在时导入，之后即以持久化文件为准。cipher 可为 nil（不加密落库）。
func NewManager(dataDir string, seed []Seed, cipher *crypt.Cipher) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		sources: map[string]*Source{},
		file:    filepath.Join(dataDir, "sources.json"),
		cipher:  cipher,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if len(m.sources) == 0 && len(seed) > 0 {
		for _, s := range seed {
			if _, err := m.Add(s.ID, s.Name, s.Type, s.Config); err != nil {
				return nil, fmt.Errorf("seed source %q: %w", s.ID, err)
			}
		}
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Seed config.yaml 的播种定义。
type Seed struct {
	ID     string
	Name   string
	Type   string
	Config map[string]any
}

// —— 查询 ——

func (m *Manager) List() []*Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Source, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.sources[id].Sanitized())
	}
	return out
}

func (m *Manager) Get(id string) (*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sources[id]
	if !ok {
		return nil, ErrNotFound
	}
	// 内部使用（下载/列取）：返回含 config 的原对象，调用方不得外传
	cp := *s
	return &cp, nil
}

// —— 变更 ——

func (m *Manager) Add(id, name, typ string, cfg map[string]any) (*Source, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("%w：id 只允许字母数字与 - _，1~64 字符", ErrBadID)
	}
	// 未知类型 / 配置不完整，立刻暴露
	drv, err := driver.New(typ, cfg)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = id
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.sources[id]; dup {
		return nil, fmt.Errorf("%w：%s", ErrDuplicate, id)
	}
	s := &Source{ID: id, Name: name, Type: typ, Config: cfg, Caps: drv.Capabilities(), drv: drv}
	m.sources[id] = s
	m.order = append(m.order, id)
	if err := m.saveLocked(); err != nil {
		// 持久化失败则回滚内存，保证内存与磁盘一致
		delete(m.sources, id)
		m.order = m.order[:len(m.order)-1]
		return nil, err
	}
	return s.Sanitized(), nil
}

// Update 编辑已有源。keepSecret=true 表示调用方没改凭据（config 中凭据字段
// 为打码值或缺失），保留数据库里的旧凭据；否则整体替换配置。
// 类型/连通性校验与 Add 相同；ID 不可变。
func (m *Manager) Update(id, name string, cfg map[string]any, keepSecret bool) (*Source, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.sources[id]
	if !ok {
		return nil, ErrNotFound
	}
	// 打码值视为“未修改”——用户表单里带回了 ******，不能把它当真凭据存进去
	if keepSecret && cfg != nil {
		merged := make(map[string]any, len(cfg))
		for k, v := range cfg {
			if isSecretKey(k) && v == redactedMark {
				if ov, ok := old.Config[k]; ok && v != "" {
					merged[k] = ov
					continue
				}
			}
			merged[k] = v
		}
		cfg = merged
	}
	drv, err := driver.New(old.Type, cfg)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = old.Name
	}
	prev := old.Config
	s := &Source{ID: id, Name: name, Type: old.Type, Config: cfg, Caps: drv.Capabilities(), drv: drv}
	m.sources[id] = s
	if err := m.saveLocked(); err != nil {
		m.sources[id] = old
		old.Config = prev
		return nil, err
	}
	return s.Sanitized(), nil
}

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	if _, ok := m.sources[id]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	idx := -1
	for i, v := range m.order {
		if v == id {
			idx = i
			break
		}
	}
	removed := m.sources[id]
	delete(m.sources, id)
	m.order = append(m.order[:idx], m.order[idx+1:]...)
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		// 回滚
		m.mu.Lock()
		m.sources[id] = removed
		m.order = append(m.order, "")
		copy(m.order[idx+1:], m.order[idx:])
		m.order[idx] = id
		m.mu.Unlock()
		return err
	}
	if m.OnRemove != nil {
		m.OnRemove(id)
	}
	return nil
}

// IDs 返回全部源 ID（按注册顺序；供索引同步枚举）。
func (m *Manager) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// Test 用给定参数建立驱动并列取根目录，返回耗时与错误摘要。
// 不落盘 —— “先测后存”由前端组合完成。
func (m *Manager) Test(ctx context.Context, typ string, cfg map[string]any) *TestResult {
	start := time.Now()
	drv, err := driver.New(typ, cfg)
	if err != nil {
		return &TestResult{OK: false, Latency: ms(start), Error: err.Error()}
	}
	if _, err := drv.List(ctx, "", driver.ListOptions{Limit: 1}); err != nil {
		return &TestResult{OK: false, Latency: ms(start), Error: err.Error()}
	}
	return &TestResult{OK: true, Latency: ms(start)}
}

func ms(start time.Time) int64 { return time.Since(start).Milliseconds() }

// —— 持久化 ——

// stored 不导出运行时字段（Caps 由驱动重算，drv 不序列化）。
// Config 敏感字段在 saveLocked/load 时经 cipher 加解密。
type stored struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config"`
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil // 首次启动，靠 seed 播种
	}
	if err != nil {
		return fmt.Errorf("source: 读取持久化文件失败: %w", err)
	}
	var list []stored
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("source: 解析 %s 失败: %w", m.file, err)
	}
	dirty := false // 发现明文凭据即标记，加载完成后立即加密重写
	for _, s := range list {
		dec, err := m.decryptConfig(s.Config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] source %q 凭据解密失败（主密钥不匹配或文件损坏），已跳过: %v\n", s.ID, err)
			continue
		}
		if encryptedCount(s.Config) < secretCount(dec) {
			dirty = true // 磁盘上还有明文凭据 → 触发一次加密重写
		}
		drv, err := driver.New(s.Type, dec)
		if err != nil {
			// 单个源损坏（如驱动被移除、配置缺失）不阻断整体启动，标记待修
			fmt.Fprintf(os.Stderr, "[warn] source %q 加载失败，已跳过: %v\n", s.ID, err)
			continue
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		m.sources[s.ID] = &Source{ID: s.ID, Name: s.Name, Type: s.Type, Config: dec, Caps: drv.Capabilities(), drv: drv}
		m.order = append(m.order, s.ID)
	}
	if dirty && m.cipher != nil {
		if err := m.saveLocked(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] 凭据加密迁移写入失败（保留明文，下次保存重试）: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[info] 已将明文凭据加密迁移至 %s\n", m.file)
		}
	}
	return nil
}

// encryptConfig 返回落库视图：敏感字段加密。零拷贝无谓复制——只在有敏感
// 字段需要加密时才复制 map。
func (m *Manager) encryptConfig(cfg map[string]any) (map[string]any, error) {
	if m.cipher == nil || cfg == nil {
		return cfg, nil
	}
	need := false
	for k, v := range cfg {
		if isSecretKey(k) {
			if s, _ := v.(string); s != "" && !crypt.IsEncrypted(s) {
				need = true
				break
			}
		}
	}
	if !need {
		return cfg, nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if isSecretKey(k) {
			if s, ok := v.(string); ok && s != "" && !crypt.IsEncrypted(s) {
				enc, err := m.cipher.Seal(s)
				if err != nil {
					return nil, err
				}
				out[k] = enc
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}

// decryptConfig 返回内存视图：敏感字段解密为明文（驱动实例化需要）。
func (m *Manager) decryptConfig(cfg map[string]any) (map[string]any, error) {
	if m.cipher == nil || cfg == nil {
		return cfg, nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if isSecretKey(k) {
			if s, ok := v.(string); ok && crypt.IsEncrypted(s) {
				plain, err := m.cipher.Open(s)
				if err != nil {
					return nil, err
				}
				out[k] = plain
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}

// saveLocked 调用方必须已持有写锁。敏感字段加密后原子写盘。
func (m *Manager) saveLocked() error {
	list := make([]stored, 0, len(m.order))
	for _, id := range m.order {
		s := m.sources[id]
		enc, err := m.encryptConfig(s.Config)
		if err != nil {
			return fmt.Errorf("source %s: %w", s.ID, err)
		}
		list = append(list, stored{ID: s.ID, Name: s.Name, Type: s.Type, Config: enc})
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("source: 写入持久化文件失败: %w", err)
	}
	return os.Rename(tmp, m.file) // 原子替换，避免写一半崩溃留下坏文件
}

// secretCount 统计配置中非空的敏感字段数。
func secretCount(cfg map[string]any) int {
	n := 0
	for k, v := range cfg {
		if isSecretKey(k) {
			if s, _ := v.(string); s != "" {
				n++
			}
		}
	}
	return n
}

// encryptedCount 统计配置中已是密文形态的敏感字段数。
func encryptedCount(cfg map[string]any) int {
	n := 0
	for k, v := range cfg {
		if isSecretKey(k) {
			if s, _ := v.(string); crypt.IsEncrypted(s) {
				n++
			}
		}
	}
	return n
}

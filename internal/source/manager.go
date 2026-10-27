// Package source 文件源的生命周期管理：增、删、测试连接、持久化与热加载。
//
// M0+ 简化决策（对标 docs/04 架构设计的 MVP 阶段）：
//   - 源定义持久化在 data/sources.json（凭据仍为明文，加密落库在 M1 完成）；
//   - config.yaml 仅作为首次启动的播种来源（文件不存在时导入其中定义）；
//     之后以 sources.json 为准，避免改写用户 YAML 破坏注释与格式；
//   - 增删即时生效：新增立刻注册路由可用，删除立刻摘除。
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

	"ufile/internal/driver"
)

var (
	ErrNotFound   = errors.New("source: not found")
	ErrDuplicate  = errors.New("source: duplicate id")
	ErrBadID      = errors.New("source: invalid id")
	ErrTypeUnkown = errors.New("source: unknown source type")

	idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Source 一个已注册文件源的运行时表示。
type Source struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Type   string              `json:"type"`
	Config map[string]any      `json:"config"` // 含凭据；HTTP 响应必须脱敏（见 Sanitized())
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
			cfg[k] = "******"
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
	mu      sync.RWMutex
	sources map[string]*Source
	order   []string
	file    string // data/sources.json
}

// NewManager 构造管理器。seed 为 config.yaml 中的初始定义：仅当持久化文件
// 不存在时导入，之后即以持久化文件为准。
func NewManager(dataDir string, seed []Seed) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		sources: map[string]*Source{},
		file:    filepath.Join(dataDir, "sources.json"),
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

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[id]; !ok {
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
	if err := m.saveLocked(); err != nil {
		// 回滚
		m.sources[id] = removed
		m.order = append(m.order, "")
		copy(m.order[idx+1:], m.order[idx:])
		m.order[idx] = id
		return err
	}
	return nil
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
	for _, s := range list {
		drv, err := driver.New(s.Type, s.Config)
		if err != nil {
			// 单个源损坏（如驱动被移除、配置缺失）不阻断整体启动，标记待修
			fmt.Fprintf(os.Stderr, "[warn] source %q 加载失败，已跳过: %v\n", s.ID, err)
			continue
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		m.sources[s.ID] = &Source{ID: s.ID, Name: s.Name, Type: s.Type, Config: s.Config, Caps: drv.Capabilities(), drv: drv}
		m.order = append(m.order, s.ID)
	}
	return nil
}

// saveLocked 调用方必须已持有写锁。
func (m *Manager) saveLocked() error {
	list := make([]stored, 0, len(m.order))
	for _, id := range m.order {
		s := m.sources[id]
		list = append(list, stored{ID: s.ID, Name: s.Name, Type: s.Type, Config: s.Config})
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

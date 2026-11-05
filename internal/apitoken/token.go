// Package apitoken 实现 M4.2 开放 API Token 管理系统。
//
// 安全设计（对齐 docs/06 4.1 节）：
//   - Token 格式为 "uft_" + 64 位加密随机十六进制串；
//   - 明文仅在创建时返回一次，服务端仅持久化 SHA-256 哈希值（data/tokens.json）；
//   - 支持细粒度权限作用域（Scope）、允许的 Source 白名单、来源 IP/CIDR 白名单与到期时间；
//   - 支持随时一键吊销或删除。
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const TokenPrefix = "uft_"

type Scope string

const (
	ScopeFilesRead    Scope = "files:read"
	ScopeFilesWrite   Scope = "files:write"
	ScopeFilesDelete  Scope = "files:delete"
	ScopeSourcesRead  Scope = "sources:read"
	ScopeSharesManage Scope = "shares:manage"
)

var AllScopes = []Scope{
	ScopeFilesRead,
	ScopeFilesWrite,
	ScopeFilesDelete,
	ScopeSourcesRead,
	ScopeSharesManage,
}

func ValidScope(s Scope) bool {
	for _, v := range AllScopes {
		if v == s {
			return true
		}
	}
	return false
}

var (
	ErrTokenInvalid   = errors.New("apitoken: API Token 无效或不存在")
	ErrTokenExpired   = errors.New("apitoken: API Token 已过期")
	ErrTokenRevoked   = errors.New("apitoken: API Token 已被吊销")
	ErrIPForbidden    = errors.New("apitoken: 当前来源 IP 不在 Token 白名单内")
	ErrScopeForbidden = errors.New("apitoken: API Token 缺少执行该操作所需的 Scope 权限")
	ErrNotFound       = errors.New("apitoken: Token 记录不存在")
)

// Token 开放 API 令牌模型（TokenHash 不在普通列表回显明文）。
type Token struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	TokenPrefix    string     `json:"token_prefix"` // 如 "uft_9a8b7c..." 供界面识别
	TokenHash      string     `json:"token_hash,omitempty"`
	CreatedBy      string     `json:"created_by"`
	Scopes         []Scope    `json:"scopes"`
	AllowedSources []string   `json:"allowed_sources,omitempty"` // 空表示不限制源
	IPWhitelist    []string   `json:"ip_whitelist,omitempty"`    // 空表示不限制 IP；支持单 IP 或 CIDR
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`      // nil 表示永不过期
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	Revoked        bool       `json:"revoked"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Sanitized 返回隐去哈希值的公开管理视图。
func (t *Token) Sanitized() *Token {
	cp := *t
	cp.TokenHash = ""
	if t.Scopes != nil {
		cp.Scopes = append([]Scope(nil), t.Scopes...)
	}
	if t.AllowedSources != nil {
		cp.AllowedSources = append([]string(nil), t.AllowedSources...)
	}
	if t.IPWhitelist != nil {
		cp.IPWhitelist = append([]string(nil), t.IPWhitelist...)
	}
	return &cp
}

// HasScope 判断 Token 是否包含指定 Scope。
func (t *Token) HasScope(want Scope) bool {
	for _, s := range t.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// AllowsSource 判断 Token 是否允许访问目标文件源（AllowedSources 为空代表全部允许）。
func (t *Token) AllowsSource(sourceID string) bool {
	if len(t.AllowedSources) == 0 {
		return true
	}
	for _, s := range t.AllowedSources {
		if s == "*" || s == sourceID {
			return true
		}
	}
	return false
}

// CreateOptions 创建 API Token 的参数。
type CreateOptions struct {
	Name           string
	CreatedBy      string
	Scopes         []Scope
	AllowedSources []string
	IPWhitelist    []string
	TTLHours       int // <=0 表示永不过期
}

// Manager 管理全部 API Token 的持久化与校验。
type Manager struct {
	mu     sync.RWMutex
	file   string
	tokens map[string]*Token
	order  []string
}

func NewManager(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:   filepath.Join(dataDir, "tokens.json"),
		tokens: map[string]*Token{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// IsAPIToken 判断 Bearer 字符串是否为开放 API Token 格式。
func IsAPIToken(raw string) bool {
	return strings.HasPrefix(raw, TokenPrefix)
}

// Create 生成新 Token，返回脱敏元数据与一次性明文令牌。
func (m *Manager) Create(opt CreateOptions) (*Token, string, error) {
	name := strings.TrimSpace(opt.Name)
	if name == "" || len(name) > 80 {
		return nil, "", errors.New("apitoken: 名称长度需在 1~80 字符之间")
	}
	if len(opt.Scopes) == 0 {
		return nil, "", errors.New("apitoken: 至少需要勾选一个权限作用域 (Scope)")
	}
	seenScope := map[Scope]bool{}
	var scopes []Scope
	for _, s := range opt.Scopes {
		if !ValidScope(s) {
			return nil, "", fmt.Errorf("apitoken: 未知权限作用域 %q", s)
		}
		if !seenScope[s] {
			seenScope[s] = true
			scopes = append(scopes, s)
		}
	}
	// 若包含 files:write 或 files:delete，自动授予 files:read
	if (seenScope[ScopeFilesWrite] || seenScope[ScopeFilesDelete]) && !seenScope[ScopeFilesRead] {
		scopes = append([]Scope{ScopeFilesRead}, scopes...)
	}

	var ips []string
	for _, rawIP := range opt.IPWhitelist {
		s := strings.TrimSpace(rawIP)
		if s == "" {
			continue
		}
		if !isValidIPOrCIDR(s) {
			return nil, "", fmt.Errorf("apitoken: 非法 IP 或 CIDR 格式 %q", s)
		}
		ips = append(ips, s)
	}

	var srcs []string
	seenSrc := map[string]bool{}
	for _, s := range opt.AllowedSources {
		s = strings.TrimSpace(s)
		if s != "" && !seenSrc[s] {
			seenSrc[s] = true
			srcs = append(srcs, s)
		}
	}

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return nil, "", err
	}
	plain := TokenPrefix + hex.EncodeToString(rawBytes)
	hash := hashString(plain)

	now := time.Now()
	var exp *time.Time
	if opt.TTLHours > 0 {
		t := now.Add(time.Duration(opt.TTLHours) * time.Hour)
		exp = &t
	}

	idBytes := make([]byte, 6)
	_, _ = rand.Read(idBytes)
	tok := &Token{
		ID:             "tok_" + hex.EncodeToString(idBytes),
		Name:           name,
		TokenPrefix:    plain[:12] + "...",
		TokenHash:      hash,
		CreatedBy:      opt.CreatedBy,
		Scopes:         scopes,
		AllowedSources: srcs,
		IPWhitelist:    ips,
		ExpiresAt:      exp,
		CreatedAt:      now,
	}

	m.mu.Lock()
	m.tokens[tok.ID] = tok
	m.order = append([]string{tok.ID}, m.order...)
	if err := m.saveLocked(); err != nil {
		delete(m.tokens, tok.ID)
		m.order = m.order[1:]
		m.mu.Unlock()
		return nil, "", err
	}
	out := tok.Sanitized()
	m.mu.Unlock()
	return out, plain, nil
}

// Verify 校验明文 Token、过期时间及来源 IP 白名单。
func (m *Manager) Verify(plainToken, clientIP string) (*Token, error) {
	if !IsAPIToken(plainToken) {
		return nil, ErrTokenInvalid
	}
	targetHash := hashString(plainToken)

	m.mu.Lock()
	defer m.mu.Unlock()

	var matched *Token
	for _, id := range m.order {
		t := m.tokens[id]
		if t == nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(targetHash)) == 1 {
			matched = t
			break
		}
	}
	if matched == nil {
		return nil, ErrTokenInvalid
	}
	if matched.Revoked {
		return nil, ErrTokenRevoked
	}
	now := time.Now()
	if matched.ExpiresAt != nil && now.After(*matched.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	if len(matched.IPWhitelist) > 0 && !matchIPWhitelist(clientIP, matched.IPWhitelist) {
		return nil, ErrIPForbidden
	}

	// 记录最近使用时间（距离上次超过 1 分钟才触发写盘，避免高频请求 IO 放大）
	shouldSave := matched.LastUsedAt == nil || now.Sub(*matched.LastUsedAt) > time.Minute
	matched.LastUsedAt = &now
	if shouldSave {
		_ = m.saveLocked()
	}
	return matched.Sanitized(), nil
}

// List 列出全部 Token（已脱敏）。
func (m *Manager) List() []*Token {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Token, 0, len(m.order))
	for _, id := range m.order {
		if t := m.tokens[id]; t != nil {
			out = append(out, t.Sanitized())
		}
	}
	return out
}

// Revoke 吊销指定 Token。
func (m *Manager) Revoke(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return ErrNotFound
	}
	t.Revoked = true
	return m.saveLocked()
}

// Delete 彻底删除指定 Token。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[id]; !ok {
		return ErrNotFound
	}
	delete(m.tokens, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return m.saveLocked()
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Token
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("apitoken: 解析 %s 失败: %w", m.file, err)
	}
	for _, t := range list {
		if t != nil && t.ID != "" {
			m.tokens[t.ID] = t
			m.order = append(m.order, t.ID)
		}
	}
	return nil
}

func (m *Manager) saveLocked() error {
	list := make([]*Token, 0, len(m.order))
	for _, id := range m.order {
		if t := m.tokens[id]; t != nil {
			list = append(list, t)
		}
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func isValidIPOrCIDR(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

func matchIPWhitelist(clientIP string, whitelist []string) bool {
	if clientIP == "" {
		return false
	}
	ip := net.ParseIP(clientIP)
	for _, rule := range whitelist {
		if rule == clientIP {
			return true
		}
		if ip != nil {
			if ruleIP := net.ParseIP(rule); ruleIP != nil && ruleIP.Equal(ip) {
				return true
			}
			if _, netCIDR, err := net.ParseCIDR(rule); err == nil && netCIDR.Contains(ip) {
				return true
			}
		}
	}
	return false
}

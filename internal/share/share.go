// Package share M3 分享外链：把某个源内的路径以带约束的公开链接暴露给访客。
//
// 组成：
//   - Manager：分享记录存于 data/shares.json（JSON 原子写，与 source/auth 同模式）；
//   - 提取码 bcrypt 哈希落库；校验失败按 token+IP 指数退避限速（照抄 auth/limiter）；
//   - grant：提取码验证通过后签发 HMAC cookie（免重复输码），密钥复用 data/jwt.key。
//
// 访问约束：过期 / 撤销 / 下载次数用尽一律视为已失效（Gone）。
package share

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrNotFound     = errors.New("share: not found")
	ErrExpired      = errors.New("share: 已过期")
	ErrRevoked      = errors.New("share: 已撤销")
	ErrExhausted    = errors.New("share: 下载次数已用完")
	ErrBadPassword  = errors.New("share: 提取码错误")
	ErrSourceGone   = errors.New("share: 文件源已删除")
	ErrRateLimited  = errors.New("share: 尝试次数过多，请稍后再试")
	ErrBadLifetime  = errors.New("share: 无效的有效期")
	ErrBadMaxDl     = errors.New("share: 无效的下载次数限制")
	ErrBadPassword2 = errors.New("share: 提取码需 4~64 字符")
)

// MaxTTL 分享有效期上限。
const MaxTTL = 30 * 24 * time.Hour

// grantTTL 提取码验证通过后的免输码时长（grant cookie 有效期）。
const grantTTL = time.Hour

// Share 一条分享记录。
type Share struct {
	ID            string    `json:"id"`
	Token         string    `json:"token"` // 公开链接凭据（base64url 22 字符）
	SourceID      string    `json:"source_id"`
	Path          string    `json:"path"`
	Name          string    `json:"name"` // 冗余展示字段（创建时的文件/目录名）
	Size          int64     `json:"size"`
	IsDir         bool      `json:"is_dir"`
	PasswordHash  string    `json:"password_hash,omitempty"` // bcrypt；空=无提取码
	ExpiresAt     time.Time `json:"expires_at"`
	MaxDownloads  int64     `json:"max_downloads"` // 0=不限
	Downloads     int64     `json:"downloads"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	Revoked       bool      `json:"revoked"`
}

// HasPassword 是否设置了提取码（对外只暴露布尔，不回显哈希）。
func (s *Share) HasPassword() bool { return s.PasswordHash != "" }

// check 校验分享当前是否可访问（不含提取码）。
func (s *Share) check() error {
	if s.Revoked {
		return ErrRevoked
	}
	if time.Now().After(s.ExpiresAt) {
		return ErrExpired
	}
	if s.MaxDownloads > 0 && s.Downloads >= s.MaxDownloads {
		return ErrExhausted
	}
	return nil
}

// Manager 持有全部分享，串行化变更。
type Manager struct {
	mu      sync.RWMutex
	shares  map[string]*Share // by id
	byToken map[string]*Share // by token（token 不可变，与 shares 同步维护）
	order   []string
	file    string // data/shares.json
	secret  []byte // grant cookie 的 HMAC 签名密钥（复用 jwt.key）

	limiter *limiter
}

// NewManager 构造管理器并加载已有分享。secret 为 grant 签名密钥。
func NewManager(dataDir string, secret []byte) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		shares:  map[string]*Share{},
		byToken: map[string]*Share{},
		file:    filepath.Join(dataDir, "shares.json"),
		secret:  secret,
		limiter: newLimiter(),
	}
	m.limiter.gc()
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("share: 读取持久化文件失败: %w", err)
	}
	var list []Share
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("share: 解析 %s 失败: %w", m.file, err)
	}
	for i := range list {
		s := &list[i]
		m.shares[s.ID] = s
		m.byToken[s.Token] = s
		m.order = append(m.order, s.ID)
	}
	return m, nil
}

// CreateInput 创建参数（ifetime/max 校验在此做，bcrypt 在此算）。
type CreateInput struct {
	SourceID     string
	Path         string
	Name         string
	Size         int64
	IsDir        bool
	Password     string // 空=无提取码
	Lifetime     time.Duration
	MaxDownloads int64
	CreatedBy    string
}

// Create 新建分享，返回含 token 的完整记录。
func (m *Manager) Create(in CreateInput) (*Share, error) {
	if in.Lifetime <= 0 || in.Lifetime > MaxTTL {
		return nil, fmt.Errorf("%w（1 分钟 ~ %d 天）", ErrBadLifetime, int(MaxTTL.Hours()/24))
	}
	if in.MaxDownloads < 0 {
		return nil, ErrBadMaxDl
	}
	var hash string
	if in.Password != "" {
		if len(in.Password) < 4 || len(in.Password) > 64 {
			return nil, ErrBadPassword2
		}
		h, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		hash = string(h)
	}
	s := &Share{
		ID:           newID(),
		Token:       newToken(),
		SourceID:     in.SourceID,
		Path:         in.Path,
		Name:         in.Name,
		Size:         in.Size,
		IsDir:        in.IsDir,
		PasswordHash: hash,
		ExpiresAt:    time.Now().Add(in.Lifetime),
		MaxDownloads: in.MaxDownloads,
		CreatedBy:    in.CreatedBy,
		CreatedAt:    time.Now(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shares[s.ID] = s
	m.byToken[s.Token] = s
	m.order = append(m.order, s.ID)
	if err := m.saveLocked(); err != nil {
		delete(m.shares, s.ID)
		delete(m.byToken, s.Token)
		m.order = m.order[:len(m.order)-1]
		return nil, err
	}
	return s, nil
}

// List 返回全部分享（按创建顺序）。
func (m *Manager) List() []*Share {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Share, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.shares[id])
	}
	return out
}

// Delete 撤销并删除分享记录。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.shares[id]
	if !ok {
		return ErrNotFound
	}
	idx := -1
	for i, v := range m.order {
		if v == id {
			idx = i
			break
		}
	}
	delete(m.shares, id)
	delete(m.byToken, s.Token)
	m.order = append(m.order[:idx], m.order[idx+1:]...)
	return m.saveLocked()
}

// Accessible 按 token 取一条当前可访问的分享（过期/撤销/超次视为不存在语义，
// 返回对应错误供 handler 区分提示）。仅校验提取码之外的条件。
func (m *Manager) Accessible(token string) (*Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byToken[token]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	if err := cp.check(); err != nil {
		return nil, err
	}
	return &cp, nil
}

// CountDownload 下载完成后计数 +1 并落盘。分享已失效则拒绝。
func (m *Manager) CountDownload(token string) error {
	m.mu.Lock()
	s, ok := m.byToken[token]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if err := s.check(); err != nil {
		m.mu.Unlock()
		return err
	}
	s.Downloads++
	err := m.saveLocked()
	m.mu.Unlock()
	return err
}

// CheckPassword 校验提取码（无提取码的分享恒通过）。key 为限速维度（token+IP）。
func (m *Manager) CheckPassword(token, password, key string) error {
	m.mu.RLock()
	s, ok := m.byToken[token]
	var hash string
	if ok {
		hash = s.PasswordHash
	}
	m.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if hash == "" {
		return nil
	}
	if !m.limiter.Allow(key) {
		return fmt.Errorf("%w（还需等待 %s）", ErrRateLimited, m.limiter.RetryAfter(key).Round(time.Second))
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		m.limiter.Fail(key)
		return ErrBadPassword
	}
	m.limiter.Reset(key)
	return nil
}

// SignGrant 为已通过提取码校验的访客签发免输码凭据（绑定 token，1 小时有效）。
func (m *Manager) SignGrant(token string) string {
	exp := time.Now().Add(grantTTL).Unix()
	payload := token + "." + fmt.Sprint(exp)
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyGrant 校验 grant cookie 是否对该 token 有效。
func (m *Manager) VerifyGrant(token, grant string) bool {
	parts := splitLast(grant)
	if len(parts) != 2 {
		return false
	}
	payload := parts[0]
	if tok, rest, ok := cutLast(payload, "."); !ok || tok != token {
		return false
	} else if exp, err := parseInt64(rest); err != nil || time.Now().Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	return hmac.Equal(sig, mac.Sum(nil))
}

// GrantTTL 暴露 grant 有效期（handler 设置 cookie 用）。
func GrantTTL() time.Duration { return grantTTL }

// ExpireForTest 仅测试用：把指定分享的过期时间拨到过去。
func (m *Manager) ExpireForTest(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byToken[token]
	if !ok {
		return ErrNotFound
	}
	s.ExpiresAt = time.Now().Add(-time.Minute)
	return m.saveLocked()
}

// 持久化：全量 JSON 原子写（与 source/auth 同模板）。
func (m *Manager) saveLocked() error {
	list := make([]Share, 0, len(m.order))
	for _, id := range m.order {
		list = append(list, *m.shares[id])
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("share: 写入持久化文件失败: %w", err)
	}
	return os.Rename(tmp, m.file)
}

// newToken 生成公开链接 token：16 字节加密随机数 → base64url（22 字符）。
func newToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("share: 生成 token 失败: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// newID 生成内部记录 ID（同为 base64url 随机，仅作唯一标识）。
func newID() string { return newToken() }

// —— 小工具（避免为 split3 引 strconv/strings 全套）——

func splitLast(s string) []string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return "", "", false
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, errors.New("empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a number")
		}
		if n > (1<<63-1)/10 {
			return 0, errors.New("overflow")
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n, nil
}

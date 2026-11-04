// Package auth M1/M3 鉴权与细粒度权限控制：
//
// 组成：
//   - Store：用户存于 data/users.json，密码 bcrypt 哈希；首次启动无用户时
//     由引导接口创建管理员；支持 admin（全权）、user（按 ACL 策略授权）、guest（只读）；
//   - ACL：源级与路径前缀级访问控制（read / write / delete / share），见 acl.go；
//   - JWT：HS256，Claims 含角色；不引入外部 JWT 库，HMAC 由标准库实现；
//   - limiter：登录失败限速（按 用户名+来源IP，指数退避）。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrBadCredentials  = errors.New("auth: 用户名或密码错误")
	ErrUserDisabled    = errors.New("auth: 账号已被禁用，请联系管理员")
	ErrNoUsers         = errors.New("auth: 尚未创建管理员")
	ErrUserExists      = errors.New("auth: 用户已存在")
	ErrLastAdmin       = errors.New("auth: 不能删除或禁用最后一个管理员")
	ErrRateLimited     = errors.New("auth: 尝试次数过多，请稍后再试")
	ErrTokenInvalid    = errors.New("auth: 会话无效或已过期")
	ErrBootstrapNeeded = errors.New("auth: 需要引导令牌才能创建首个管理员")
)

// Role 用户角色：admin 全权；user 按 ACL 授权；guest 只读（浏览/下载/预览）。
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	RoleGuest Role = "guest"
)

// ValidRole 校验角色是否合法。
func ValidRole(r Role) bool {
	switch r {
	case RoleAdmin, RoleUser, RoleGuest:
		return true
	default:
		return false
	}
}

const tokenTTL = 24 * time.Hour

// —— 用户存储 ——

type storedUser struct {
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // bcrypt
	Role      Role      `json:"role"`
	Disabled  bool      `json:"disabled,omitempty"`
	Remark    string    `json:"remark,omitempty"`
	Policies  []Policy  `json:"policies,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type user struct {
	storedUser
	hash []byte
}

// Store 用户存储 + JWT 签发 + 登录密码解密。
type Store struct {
	mu     sync.RWMutex
	file   string // data/users.json
	secret []byte // JWT 签名密钥（data/jwt.key，独立于凭据主密钥）
	users  map[string]*user
	order  []string

	limiter *limiter

	loginKey *rsa.PrivateKey // 登录信封私钥（data/login.key）
	pubPEM   []byte          // 对应公钥（SPKI PEM，供前端加密）
}

// NewStore 打开（或初始化）用户存储。secret 为 JWT 签名密钥。
func NewStore(dataDir string, secret []byte) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	loginKey, err := loadOrCreateLoginKey(dataDir)
	if err != nil {
		return nil, err
	}
	pubPEM, err := publicKeyPEM(loginKey)
	if err != nil {
		return nil, err
	}
	s := &Store{
		file:     filepath.Join(dataDir, "users.json"),
		secret:   secret,
		users:    map[string]*user{},
		limiter:  newLimiter(),
		loginKey: loginKey,
		pubPEM:   pubPEM,
	}
	s.limiter.gc()
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []storedUser
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("auth: 解析 %s 失败: %w", s.file, err)
	}
	for _, u := range list {
		s.users[u.Name] = &user{storedUser: u, hash: []byte(u.Hash)}
		s.order = append(s.order, u.Name)
	}
	return s, nil
}

// HasUsers 是否已有任何用户（决定是否进入引导模式）。
func (s *Store) HasUsers() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users) > 0
}

// Create 创建用户（首个用户必须是 admin）。
func (s *Store) Create(name, password string, role Role) error {
	return s.CreateWithOptions(CreateUserOptions{
		Name:     name,
		Password: password,
		Role:     role,
	})
}

// Remove 删除用户（不允许删掉最后一个启用中的管理员）。
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrBadCredentials
	}
	if u.Role == RoleAdmin && !u.Disabled {
		if s.activeAdminCountLocked(name) == 0 {
			return ErrLastAdmin
		}
	}
	delete(s.users, name)
	for i, v := range s.order {
		if v == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return s.saveLocked()
}

// ChangePassword 管理员重置密码。
func (s *Store) ChangePassword(name, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("auth: 密码至少 8 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrBadCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Hash = string(hash)
	u.hash = hash
	u.UpdatedAt = time.Now()
	return s.saveLocked()
}

// saveLocked 调用方必须已持有写锁。
func (s *Store) saveLocked() error {
	list := make([]storedUser, 0, len(s.order))
	for _, name := range s.order {
		list = append(list, s.users[name].storedUser)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

// —— 登录 + JWT ——

// List 返回全部用户（按创建顺序；密码哈希不外发）。
func (s *Store) List() []storedUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]storedUser, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.users[name].storedUser)
	}
	return out
}

// Login 校验用户名密码，签发 JWT。key 为限速维度（用户名+来源 IP），
// 连续失败会触发指数退避限速。
func (s *Store) Login(name, password, key string) (token string, role Role, err error) {
	if !s.limiter.Allow(key) {
		return "", "", fmt.Errorf("%w（还需等待 %s）", ErrRateLimited, s.limiter.RetryAfter(key).Round(time.Second))
	}
	s.mu.RLock()
	u, ok := s.users[name]
	var hash []byte
	var role2 Role
	var disabled bool
	if ok {
		hash = u.hash
		role2 = u.Role
		disabled = u.Disabled
	}
	s.mu.RUnlock()
	// 用户不存在也做一次 bcrypt 比较，抹平时序差异
	if !ok {
		bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0X8fF2W6bQ6z1dKU9v3aZz8yE9O"), // "dummy"
			[]byte(password))
		s.limiter.Fail(key)
		return "", "", ErrBadCredentials
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		s.limiter.Fail(key)
		return "", "", ErrBadCredentials
	}
	if disabled {
		return "", "", ErrUserDisabled
	}
	s.limiter.Reset(key)
	tok, err := s.sign(name, role2)
	if err != nil {
		return "", "", err
	}
	return tok, role2, nil
}

// Claims JWT 载荷。
type Claims struct {
	Sub  string `json:"sub"`
	Role Role   `json:"role"`
	Exp  int64  `json:"exp"`
	Iat  int64  `json:"iat"`
}

// sign 签发 HS256 JWT。
func (s *Store) sign(name string, role Role) (string, error) {
	now := time.Now()
	cl := Claims{Sub: name, Role: role, Iat: now.Unix(), Exp: now.Add(tokenTTL).Unix()}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}
	payload := head + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify 校验 JWT 并返回 Claims。
func (s *Store) Verify(token string) (*Claims, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, ErrTokenInvalid
	}
	payload := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrTokenInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var cl Claims
	if err := json.Unmarshal(body, &cl); err != nil {
		return nil, ErrTokenInvalid
	}
	if time.Now().Unix() >= cl.Exp {
		return nil, ErrTokenInvalid
	}
	s.mu.RLock()
	u, ok := s.users[cl.Sub]
	if !ok || u.Disabled {
		s.mu.RUnlock()
		return nil, ErrTokenInvalid // 用户已被删除或已禁用
	}
	cl.Role = u.Role // 实时同步最新角色
	s.mu.RUnlock()
	return &cl, nil
}

// GenerateSecret 生成 JWT 签名密钥（32 字节加密随机数）。
func GenerateSecret() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

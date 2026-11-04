package auth

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Action 细粒度操作权限类型。
type Action string

const (
	ActionRead   Action = "read"   // 浏览目录、下载、在线预览、直链、打包、搜索
	ActionWrite  Action = "write"  // 上传文件、新建目录、重命名/移动
	ActionDelete Action = "delete" // 删除文件/目录
	ActionShare  Action = "share"  // 创建与管理公开分享外链
)

var allActions = []Action{ActionRead, ActionWrite, ActionDelete, ActionShare}

// ValidAction 校验动作标识是否合法。
func ValidAction(a Action) bool {
	switch a {
	case ActionRead, ActionWrite, ActionDelete, ActionShare:
		return true
	default:
		return false
	}
}

// Policy 单条源级/路径级访问控制策略。
// 支持单路径前缀 PathPrefix 与多目录树勾选 PathPrefixes（均为空表示整源全部目录可访问）。
type Policy struct {
	SourceID     string   `json:"source_id"`               // 目标源 ID，"*" 表示匹配所有源
	Actions      []Action `json:"actions"`                 // 允许的操作集合
	PathPrefix   string   `json:"path_prefix,omitempty"`   // 单目录限制前缀（向后兼容及单选时使用）
	PathPrefixes []string `json:"path_prefixes,omitempty"` // 目录树勾选的多目录前缀集合（空表示整源全选）
}

// HasAction 判断策略是否包含指定动作。
func (p Policy) HasAction(act Action) bool {
	for _, a := range p.Actions {
		if a == act {
			return true
		}
	}
	return false
}

// EffectivePrefixes 返回规整收敛后的路径前缀集合；若为空切片则表示整源无路径限制（全选）。
func (p Policy) EffectivePrefixes() []string {
	raw := make([]string, 0, len(p.PathPrefixes)+1)
	if len(p.PathPrefixes) > 0 {
		raw = append(raw, p.PathPrefixes...)
	}
	if p.PathPrefix != "" {
		raw = append(raw, p.PathPrefix)
	}
	if len(raw) == 0 {
		return nil
	}
	prefixes, err := normalizeAndCompactPrefixes(raw)
	if err != nil {
		return nil
	}
	return prefixes
}

// NormalizePath 规范化源内相对路径（去除首尾 /，统一分隔符）。
// 若路径包含 ".." 穿越段则返回第二个返回值 false。
func NormalizePath(raw string) (string, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", false
		}
	}
	cleaned := path.Clean("/" + s)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		cleaned = ""
	}
	return cleaned, true
}

// normalizeAndCompactPrefixes 规范化路径前缀列表并收敛父子包含关系：
//   - 若包含根目录 ""，则整源可访问，返回 nil；
//   - 若同时包含 "a" 与 "a/b"，自动收敛保留父目录 "a"。
func normalizeAndCompactPrefixes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	cleaned := make([]string, 0, len(in))
	for _, item := range in {
		if strings.TrimSpace(item) == "" {
			continue
		}
		norm, ok := NormalizePath(item)
		if !ok {
			return nil, fmt.Errorf("非法路径前缀 %q", item)
		}
		if norm == "" {
			// 显式传入 "/" 代表整源全部目录
			return nil, nil
		}
		cleaned = append(cleaned, norm)
	}
	if len(cleaned) == 0 {
		return nil, nil
	}
	// 按长度与字典序排序，使父路径排在子路径之前
	sort.Slice(cleaned, func(i, j int) bool {
		if len(cleaned[i]) != len(cleaned[j]) {
			return len(cleaned[i]) < len(cleaned[j])
		}
		return cleaned[i] < cleaned[j]
	})
	compact := make([]string, 0, len(cleaned))
	for _, p := range cleaned {
		redundant := false
		for _, parent := range compact {
			if p == parent || strings.HasPrefix(p, parent+"/") {
				redundant = true
				break
			}
		}
		if !redundant {
			compact = append(compact, p)
		}
	}
	return compact, nil
}

// NormalizePolicies 校验并规整策略列表：
//   - 过滤无动作的空策略；
//   - 去重并校验动作名；若含 write/delete/share 则自动补齐基础 read 权限；
//   - 规范化并收敛 PathPrefix / PathPrefixes（禁止含 ..）。
func NormalizePolicies(in []Policy) ([]Policy, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]Policy, 0, len(in))
	for _, p := range in {
		sid := strings.TrimSpace(p.SourceID)
		if sid == "" {
			return nil, errors.New("auth: 策略缺少 source_id")
		}
		rawPrefixes := make([]string, 0, len(p.PathPrefixes)+1)
		rawPrefixes = append(rawPrefixes, p.PathPrefixes...)
		if p.PathPrefix != "" {
			rawPrefixes = append(rawPrefixes, p.PathPrefix)
		}
		prefixes, err := normalizeAndCompactPrefixes(rawPrefixes)
		if err != nil {
			return nil, fmt.Errorf("auth: 源 %s 的%v", sid, err)
		}

		seen := map[Action]bool{}
		for _, a := range p.Actions {
			if !ValidAction(a) {
				return nil, fmt.Errorf("auth: 未知权限动作 %q", a)
			}
			seen[a] = true
		}
		if len(seen) == 0 {
			continue // 未勾选任何权限则跳过该源
		}
		// 拥有写/删/分享任一权限时，必须具备基础浏览权限
		seen[ActionRead] = true
		acts := make([]Action, 0, len(seen))
		for _, a := range allActions {
			if seen[a] {
				acts = append(acts, a)
			}
		}
		pol := Policy{
			SourceID:     sid,
			Actions:      acts,
			PathPrefixes: prefixes,
		}
		if len(prefixes) == 1 {
			pol.PathPrefix = prefixes[0]
		}
		out = append(out, pol)
	}
	return out, nil
}

// CreateUserOptions 创建用户的完整参数。
type CreateUserOptions struct {
	Name     string
	Password string
	Role     Role
	Disabled bool
	Remark   string
	Policies []Policy
}

// UpdateUserOptions 编辑用户的参数（指针为 nil 表示不修改该字段）。
type UpdateUserOptions struct {
	Role     *Role
	Disabled *bool
	Remark   *string
	Policies *[]Policy
}

// CreateWithOptions 按完整选项创建用户。
func (s *Store) CreateWithOptions(opt CreateUserOptions) error {
	name := strings.TrimSpace(opt.Name)
	if len(name) < 2 || len(name) > 64 {
		return fmt.Errorf("auth: 用户名长度需 2~64")
	}
	if len(opt.Password) < 8 {
		return errors.New("auth: 密码至少 8 位")
	}
	if !ValidRole(opt.Role) {
		return fmt.Errorf("auth: 未知角色 %q", opt.Role)
	}
	policies, err := NormalizePolicies(opt.Policies)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.users[name]; dup {
		return ErrUserExists
	}
	if len(s.users) == 0 {
		if opt.Role != RoleAdmin {
			return errors.New("auth: 首个用户必须为管理员")
		}
		if opt.Disabled {
			return errors.New("auth: 首个管理员不能为禁用状态")
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(opt.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now()
	u := &user{
		storedUser: storedUser{
			Name:      name,
			Hash:      string(hash),
			Role:      opt.Role,
			Disabled:  opt.Disabled,
			Remark:    strings.TrimSpace(opt.Remark),
			Policies:  policies,
			CreatedAt: now,
			UpdatedAt: now,
		},
		hash: hash,
	}
	s.users[name] = u
	s.order = append(s.order, name)
	return s.saveLocked()
}

// UpdateUser 更新用户角色、启用状态、备注或权限策略。
// 保护规则：系统至少保留一个处于启用状态的管理员。
func (s *Store) UpdateUser(name string, opt UpdateUserOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrBadCredentials
	}

	newRole := u.Role
	if opt.Role != nil {
		if !ValidRole(*opt.Role) {
			return fmt.Errorf("auth: 未知角色 %q", *opt.Role)
		}
		newRole = *opt.Role
	}
	newDisabled := u.Disabled
	if opt.Disabled != nil {
		newDisabled = *opt.Disabled
	}

	// 若该用户原本是有效管理员，现在要被降级或禁用，检查是否还有其他有效管理员
	if u.Role == RoleAdmin && !u.Disabled && (newRole != RoleAdmin || newDisabled) {
		if s.activeAdminCountLocked(name) == 0 {
			return ErrLastAdmin
		}
	}

	if opt.Policies != nil {
		norm, err := NormalizePolicies(*opt.Policies)
		if err != nil {
			return err
		}
		u.Policies = norm
	}
	u.Role = newRole
	u.Disabled = newDisabled
	if opt.Remark != nil {
		u.Remark = strings.TrimSpace(*opt.Remark)
	}
	u.UpdatedAt = time.Now()
	return s.saveLocked()
}

// SetPolicies 单独更新指定用户的 ACL 策略集合。
func (s *Store) SetPolicies(name string, policies []Policy) error {
	return s.UpdateUser(name, UpdateUserOptions{Policies: &policies})
}

// Get 获取单个用户（不含密码哈希内部切片）。
func (s *Store) Get(name string) (storedUser, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[name]
	if !ok {
		return storedUser{}, false
	}
	return u.storedUser, true
}

// activeAdminCountLocked 统计除 exclude 以外处于启用状态的管理员数量（调用方需持锁）。
func (s *Store) activeAdminCountLocked(exclude string) int {
	n := 0
	for name, u := range s.users {
		if name != exclude && u.Role == RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n
}

// CanAccess 判定用户 username 是否有权对 sourceID 下的 targetPath 执行 act 操作。
func (s *Store) CanAccess(username, sourceID, targetPath string, act Action) bool {
	cleanTarget, ok := NormalizePath(targetPath)
	if !ok {
		return false // 含 .. 穿越段一律拒绝
	}

	s.mu.RLock()
	u, exists := s.users[username]
	if !exists || u.Disabled {
		s.mu.RUnlock()
		return false
	}
	role := u.Role
	policies := u.Policies
	s.mu.RUnlock()

	// 1. 超级管理员拥有全部权限
	if role == RoleAdmin {
		return true
	}

	// 2. 向后兼容：未配置任何策略的 guest 角色默认拥有全局只读权限
	if role == RoleGuest && len(policies) == 0 {
		return act == ActionRead
	}

	// 3. 按 ACL 策略逐条匹配
	for _, p := range policies {
		if p.SourceID != "*" && p.SourceID != sourceID {
			continue
		}
		if !p.HasAction(act) {
			continue
		}
		prefixes := p.EffectivePrefixes()
		if len(prefixes) == 0 {
			return true // 未限制子目录（全选整源）
		}
		for _, prefix := range prefixes {
			if cleanTarget == prefix || strings.HasPrefix(cleanTarget, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// IsAncestorOfAllowed 判定当用户在某源配置了多个目录前缀（len > 1）时，
// dirPath 是否是这些允许前缀的公共祖先目录（用于在父目录浏览时仅展示被勾选的子目录分支）。
// 返回值：是否是允许的导航父目录，以及在该目录下允许看见的直接子项判断函数。
func (s *Store) IsAncestorOfAllowed(username, sourceID, dirPath string) (bool, func(entryPath string, isDir bool) bool) {
	cleanDir, ok := NormalizePath(dirPath)
	if !ok {
		return false, nil
	}
	acc := s.SourceAccessOf(username, sourceID)
	if acc == nil || len(acc.PathPrefixes) <= 1 {
		return false, nil
	}

	var matchedPrefixes []string
	for _, pfx := range acc.PathPrefixes {
		if cleanDir == "" || strings.HasPrefix(pfx, cleanDir+"/") {
			matchedPrefixes = append(matchedPrefixes, pfx)
		}
	}
	if len(matchedPrefixes) == 0 {
		return false, nil
	}

	filter := func(entryPath string, isDir bool) bool {
		cleanEntry, ok := NormalizePath(entryPath)
		if !ok {
			return false
		}
		for _, pfx := range matchedPrefixes {
			// 条目本身已在某个授权目录内，或是授权目录本身
			if cleanEntry == pfx || strings.HasPrefix(cleanEntry, pfx+"/") {
				return true
			}
			// 条目是通往某个授权目录的父级文件夹
			if isDir && strings.HasPrefix(pfx, cleanEntry+"/") {
				return true
			}
		}
		return false
	}
	return true, filter
}

// CanAccessSource 判定用户对指定源是否拥有某项操作权限（不限具体子路径，用于源列表可见性等）。
func (s *Store) CanAccessSource(username, sourceID string, act Action) bool {
	s.mu.RLock()
	u, exists := s.users[username]
	if !exists || u.Disabled {
		s.mu.RUnlock()
		return false
	}
	role := u.Role
	policies := u.Policies
	s.mu.RUnlock()

	if role == RoleAdmin {
		return true
	}
	if role == RoleGuest && len(policies) == 0 {
		return act == ActionRead
	}
	for _, p := range policies {
		if (p.SourceID == "*" || p.SourceID == sourceID) && p.HasAction(act) {
			return true
		}
	}
	return false
}

// CanShareAny 判断用户是否为管理员，或至少在任一源上拥有外链分享权限。
func (s *Store) CanShareAny(username string) bool {
	s.mu.RLock()
	u, exists := s.users[username]
	if !exists || u.Disabled {
		s.mu.RUnlock()
		return false
	}
	role := u.Role
	policies := u.Policies
	s.mu.RUnlock()

	if role == RoleAdmin {
		return true
	}
	for _, p := range policies {
		if p.HasAction(ActionShare) {
			return true
		}
	}
	return false
}

// SourceAccess 汇总用户在单个源上的有效权限与路径前缀（供前端渲染按钮与面包屑）。
type SourceAccess struct {
	Actions      []Action `json:"actions"`
	PathPrefix   string   `json:"path_prefix,omitempty"`
	PathPrefixes []string `json:"path_prefixes,omitempty"`
}

// SourceAccessOf 计算用户对 sourceID 的有效权限。若无访问权返回 nil。
func (s *Store) SourceAccessOf(username, sourceID string) *SourceAccess {
	s.mu.RLock()
	u, exists := s.users[username]
	if !exists || u.Disabled {
		s.mu.RUnlock()
		return nil
	}
	role := u.Role
	policies := u.Policies
	s.mu.RUnlock()

	if role == RoleAdmin {
		return &SourceAccess{Actions: []Action{ActionRead, ActionWrite, ActionDelete, ActionShare}}
	}
	if role == RoleGuest && len(policies) == 0 {
		return &SourceAccess{Actions: []Action{ActionRead}}
	}

	seen := map[Action]bool{}
	var allPrefixes []string
	 unrestricted := false
	matched := false
	for _, p := range policies {
		if p.SourceID != "*" && p.SourceID != sourceID {
			continue
		}
		matched = true
		for _, a := range p.Actions {
			seen[a] = true
		}
		pfxs := p.EffectivePrefixes()
		if len(pfxs) == 0 {
			unrestricted = true
		} else if !unrestricted {
			allPrefixes = append(allPrefixes, pfxs...)
		}
	}
	if !matched || !seen[ActionRead] {
		return nil
	}
	acts := make([]Action, 0, len(seen))
	for _, a := range allActions {
		if seen[a] {
			acts = append(acts, a)
		}
	}
	out := &SourceAccess{Actions: acts}
	if !unrestricted && len(allPrefixes) > 0 {
		compacted, _ := normalizeAndCompactPrefixes(allPrefixes)
		out.PathPrefixes = compacted
		if len(compacted) == 1 {
			out.PathPrefix = compacted[0]
		}
	}
	return out
}

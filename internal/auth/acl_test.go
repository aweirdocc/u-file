package auth

import (
	"errors"
	"testing"
)

func TestACLUserAndPolicies(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("root", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// 1. 创建普通用户（配置 nas 源读写 + 限制在 docs/team1，oss 源只读）
	err := s.CreateWithOptions(CreateUserOptions{
		Name:     "alice",
		Password: "password123",
		Role:     RoleUser,
		Remark:   "研发部",
		Policies: []Policy{
			{
				SourceID:   "nas",
				Actions:    []Action{ActionWrite}, // 应自动补齐 ActionRead
				PathPrefix: "/docs/team1/",
			},
			{
				SourceID: "oss",
				Actions:  []Action{ActionRead, ActionShare},
			},
		},
	})
	if err != nil {
		t.Fatalf("创建普通用户失败: %v", err)
	}

	// 2. 验证源级可见性
	if !s.CanAccessSource("alice", "nas", ActionRead) {
		t.Fatal("alice 应能读 nas 源")
	}
	if !s.CanAccessSource("alice", "nas", ActionWrite) {
		t.Fatal("alice 应能写 nas 源")
	}
	if s.CanAccessSource("alice", "nas", ActionDelete) {
		t.Fatal("alice 不应能删 nas 源")
	}
	if s.CanAccessSource("alice", "unassigned-src", ActionRead) {
		t.Fatal("alice 不应能访问未授权源")
	}

	// 3. 验证路径前缀限制与防穿越
	if !s.CanAccess("alice", "nas", "docs/team1", ActionRead) {
		t.Fatal("alice 应能访问前缀目录本身")
	}
	if !s.CanAccess("alice", "nas", "docs/team1/sub/a.txt", ActionWrite) {
		t.Fatal("alice 应能写前缀子目录")
	}
	if s.CanAccess("alice", "nas", "docs/team10/a.txt", ActionRead) {
		t.Fatal("前缀边界校验：docs/team10 不应匹配 docs/team1")
	}
	if s.CanAccess("alice", "nas", "docs", ActionRead) {
		t.Fatal("父目录 docs 不应被访问")
	}
	if s.CanAccess("alice", "nas", "", ActionRead) {
		t.Fatal("根目录不应被访问")
	}
	if s.CanAccess("alice", "nas", "docs/team1/../team2/a.txt", ActionRead) {
		t.Fatal("含 .. 路径穿越应被拒绝")
	}

	// 4. 验证 oss 全源只读 + 分享
	if !s.CanAccess("alice", "oss", "", ActionRead) {
		t.Fatal("alice 应能访问 oss 根目录")
	}
	if !s.CanAccess("alice", "oss", "any/path.pdf", ActionShare) {
		t.Fatal("alice 应能分享 oss 文件")
	}
	if s.CanAccess("alice", "oss", "any/path.pdf", ActionWrite) {
		t.Fatal("alice 不应能写 oss 文件")
	}

	// 5. 禁用用户后立即拒绝访问且 Token 失效
	tok, _, err := s.Login("alice", "password123", "ip1")
	if err != nil {
		t.Fatalf("alice 登录失败: %v", err)
	}
	disabled := true
	if err := s.UpdateUser("alice", UpdateUserOptions{Disabled: &disabled}); err != nil {
		t.Fatalf("禁用 alice 失败: %v", err)
	}
	if s.CanAccess("alice", "oss", "", ActionRead) {
		t.Fatal("禁用后不应通过 CanAccess")
	}
	if _, err := s.Verify(tok); err != ErrTokenInvalid {
		t.Fatalf("禁用后已签发 token 应立即失效，实际: %v", err)
	}
	if _, _, err := s.Login("alice", "password123", "ip1"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("禁用后登录应返回 ErrUserDisabled，实际: %v", err)
	}

	// 6. 保护最后一个有效管理员不可被禁用或降级
	if err := s.UpdateUser("root", UpdateUserOptions{Disabled: &disabled}); err != ErrLastAdmin {
		t.Fatalf("禁用唯一管理员应返回 ErrLastAdmin，实际: %v", err)
	}
	userRole := RoleUser
	if err := s.UpdateUser("root", UpdateUserOptions{Role: &userRole}); err != ErrLastAdmin {
		t.Fatalf("降级唯一管理员应返回 ErrLastAdmin，实际: %v", err)
	}
}

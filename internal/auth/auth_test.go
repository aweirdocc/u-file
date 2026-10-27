package auth

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), []byte("test-secret-32-bytes-xxxxxxxxxxxx!"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBootstrapAdminAndLogin(t *testing.T) {
	s := newTestStore(t)
	if s.HasUsers() {
		t.Fatal("新存储不应有用户")
	}
	// 首个用户必须为 admin
	if err := s.Create("root", "password123", RoleGuest); err == nil {
		t.Fatal("首个用户为 guest 应被拒绝")
	}
	if err := s.Create("root", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if !s.HasUsers() {
		t.Fatal("创建后应有用户")
	}
	tok, role, err := s.Login("root", "password123", "ip1")
	if err != nil || role != RoleAdmin {
		t.Fatalf("登录失败: role=%s err=%v", role, err)
	}
	// 错误密码
	if _, _, err := s.Login("root", "wrong-pass", "ip1"); err != ErrBadCredentials {
		t.Fatalf("错误密码应报 ErrBadCredentials，实际 %v", err)
	}
	// token 可校验
	cl, err := s.Verify(tok)
	if err != nil || cl.Sub != "root" || cl.Role != RoleAdmin {
		t.Fatalf("token 校验失败: %+v err=%v", cl, err)
	}
	// 篡改 token 任一部分均拒绝
	bad := tok[:len(tok)-3] + "aaa"
	if _, err := s.Verify(bad); err != ErrTokenInvalid {
		t.Fatalf("篡改 token 应拒绝，实际 %v", err)
	}
	// 换签名密钥的 store 拒绝
	s2 := newTestStore(t)
	if _, err := s2.Verify(tok); err != ErrTokenInvalid {
		t.Fatal("换密钥后旧 token 应拒绝")
	}
}

func TestGuestRole(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create("root", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("visitor", "password456", RoleGuest); err != nil {
		t.Fatal(err)
	}
	_, role, err := s.Login("visitor", "password456", "ip2")
	if err != nil || role != RoleGuest {
		t.Fatalf("guest 登录失败: %v", err)
	}
}

func TestPasswordHashNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create("root", "super-secret-pw", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(dir + "/users.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "super-secret-pw") {
		t.Fatal("密码明文泄漏到 users.json")
	}
	if !strings.Contains(raw, "$2") {
		t.Fatal("users.json 应含 bcrypt 哈希")
	}
}

func TestLastAdminProtected(t *testing.T) {
	s := newTestStore(t)
	s.Create("root", "password123", RoleAdmin)
	if err := s.Remove("root"); err != ErrLastAdmin {
		t.Fatalf("删除最后管理员应被拒绝，实际 %v", err)
	}
	s.Create("root2", "password123", RoleAdmin)
	if err := s.Remove("root"); err != nil {
		t.Fatalf("有第二个管理员后应可删除: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	s := newTestStore(t)
	s.Create("root", "password123", RoleAdmin)
	if err := s.ChangePassword("root", "short"); err == nil {
		t.Fatal("短密码应被拒绝")
	}
	if err := s.ChangePassword("root", "newpassword8"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login("root", "password123", "k"); err == nil {
		t.Fatal("旧密码应失效")
	}
	if _, _, err := s.Login("root", "newpassword8", "k"); err != nil {
		t.Fatalf("新密码应可登录: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	s := newTestStore(t)
	s.Create("root", "password123", RoleAdmin)
	key := "root|1.2.3.4"
	// 前 allowed-1 次失败不受限
	for i := 0; i < 4; i++ {
		if _, _, err := s.Login("root", "wrong", key); err == nil {
			t.Fatal("错误密码应失败")
		}
	}
	if _, _, err := s.Login("root", "password123", key); err != nil {
		t.Fatalf("第 5 次用正确密码仍应允许: %v", err)
	}
	// 再失败 5 次 → 触发限速
	for i := 0; i < 5; i++ {
		s.Login("root", "wrong", key)
	}
	if _, _, err := s.Login("root", "password123", key); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("应被限速，实际 %v", err)
	}
	// 其他 key 不受影响
	if _, _, err := s.Login("root", "password123", "other-ip"); err != nil {
		t.Fatalf("其他 IP 不应被牵连: %v", err)
	}
}

func TestLimiterBackoffGrows(t *testing.T) {
	l := newLimiter()
	k := "k"
	// 第一次进入惩罚：base=5s
	for i := 0; i < 5; i++ {
		limited := l.Fail(k)
		if i == 4 && !limited {
			t.Fatal("第 5 次失败应触发限速")
		}
	}
	d1 := l.RetryAfter(k)
	if d1 <= 0 {
		t.Fatal("限速中 RetryAfter 应 > 0")
	}
	// 惩罚窗口过后再失败 → 窗口更长（指数退避）
	l.fails[k].until = timeZero()
	for i := 0; i < 5; i++ {
		l.Fail(k)
	}
	d2 := l.RetryAfter(k)
	if d2 <= d1 {
		t.Fatalf("连续失败越多惩罚应越长: d1=%v d2=%v", d1, d2)
	}
	l.Reset(k)
	if l.RetryAfter(k) != 0 {
		t.Fatal("Reset 后应解除限速")
	}
}

func TestDeletedUserTokenInvalid(t *testing.T) {
	s := newTestStore(t)
	s.Create("root", "password123", RoleAdmin)
	s.Create("temp", "password123", RoleAdmin)
	tok, _, err := s.Login("temp", "password123", "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("temp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); err != ErrTokenInvalid {
		t.Fatal("被删除用户的 token 应立即失效")
	}
}

func timeZero() time.Time { return time.Time{} }

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

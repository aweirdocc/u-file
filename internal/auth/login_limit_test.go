package auth

import (
	"errors"
	"fmt"
	"testing"
)

// 回归测试：攻击者轮换伪造来源 IP（X-Forwarded-For）时，单 IP 维度限速
// 不再生效，但仅按用户名的第二维度仍应拦住对同一账号的在线爆破。
func TestLoginRateLimitPerUsernameAcrossIPs(t *testing.T) {
	s := newTestStore(t)
	s.Create("victim", "correct-horse", RoleAdmin)

	// 5 个伪造 IP 各失败 4 次（不超过单 IP 维度阈值 5），共 20 次达到用户名维度阈值 20
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("victim|10.0.0.%d", i)
		for j := 0; j < 4; j++ {
			if _, _, err := s.Login("victim", "wrong", key); !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("应返回凭据错误，实际 %v", err)
			}
		}
	}

	// 第 21 次：换全新 IP 用正确密码，仍应被用户名维度限速拦截
	if _, _, err := s.Login("victim", "correct-horse", "victim|10.0.0.99"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("轮换 IP 后应仍被用户名维度限速，实际 %v", err)
	}

	// 其他用户不受牵连
	s.Create("other", "password123", RoleGuest)
	if _, _, err := s.Login("other", "password123", "other|10.0.0.1"); err != nil {
		t.Fatalf("其他用户不应被牵连: %v", err)
	}
}

// 登录成功后用户名维度记录应清零，不影响该账号后续正常登录。
func TestLoginRateLimitUsernameResetOnSuccess(t *testing.T) {
	s := newTestStore(t)
	s.Create("root", "password123", RoleAdmin)

	// 累计 19 次失败（跨 IP，不触发任一维度）
	for i := 0; i < 19; i++ {
		key := fmt.Sprintf("root|10.1.0.%d", i)
		s.Login("root", "wrong", key)
	}
	if _, _, err := s.Login("root", "password123", "root|10.1.0.100"); err != nil {
		t.Fatalf("阈值内正确密码应可登录: %v", err)
	}
	// 成功清零后，再失败若干次不应立即被限速
	if _, _, err := s.Login("root", "wrong", "root|10.1.0.101"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("成功登录后计数应清零，实际 %v", err)
	}
}

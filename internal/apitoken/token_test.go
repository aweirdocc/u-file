package apitoken

import (
	"errors"
	"testing"
)

func TestCreateVerifyAndRevokeToken(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	tok, plain, err := mgr.Create(CreateOptions{
		Name:           "CI Upload Token",
		CreatedBy:      "admin",
		Scopes:         []Scope{ScopeFilesWrite},
		AllowedSources: []string{"oss-prod"},
		IPWhitelist:    []string{"127.0.0.1", "10.0.0.0/8"},
		TTLHours:       24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !IsAPIToken(plain) {
		t.Fatalf("expected uft_ prefix, got %q", plain)
	}
	// 自动补齐 files:read
	if !tok.HasScope(ScopeFilesRead) || !tok.HasScope(ScopeFilesWrite) {
		t.Fatalf("expected files:read and files:write, got %v", tok.Scopes)
	}
	if !tok.AllowsSource("oss-prod") || tok.AllowsSource("other-src") {
		t.Fatalf("allowed sources check failed")
	}

	// 1. 合法 IP 验证通过
	verified, err := mgr.Verify(plain, "10.23.45.67")
	if err != nil || verified.ID != tok.ID {
		t.Fatalf("verify failed: %v", err)
	}

	// 2. 非白名单 IP 拒绝
	if _, err := mgr.Verify(plain, "192.168.1.5"); !errors.Is(err, ErrIPForbidden) {
		t.Fatalf("expected ErrIPForbidden, got %v", err)
	}

	// 3. 重启持久化恢复验证
	mgr2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr2.Verify(plain, "127.0.0.1"); err != nil {
		t.Fatalf("verify after reload failed: %v", err)
	}

	// 4. 吊销后拒绝
	if err := mgr2.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr2.Verify(plain, "127.0.0.1"); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("expected ErrTokenRevoked, got %v", err)
	}
}

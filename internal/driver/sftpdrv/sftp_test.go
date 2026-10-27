package sftpdrv

import (
	"testing"

	"ufile/internal/driver"
)

// 本包单测聚焦：配置解析、认证方式组装、路径与错误分类。
// 真实 SFTP 服务器集成由 TestSFTPLiveSmoke 承担（环境变量门控）：
// TEST_SFTP_ADDR=127.0.0.1:22 TEST_SFTP_USER=u TEST_SFTP_PASS=p [TEST_SFTP_KEY=<path>]

func TestNewValidation(t *testing.T) {
	if _, err := New(map[string]any{}); err == nil {
		t.Fatal("缺少 host 应报错")
	}
	if _, err := New(map[string]any{"host": "h"}); err == nil {
		t.Fatal("缺少 username 应报错")
	}
	if _, err := New(map[string]any{"host": "h", "username": "u"}); err == nil {
		t.Fatal("password/private_key 至少其一")
	}
	d, err := New(map[string]any{
		"host": "127.0.0.1", "port": 2222, "username": "u", "password": "p", "root": "/srv/",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := d.(*SFTP)
	if f.port != 2222 || f.root != "srv/" || f.password != "p" {
		t.Fatalf("配置解析不符: %+v", f)
	}
	if !f.Capabilities().Write {
		t.Fatal("SFTP 驱动应声明 Write 能力")
	}
}

func TestAuthMethods(t *testing.T) {
	d, _ := New(map[string]any{"host": "h", "username": "u", "password": "p"})
	f := d.(*SFTP)
	ms, err := f.authMethods()
	if err != nil || len(ms) != 1 {
		t.Fatalf("密码认证: %v %d", err, len(ms))
	}
	// 私钥（文件路径形态）可用：用测试生成的临时 PEM
	dir := t.TempDir()
	keyPath := dir + "/id_ed25519"
	// 快速生成一个可解析的 ed25519 私钥 PEM（不依赖 ssh-keygen）
	if err := writeTestKey(keyPath); err != nil {
		t.Skipf("生成测试私钥失败: %v", err)
	}
	d2, _ := New(map[string]any{"host": "h", "username": "u", "private_key": keyPath})
	f2 := d2.(*SFTP)
	ms2, err := f2.authMethods()
	if err != nil || len(ms2) != 1 {
		t.Fatalf("私钥认证: %v %d", err, len(ms2))
	}
	// PEM 内容直传
	d3, _ := New(map[string]any{"host": "h", "username": "u", "private_key": readAll(keyPath)})
	if _, err := d3.(*SFTP).authMethods(); err != nil {
		t.Fatalf("PEM 内容认证: %v", err)
	}
	// 坏私钥应报错
	d4, _ := New(map[string]any{"host": "h", "username": "u", "private_key": "not a key"})
	if _, err := d4.(*SFTP).authMethods(); err == nil {
		t.Fatal("坏私钥应报错")
	}
}

func TestFullPath(t *testing.T) {
	d, _ := New(map[string]any{"host": "h", "username": "u", "password": "p", "root": "base"})
	f := d.(*SFTP)
	got, err := f.full("docs/a.txt")
	if err != nil || got != "/base/docs/a.txt" {
		t.Fatalf("full: %q err=%v", got, err)
	}
	if _, err := f.full(".."); err == nil {
		t.Fatal(".. 应被拒绝")
	}
}

func TestHostKeyPolicy(t *testing.T) {
	d, _ := New(map[string]any{"host": "h", "username": "u", "password": "p"})
	f := d.(*SFTP)
	cb := f.hostKeyCallback()
	// 首次接受任意密钥（accept-new 语义）
	if err := cb("h1", nil, fakeKey()); err != nil {
		t.Fatalf("首次应接受: %v", err)
	}
	// 同主机同密钥放行
	if err := cb("h1", nil, fakeKey()); err != nil {
		t.Fatalf("同密钥应放行: %v", err)
	}
	// 同主机密钥变了 → 拒绝
	if err := cb("h1", nil, fakeKey2()); err == nil {
		t.Fatal("主机密钥变更应拒绝")
	}
	// 其他主机首次接受
	if err := cb("h2", nil, fakeKey2()); err != nil {
		t.Fatalf("新主机首次应接受: %v", err)
	}
}

func TestSchemeAndRegistration(t *testing.T) {
	d, _ := New(map[string]any{"host": "h", "username": "u", "password": "p"})
	if d.Scheme() != "sftp" {
		t.Fatalf("Scheme=%q", d.Scheme())
	}
	if _, err := driver.New("sftp", map[string]any{"host": "x", "username": "u", "password": "p"}); err != nil {
		t.Fatalf("driver.New(sftp): %v", err)
	}
}

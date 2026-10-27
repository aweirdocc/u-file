package ftp

import (
	"net/textproto"
	"testing"

	"ufile/internal/driver"
)

// 本包单测聚焦：配置解析、路径处理与能力位。
// 需要真实 FTP 服务器的集成行为由 TestFTPLiveSmoke 承担（环境变量门控），
// 与 s3 驱动的 TEST_S3_* 门控模式一致：TEST_FTP_ADDR=127.0.0.1:2121 TEST_FTP_USER=u TEST_FTP_PASS=p

func TestNewValidation(t *testing.T) {
	if _, err := New(map[string]any{}); err == nil {
		t.Fatal("缺少 host 应报错")
	}
	d, err := New(map[string]any{
		"host": "127.0.0.1", "port": 2121, "username": "u", "password": "p", "root": "/srv/files/",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := d.(*FTP)
	if f.port != 2121 || f.root != "srv/files/" {
		t.Fatalf("配置解析不符: port=%d root=%q", f.port, f.root)
	}
	if f.user != "u" || f.pass != "p" {
		t.Fatal("凭据解析不符")
	}
	if f.tls {
		t.Fatal("默认应为明文 FTP")
	}
	caps := f.Capabilities()
	if !caps.List || !caps.Download || !caps.Write {
		t.Fatalf("能力位不符: %+v", caps)
	}
}

func TestFullPath(t *testing.T) {
	d, _ := New(map[string]any{"host": "h", "root": "base"})
	f := d.(*FTP)
	got, err := f.full("docs/a.txt")
	if err != nil || got != "/base/docs/a.txt" {
		t.Fatalf("full: %q err=%v", got, err)
	}
	got, err = f.full("")
	if err != nil || got != "/base/" {
		t.Fatalf("full(root): %q err=%v", got, err)
	}
	if _, err := f.full("../escape"); err == nil {
		t.Fatal(".. 应被拒绝")
	}
	if _, err := f.full(`a\b`); err == nil {
		t.Fatal("反斜杠应被拒绝")
	}
}

func TestSchemeAndRegistration(t *testing.T) {
	d, _ := New(map[string]any{"host": "h"})
	if d.Scheme() != "ftp" {
		t.Fatalf("Scheme=%q", d.Scheme())
	}
	// 注册表能按类型实例化（all 包导入后 init 生效由全局测试覆盖）
	if _, err := driver.New("ftp", map[string]any{"host": "x"}); err != nil {
		t.Fatalf("driver.New(ftp): %v", err)
	}
}

func TestMax0(t *testing.T) {
	if max0(-5) != 0 || max0(0) != 0 || max0(7) != 7 {
		t.Fatal("max0 语义错误")
	}
}

func TestIsNotFoundClassification(t *testing.T) {
	if isNotFound(nil) {
		t.Fatal("nil 不应判为 not found")
	}
	// FTP 服务器 550/450 应判为 not found（textproto.Error 通道）
	if !isNotFound(&textproto.Error{Code: 550, Msg: "No such file"}) {
		t.Fatal("550 应判为 not found")
	}
	if isNotFound(&textproto.Error{Code: 530, Msg: "login incorrect"}) {
		t.Fatal("530 不应判为 not found")
	}
}

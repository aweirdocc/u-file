package crypt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	c, err := New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "hello", "AKID64rUuARdeWO8j9apFulsy263qTvCIG2n", "含中文的凭据🔐"} {
		ct, err := c.Seal(plain)
		if err != nil {
			t.Fatalf("Seal(%q): %v", plain, err)
		}
		if plain == "" {
			if ct != "" {
				t.Fatalf("空串应原样返回，得到 %q", ct)
			}
			continue
		}
		if !IsEncrypted(ct) {
			t.Fatalf("密文应有 enc:v1: 前缀：%q", ct)
		}
		got, err := c.Open(ct)
		if err != nil || got != plain {
			t.Fatalf("round trip %q → %q, err=%v", plain, got, err)
		}
		// 相同明文两次加密应产生不同密文（随机 nonce）
		ct2, _ := c.Seal(plain)
		if ct2 == ct {
			t.Fatal("相同明文两次加密不应产生相同密文")
		}
	}
}

func TestOpenPlaintextPassthrough(t *testing.T) {
	c, _ := New(make([]byte, 32))
	// 非密文前缀的值视为明文原样返回 —— 旧 sources.json 渐进迁移依赖此行为
	got, err := c.Open("plain-secret")
	if err != nil || got != "plain-secret" {
		t.Fatalf("明文应原样通过，got=%q err=%v", got, err)
	}
}

func TestOpenWrongKey(t *testing.T) {
	c1, _ := New([]byte("11111111111111111111111111111111"))
	c2, _ := New([]byte("22222222222222222222222222222222"))
	ct, _ := c1.Seal("secret")
	if _, err := c2.Open(ct); err == nil {
		t.Fatal("换密钥解密应报错")
	}
	// 篡改密文
	if _, err := c1.Open(ct[:len(ct)-4] + "AAAA"); err == nil {
		t.Fatal("篡改后的密文解密应报错（GCM 完整性）")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	dir := t.TempDir()
	k1, created, err := LoadOrCreateKey(dir)
	if err != nil || !created {
		t.Fatalf("首次应生成密钥: created=%v err=%v", created, err)
	}
	if len(k1) != 32 {
		t.Fatalf("密钥应为 32 字节，实际 %d", len(k1))
	}
	// 文件权限（POSIX 生效；Windows 忽略）
	if info, err := os.Stat(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Logf("提示：密钥文件权限 %v（Windows 上不强制）", info.Mode().Perm())
	}
	// 第二次读取应拿到同一把
	k2, created, err := LoadOrCreateKey(dir)
	if err != nil || created {
		t.Fatalf("第二次应读取而非重新生成: created=%v err=%v", created, err)
	}
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Fatal("两次读取的密钥不一致")
		}
	}
	// 内容损坏应报错而非静默换钥匙（换钥匙会导致旧密文全部解不开）
	if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte("not-base64!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("损坏的密钥文件应报错")
	}
}

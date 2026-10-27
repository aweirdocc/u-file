package sftpdrv

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeTestKey 生成 ed25519 私钥 PKCS#8 PEM 文件（ssh.ParsePrivateKey 支持）。
func writeTestKey(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func readAll(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// fakeKey/fakeKey2 各生成一次并缓存（同函数返回同一密钥）。
var (
	cachedKey  ssh.PublicKey
	cachedKey2 ssh.PublicKey
)

func fakeKey() ssh.PublicKey {
	if cachedKey == nil {
		cachedKey = genKey()
	}
	return cachedKey
}

func fakeKey2() ssh.PublicKey {
	if cachedKey2 == nil {
		cachedKey2 = genKey()
	}
	return cachedKey2
}

// genKey 生成真实的 ed25519 公钥（ssh.NewPublicKey 要求合法曲线点）。
func genKey() ssh.PublicKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		panic(err)
	}
	return pub
}

var _ = testing.Short

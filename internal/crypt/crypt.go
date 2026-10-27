// Package crypt 凭据落库加密：AES-256-GCM + 本地主密钥文件。
//
// 设计（对应 docs/04-总体架构设计 安全基线）：
//   - 主密钥为 32 字节随机数，存于 data/master.key（0600，首次使用时自动生成）；
//     主密钥丢失则已加密凭据不可恢复，启动日志会提示备份；
//   - 密文格式 "enc:v1:<base64(nonce|ciphertext)>"，前缀用于在 JSON 中
//     与明文值区分，兼容渐进迁移（旧明文读到内存后，下次保存即加密）；
//   - 只加密敏感字段（access_key/secret_key/password），endpoint/bucket 等
//     非敏感配置保持可读，便于人工排查与手工编辑。
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prefix 密文值前缀（版本化，便于未来换算法/密钥时识别）。
const Prefix = "enc:v1:"

var ErrBadCiphertext = errors.New("crypt: 密文格式非法或主密钥不匹配")

// Cipher 一个绑定主密钥的 AES-256-GCM 实例。并发安全。
type Cipher struct {
	aead cipher.AEAD
}

// New 用 32 字节主密钥构造。
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypt: 主密钥必须为 32 字节，实际 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal 加密为 "enc:v1:<b64>"。空串原样返回（无凭据是合法状态）。
func (c *Cipher) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return Prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open 解密 "enc:v1:<b64>"。非本前缀的值视为明文原样返回（兼容迁移）。
func (c *Cipher) Open(val string) (string, error) {
	if !strings.HasPrefix(val, Prefix) {
		return val, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(val, Prefix))
	if err != nil {
		return "", ErrBadCiphertext
	}
	if len(raw) < c.aead.NonceSize() {
		return "", ErrBadCiphertext
	}
	plain, err := c.aead.Open(nil, raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():], nil)
	if err != nil {
		return "", ErrBadCiphertext
	}
	return string(plain), nil
}

// IsEncrypted 判断值是否为密文。
func IsEncrypted(val string) bool { return strings.HasPrefix(val, Prefix) }

// LoadOrCreateKey 读取（或首次生成）data 目录下的主密钥文件。
// 文件权限 0600；生成时返回 created=true，调用方应提示用户备份。
func LoadOrCreateKey(dataDir string) (key []byte, created bool, err error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dataDir, "master.key")
	if raw, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, false, fmt.Errorf("crypt: %s 内容非法（应为 32 字节密钥的 base64）", path)
		}
		return key, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	enc := base64.StdEncoding.EncodeToString(key)
	// Windows 不支持 0600 的完整语义，WriteFile 尽力设置；失败不阻断
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

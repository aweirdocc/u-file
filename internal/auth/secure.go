package auth

// 登录密码信封加密：服务端持有 RSA 私钥，浏览器用公钥加密后提交，
// 密码不以防窃听形态出现在网络/日志中。
//
// 边界（务必知晓）：应用层加密无法防御主动 MITM（攻击者可替换公钥），
// 生产部署仍应启用 HTTPS/TLS；此处解决的是明文密码裸奔与日志泄露。
//
// 密文载荷：base64(RSA-OAEP-SHA256(JSON{"p":密码,"ts":Unix 秒}))
// ts 用于拒绝重放（容忍 ±5 分钟时钟偏差）。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrBadCipher 密文无效（格式/解密/超时）。
var ErrBadCipher = errors.New("auth: 密码密文无效或已过期")

// replayWindow 密文时间戳容忍窗口。
const replayWindow = 5 * time.Minute

// loginEnvelope RSA-OAEP 加密前的明文载荷。
type loginEnvelope struct {
	P  string `json:"p"`  // 密码
	TS int64  `json:"ts"` // Unix 秒
}

// loadOrCreateLoginKey 读取（或生成）data/login.key：RSA-2048 私钥，
// PKCS#8 PEM，0600。仅用于登录信封加解密，与凭据主密钥/JWT 密钥相互独立。
func loadOrCreateLoginKey(dataDir string) (*rsa.PrivateKey, error) {
	path := filepath.Join(dataDir, "login.key")
	if raw, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(raw); blk != nil {
			if key, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
				if rk, ok := key.(*rsa.PrivateKey); ok {
					return rk, nil
				}
			}
		}
		// 文件损坏：旧密文只用于瞬时登录（≤5 分钟窗口），直接重建无迁移负担
		fmt.Fprintf(os.Stderr, "[warn] %s 解析失败，已重新生成登录密钥\n", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("auth: 写入登录密钥失败: %w", err)
	}
	return key, nil
}

// publicKeyPEM 导出 SPKI 公钥 PEM（前端 WebCrypto importKey('spki')）。
func publicKeyPEM(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// DecryptPassword 解密登录信封，返回明文密码。
func (s *Store) DecryptPassword(encB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return "", ErrBadCipher
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, s.loginKey, raw, nil)
	if err != nil {
		return "", ErrBadCipher
	}
	var env loginEnvelope
	if err := json.Unmarshal(plain, &env); err != nil || env.P == "" {
		return "", ErrBadCipher
	}
	ts := time.Unix(env.TS, 0)
	if d := time.Since(ts); d > replayWindow || d < -replayWindow {
		return "", ErrBadCipher
	}
	return env.P, nil
}

// PublicKeyPEM 返回登录加密公钥。
func (s *Store) PublicKeyPEM() []byte { return s.pubPEM }

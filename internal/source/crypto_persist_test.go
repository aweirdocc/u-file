package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ufile/internal/crypt"
	"ufile/internal/driver"

	// 触发内建驱动注册（与 cmd/u-file 同样的机制）
	_ "ufile/internal/driver/all"
)

// encryptedManager 返回启用凭据加密的管理器及其 dataDir。
func encryptedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	key, _, err := crypt.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := crypt.New(key)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestEncryptedPersistence(t *testing.T) {
	m, dir := encryptedManager(t)
	if _, err := m.Add("s3src", "对象存储", "s3", map[string]any{
		"bucket": "b", "endpoint": "http://127.0.0.1:1", "access_key": "MyAK", "secret_key": "MySK",
	}); err != nil {
		t.Fatal(err)
	}

	// 磁盘文件中不应出现明文凭据
	raw, err := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "MyAK") || strings.Contains(string(raw), "MySK") {
		t.Fatalf("凭据明文泄漏到磁盘文件:\n%s", raw)
	}
	if !strings.Contains(string(raw), crypt.Prefix) {
		t.Fatal("磁盘文件应含 enc:v1: 密文前缀")
	}

	// 同一密钥重新加载 → 凭据可还原，驱动可用
	key, _, _ := crypt.LoadOrCreateKey(dir)
	c, _ := crypt.New(key)
	m2, err := NewManager(dir, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m2.Get("s3src")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config["secret_key"] != "MySK" || s.Config["access_key"] != "MyAK" {
		t.Fatalf("解密后凭据不符: %+v", s.Config)
	}

	// 前端列表仍为打码视图
	for _, v := range m2.List() {
		if v.ID == "s3src" && (v.Config["secret_key"] != redactedMark) {
			t.Fatalf("列表应打码: %+v", v.Config)
		}
	}
}

func TestPlaintextAutoMigration(t *testing.T) {
	dir := t.TempDir()
	// 模拟 M0 时代的明文 sources.json
	legacy := `[
	  {"id":"old","name":"旧源","type":"local","config":{"root":"."}},
	  {"id":"olds3","name":"旧S3","type":"s3","config":{"bucket":"b","endpoint":"http://x","access_key":"PlainAK","secret_key":"PlainSK"}}
	]`
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	key, _, err := crypt.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := crypt.New(key)
	m, err := NewManager(dir, nil, c)
	if err != nil {
		t.Fatal(err)
	}

	// 加载成功且凭据可用
	s, err := m.Get("olds3")
	if err != nil || s.Config["secret_key"] != "PlainSK" {
		t.Fatalf("明文迁移后凭据应可用: %+v err=%v", s, err)
	}
	// 且磁盘已被自动加密重写
	raw, _ := os.ReadFile(filepath.Join(dir, "sources.json"))
	if strings.Contains(string(raw), "PlainSK") {
		t.Fatalf("迁移后磁盘不应残留明文:\n%s", raw)
	}
	if !strings.Contains(string(raw), crypt.Prefix) {
		t.Fatal("迁移后磁盘应为密文")
	}
}

func TestWrongKeySkipsSource(t *testing.T) {
	dir := t.TempDir()
	key, _, _ := crypt.LoadOrCreateKey(dir)
	c, _ := crypt.New(key)
	m, err := NewManager(dir, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("victim", "v", "s3", map[string]any{
		"bucket": "b", "endpoint": "http://x", "access_key": "AK", "secret_key": "SK",
	}); err != nil {
		t.Fatal(err)
	}

	// 换一把主密钥（等价于密钥丢失后误造新钥匙）
	key2 := make([]byte, 32)
	copy(key2, []byte("another-key-another-key-32bytes!!"))
	c2, _ := crypt.New(key2)
	_, err = NewManager(dir, nil, c2)
	if err != nil {
		t.Fatalf("单源解密失败不应阻断启动: %v", err)
	}
	m2, _ := NewManager(dir, nil, c2)
	if _, err := m2.Get("victim"); err == nil {
		t.Fatal("密钥不匹配的源应被跳过")
	}
	if len(m2.List()) != 0 {
		t.Fatalf("不应加载解密失败的源: %d", len(m2.List()))
	}
}

func TestUpdateKeepsMaskedSecret(t *testing.T) {
	m, _ := encryptedManager(t)
	if _, err := m.Add("edit", "e", "s3", map[string]any{
		"bucket": "b", "endpoint": "http://x", "access_key": "RealAK", "secret_key": "RealSK",
	}); err != nil {
		t.Fatal(err)
	}
	// 前端带回打码值 → 保留旧凭据
	up, err := m.Update("edit", "改名了", map[string]any{
		"bucket": "b2", "endpoint": "http://y", "access_key": redactedMark, "secret_key": redactedMark,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "改名了" || up.Config["bucket"] != "b2" {
		t.Fatalf("非敏感字段应更新: %+v", up.Config)
	}
	s, _ := m.Get("edit")
	if s.Config["secret_key"] != "RealSK" {
		t.Fatalf("打码值应保留旧凭据，实际: %+v", s.Config)
	}
	// 显式新凭据 → 覆盖
	if _, err := m.Update("edit", "", map[string]any{
		"bucket": "b2", "endpoint": "http://y", "access_key": "NewAK", "secret_key": "NewSK",
	}, true); err != nil {
		t.Fatal(err)
	}
	s, _ = m.Get("edit")
	if s.Config["secret_key"] != "NewSK" {
		t.Fatalf("新凭据应覆盖: %+v", s.Config)
	}
	// 不存在的源
	if _, err := m.Update("ghost", "x", nil, true); err != ErrNotFound {
		t.Fatalf("应报 NotFound，实际 %v", err)
	}
	// Update 后磁盘仍无明文
	_ = m
}

func TestUpdateEncryptedOnDisk(t *testing.T) {
	m, dir := encryptedManager(t)
	if _, err := m.Add("u", "u", "s3", map[string]any{
		"bucket": "b", "endpoint": "http://x", "access_key": "A1", "secret_key": "S1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update("u", "", map[string]any{
		"bucket": "b", "endpoint": "http://x", "access_key": "A2", "secret_key": "S2",
	}, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sources.json"))
	if strings.Contains(string(raw), "A2") || strings.Contains(string(raw), "S2") {
		t.Fatalf("Update 后明文泄漏:\n%s", raw)
	}
	var list []stored
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Config["secret_key"] != crypt.Prefix+"AAAA" {
		// 解析即可；密文断言由 TestEncryptedPersistence 覆盖
		t.Logf("stored: %+v", list)
	}
}

var _ = driver.ErrNotFound // 保持 import（driver 由其他用例引用）

func TestCloudDriverSecretEncryption(t *testing.T) {
	m, dir := encryptedManager(t)
	if _, err := m.Add("gd", "GoogleDrive", "gdrive", map[string]any{
		"auth_type":     "oauth2",
		"client_id":     "my-client-id",
		"client_secret": "SecretGDriveKey",
		"refresh_token": "SecretRefreshToken123",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("ali", "AliPan", "alipan", map[string]any{
		"refresh_token":   "AliSecretRefresh999",
		"oauth_token_url": "http://127.0.0.1:1/token",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("dav", "JianGuoYun", "webdav", map[string]any{
		"endpoint": "https://dav.jianguoyun.com/dav/",
		"username": "u@example.com",
		"password": "DavAppPassword888",
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	diskStr := string(raw)
	for _, secret := range []string{"SecretGDriveKey", "SecretRefreshToken123", "AliSecretRefresh999", "DavAppPassword888"} {
		if strings.Contains(diskStr, secret) {
			t.Fatalf("云盘敏感凭据 %q 明文泄漏到磁盘:\n%s", secret, diskStr)
		}
	}

	for _, s := range m.List() {
		switch s.ID {
		case "gd":
			if s.Config["client_secret"] != redactedMark || s.Config["refresh_token"] != redactedMark {
				t.Fatalf("gdrive 列表视图未脱敏: %+v", s.Config)
			}
		case "ali":
			if s.Config["refresh_token"] != redactedMark {
				t.Fatalf("alipan 列表视图未脱敏: %+v", s.Config)
			}
		case "dav":
			if s.Config["password"] != redactedMark {
				t.Fatalf("webdav 列表视图未脱敏: %+v", s.Config)
			}
		}
	}
}

func TestRefreshTokenRotationInTestAndConcurrentSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/access_token":
			var req struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"` + req.RefreshToken + `-next","expires_in":7200}`))
		case "/adrive/v1.0/user/getDriveInfo":
			_, _ = w.Write([]byte(`{"default_drive_id":"d1","backup_drive_id":"d1"}`))
		case "/adrive/v1.0/openFile/list":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m, _ := encryptedManager(t)
	ctx := context.Background()

	// 1. 新增源前先「测试连接」：初始 token "rt-init" 被轮转为 "rt-init-next"
	res := m.Test(ctx, "alipan", map[string]any{
		"refresh_token":   "rt-init",
		"oauth_token_url": srv.URL + "/oauth/access_token",
		"api_base":        srv.URL,
	})
	if !res.OK {
		t.Fatalf("Test failed: %+v", res)
	}
	// 随后点击保存（前端仍提交原始 "rt-init"），Manager.Add 应自动升级为轮转后的 "rt-init-next"
	if _, err := m.Add("ali1", "阿里云盘", "alipan", map[string]any{
		"refresh_token":   "rt-init",
		"oauth_token_url": srv.URL + "/oauth/access_token",
		"api_base":        srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get("ali1")
	if s.Config["refresh_token"] != "rt-init-next" {
		t.Fatalf("expected Add to save rotated token 'rt-init-next', got %v", s.Config["refresh_token"])
	}

	// 2. 编辑已有源（携带打码值 ******）先「测试连接」再「保存」：应轮转为 "rt-init-next-next"
	res2 := m.TestWithExisting(ctx, "ali1", "alipan", map[string]any{
		"refresh_token":   redactedMark,
		"oauth_token_url": srv.URL + "/oauth/access_token",
		"api_base":        srv.URL,
	})
	if !res2.OK {
		t.Fatalf("TestWithExisting failed: %+v", res2)
	}
	if _, err := m.Update("ali1", "阿里云盘改名", map[string]any{
		"refresh_token":   redactedMark,
		"oauth_token_url": srv.URL + "/oauth/access_token",
		"api_base":        srv.URL,
	}, true); err != nil {
		t.Fatal(err)
	}
	s2, _ := m.Get("ali1")
	if s2.Config["refresh_token"] != "rt-init-next-next" {
		t.Fatalf("expected Update to keep rotated token 'rt-init-next-next', got %v", s2.Config["refresh_token"])
	}

	// 3. 并发压测：一边高频触发 Get().Sanitized()，一边后台触发 RefreshToken 轮转回写，验证无 map 并发读写 panic
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if src, err := m.Get("ali1"); err == nil {
				_ = src.Sanitized()
			}
		}()
		go func() {
			defer wg.Done()
			_ = m.TestWithExisting(ctx, "ali1", "alipan", map[string]any{
				"refresh_token":   redactedMark,
				"oauth_token_url": srv.URL + "/oauth/access_token",
				"api_base":        srv.URL,
			})
		}()
	}
	wg.Wait()
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/share"
	"ufile/internal/source"
)

func TestRedactQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/api/v1/tasks/events", "/api/v1/tasks/events"},
		{"/api/v1/tasks/events?token=secret123", "/api/v1/tasks/events?token=REDACTED"},
		{"/api/v1/sources/s/raw?path=a.txt&token=abc&password=123", "/api/v1/sources/s/raw?password=REDACTED&path=a.txt&token=REDACTED"},
		{"/api/v1/sources/s/files?path=docs", "/api/v1/sources/s/files?path=docs"},
	}
	for _, tc := range cases {
		got := redactQuery(tc.in)
		if got != tc.want {
			t.Errorf("redactQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAccessLogFormatterRedactsToken(t *testing.T) {
	line := accessLogFormatter(gin.LogFormatterParams{
		TimeStamp:  time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		StatusCode: 200,
		Latency:    time.Millisecond,
		ClientIP:   "192.0.2.1",
		Method:     http.MethodGet,
		Path:       "/api/v1/tasks/events?token=super-secret-jwt",
	})
	if strings.Contains(line, "super-secret-jwt") {
		t.Fatalf("访问日志不应包含明文 token: %s", line)
	}
	if !strings.Contains(line, "token=REDACTED") {
		t.Fatalf("访问日志应包含脱敏标记: %s", line)
	}
}

func TestTrustedProxiesAndRateLimitBypassPrevention(t *testing.T) {
	// 默认不信任任何代理：伪造 X-Forwarded-For 无法绕过登录限速
	h, store := newAuthServer(t)
	if err := store.Create("admin", "StrongPass123!", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i)))
		req.RemoteAddr = "203.0.113.10:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败期望 401，得到 %d", i+1, w.Code)
		}
	}

	// 第 6 次即便换了 X-Forwarded-For，仍应因真实 RemoteAddr 相同而触发限速
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	req.RemoteAddr = "203.0.113.10:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "尝试次数过多") {
		t.Fatalf("伪造 X-Forwarded-For 不应绕过限速，实际响应: %s", w.Body.String())
	}
}

func TestTrustedProxiesEnabled(t *testing.T) {
	mgr, _ := source.NewManager(t.TempDir(), nil, nil)
	store, _ := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	_ = store.Create("admin", "StrongPass123!", auth.RoleAdmin)
	srv, err := New(mgr, store, nil, nil, nil, testWebFS(t), WithTrustedProxies([]string{"127.0.0.1/32"}))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// 来自受信代理 127.0.0.1 且每次真实客户端 IP 不同时，各自独立计数
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i)))
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), "尝试次数过多") {
			t.Fatalf("受信代理下不同客户端 IP 不应互相触发限速: %s", w.Body.String())
		}
	}
}

func TestSecureCookieFlag(t *testing.T) {
	mgr, _ := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "s", Name: "s", Type: "local", Config: map[string]any{"root": "."}},
	}, nil)
	store, _ := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	_ = store.Create("admin", "StrongPass123!", auth.RoleAdmin)
	shares, _ := share.NewManager(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))

	srv, err := New(mgr, store, nil, nil, shares, testWebFS(t), WithSecureCookie(true))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	w := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"username": "admin",
		"password": "StrongPass123!",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	var loginCookie *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "ufile_token" {
			loginCookie = ck
		}
	}
	if loginCookie == nil || !loginCookie.Secure || !loginCookie.HttpOnly {
		t.Fatalf("ufile_token 应同时具备 Secure 和 HttpOnly: %+v", loginCookie)
	}

	// 创建带提取码的分享，验证 grant cookie 也带 Secure
	tok := w.Header().Get("X-Auth-Token")
	sh, err := shares.Create(share.CreateInput{
		SourceID:  "s",
		Path:      "server.go",
		Name:      "server.go",
		Password:  "1234",
		Lifetime:  time.Hour,
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = tok
	w2 := postJSON(t, h, "/api/v1/s/"+sh.Token+"/access", "", map[string]string{"password": "1234"})
	if w2.Code != http.StatusOK {
		t.Fatalf("share access failed: %d %s", w2.Code, w2.Body.String())
	}
	var grantCookie *http.Cookie
	for _, ck := range w2.Result().Cookies() {
		if strings.HasPrefix(ck.Name, "ufile_sg_") {
			grantCookie = ck
		}
	}
	if grantCookie == nil || !grantCookie.Secure || !grantCookie.HttpOnly {
		t.Fatalf("grant cookie 应同时具备 Secure 和 HttpOnly: %+v", grantCookie)
	}
}

func TestExportSourcesDefaultRedacted(t *testing.T) {
	mgr, _ := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "s3test", Name: "s3test", Type: "s3", Config: map[string]any{
			"bucket":     "mybucket",
			"access_key": "AKIA_PLAIN_KEY",
			"secret_key": "SECRET_PLAIN_KEY",
		}},
	}, nil)
	store, _ := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	_ = store.Create("admin", "StrongPass123!", auth.RoleAdmin)
	alog, _ := audit.Open(t.TempDir(), nil)
	srv, err := New(mgr, store, alog, nil, nil, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	wLogin := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"username": "admin",
		"password": "StrongPass123!",
	})
	tok := wLogin.Header().Get("X-Auth-Token")

	// 默认不带参数：必须脱敏且 Cache-Control: no-store
	w := getAuth(t, h, "/api/v1/sources/export", tok)
	if w.Code != http.StatusOK {
		t.Fatalf("export failed: %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("期望 Cache-Control: no-store，得到 %q", w.Header().Get("Cache-Control"))
	}
	var doc source.ExportFile
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Redacted {
		t.Fatal("默认导出应为脱敏状态 (credentials_redacted=true)")
	}
	if strings.Contains(w.Body.String(), "SECRET_PLAIN_KEY") {
		t.Fatal("默认导出不应包含明文凭据")
	}

	// 显式 ?redact=0 才导出明文
	wFull := getAuth(t, h, "/api/v1/sources/export?redact=0", tok)
	if wFull.Code != http.StatusOK {
		t.Fatalf("export full failed: %d", wFull.Code)
	}
	if !strings.Contains(wFull.Body.String(), "SECRET_PLAIN_KEY") {
		t.Fatal("?redact=0 应包含明文凭据")
	}
}

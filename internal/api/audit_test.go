package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/source"
)

func TestAuditLogEndpoint(t *testing.T) {
	dir := t.TempDir()
	mgr, err := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "src-a", Name: "源A", Type: "local", Config: map[string]any{"root": dir}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(mgr, store, alog, nil, nil, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// 1. 引导创建管理员
	w := postJSON(t, h, "/api/v1/auth/bootstrap", "", map[string]string{"username": "admin", "password": "password123"})
	adminTok := w.Header().Get("X-Auth-Token")

	// 2. 创建普通用户并登录
	w = postJSON(t, h, "/api/v1/auth/users", adminTok, map[string]any{
		"username": "bob",
		"password": "password123",
		"role":     "user",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create user failed: %d %s", w.Code, w.Body.String())
	}
	w = postJSON(t, h, "/api/v1/auth/login", "", map[string]string{"username": "bob", "password": "password123"})
	bobTok := w.Header().Get("X-Auth-Token")

	// 3. 执行一个文件目录创建操作产生 file.mkdir 审计
	postJSON(t, h, "/api/v1/sources/src-a/mkdir", adminTok, map[string]string{"path": "hello"})

	// 4. 非管理员访问审计接口应被拒绝（403）
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+bobTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. 管理员查询审计日志，应包含记录且倒序排列
	req = httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []audit.Entry `json:"items"`
		Total int           `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total < 4 || len(resp.Items) != resp.Total {
		t.Fatalf("expected at least 4 audit entries, got total=%d len=%d", resp.Total, len(resp.Items))
	}
	if resp.Items[0].Action != "file.mkdir" {
		t.Fatalf("expected newest entry to be file.mkdir, got %s", resp.Items[0].Action)
	}

	// 6. 按 action 和 q 过滤
	req = httptest.NewRequest(http.MethodGet, "/api/v1/audit?action=file.mkdir&q=hello", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on filtered query, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || resp.Items[0].Action != "file.mkdir" {
		t.Fatalf("expected 1 filtered file.mkdir entry, got %+v", resp)
	}
}

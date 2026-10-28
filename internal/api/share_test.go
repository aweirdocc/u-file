package api

// M3 分享外链端到端测试：创建 → 公开信息 → 下载计数 → 限次/过期 →
// 提取码（错误限速/正确放行）→ 撤销 → 源删除。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/share"
	"ufile/internal/source"
)

// newShareServer 带分享能力的测试服务器：local 源 + hello.txt + docs/ 目录。
func newShareServer(t *testing.T) (http.Handler, *share.Manager) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello share"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "s", Name: "s", Type: "local", Config: map[string]any{"root": dir}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	alog, _ := audit.Open(t.TempDir(), nil)
	shares, err := share.NewManager(t.TempDir(), []byte("test-grant-secret-32-bytes-xxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(mgr, store, alog, nil, shares, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), shares
}

// bootstrapAdmin 引导管理员并返回 token。
func bootstrapAdmin(t *testing.T, h http.Handler) string {
	t.Helper()
	w := postJSON(t, h, "/api/v1/auth/bootstrap", "", map[string]string{"username": "root", "password": "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("引导应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	return w.Header().Get("X-Auth-Token")
}

type shareView struct {
	ID          string `json:"id"`
	Token       string `json:"token"`
	Name        string `json:"name"`
	HasPassword bool   `json:"has_password"`
	Downloads   int64  `json:"downloads"`
}

func createShare(t *testing.T, h http.Handler, adminTok string, body map[string]any) shareView {
	t.Helper()
	w := postJSON(t, h, "/api/v1/shares", adminTok, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建分享应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var sv shareView
	if err := json.Unmarshal(w.Body.Bytes(), &sv); err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestShareManagementAuth(t *testing.T) {
	h, _ := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	// 未登录管理面 → 401
	if w := getAuth(t, h, "/api/v1/shares", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录列分享应 401，实际 %d", w.Code)
	}
	// 未登录创建 → 401
	if w := postJSON(t, h, "/api/v1/shares", "", map[string]any{"source_id": "s", "path": "hello.txt", "ttl_hours": 1}); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录创建分享应 401，实际 %d", w.Code)
	}
	// 分享源不存在 → 404
	w := postJSON(t, h, "/api/v1/shares", adminTok, map[string]any{"source_id": "nope", "path": "hello.txt", "ttl_hours": 1})
	if w.Code != http.StatusNotFound {
		t.Fatalf("分享不存在的源应 404，实际 %d", w.Code)
	}
	// 分享不存在的文件 → 404
	w = postJSON(t, h, "/api/v1/shares", adminTok, map[string]any{"source_id": "s", "path": "ghost.txt", "ttl_hours": 1})
	if w.Code != http.StatusNotFound {
		t.Fatalf("分享不存在的文件应 404，实际 %d", w.Code)
	}
	// TTL 超上限（>720h）→ 400
	w = postJSON(t, h, "/api/v1/shares", adminTok, map[string]any{"source_id": "s", "path": "hello.txt", "ttl_hours": 721})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("TTL 超上限应 400，实际 %d", w.Code)
	}
}

func TestShareFileDownloadFlow(t *testing.T) {
	h, shares := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	sv := createShare(t, h, adminTok, map[string]any{
		"source_id": "s", "path": "hello.txt", "ttl_hours": 24, "max_downloads": 2,
	})

	// 公开信息：无需登录
	w := get(t, h, "/api/v1/s/"+sv.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("公开信息应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var info map[string]any
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["name"] != "hello.txt" || info["has_password"] != false {
		t.Fatalf("公开信息不符: %s", w.Body.String())
	}

	// 公开下载成功，内容正确
	w = get(t, h, "/api/v1/s/"+sv.Token+"/download")
	if w.Code != http.StatusOK || w.Body.String() != "hello share" {
		t.Fatalf("公开下载应 200 + 正确内容，实际 %d: %s", w.Code, w.Body.String())
	}

	// 管理列表可见计数 +1
	w = getAuth(t, h, "/api/v1/shares", adminTok)
	var list []shareView
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Downloads != 1 {
		t.Fatalf("列表应含 1 条且计数 1: %s", w.Body.String())
	}

	// 再下 1 次到顶，第 3 次 → 410
	get(t, h, "/api/v1/s/"+sv.Token+"/download")
	if w = get(t, h, "/api/v1/s/"+sv.Token+"/download"); w.Code != http.StatusGone {
		t.Fatalf("超次下载应 410，实际 %d", w.Code)
	}
	if w = get(t, h, "/api/v1/s/"+sv.Token); w.Code != http.StatusGone {
		t.Fatalf("超次后信息应 410，实际 %d", w.Code)
	}

	// 撤销后 → 404
	if w = del(t, h, "/api/v1/shares/"+sv.ID, adminTok); w.Code != http.StatusOK {
		t.Fatalf("撤销应 200，实际 %d", w.Code)
	}
	if w = get(t, h, "/api/v1/s/"+sv.Token); w.Code != http.StatusNotFound {
		t.Fatalf("撤销后访问应 404，实际 %d", w.Code)
	}
	_ = shares
}

func TestShareDirectoryZipDownload(t *testing.T) {
	h, _ := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	sv := createShare(t, h, adminTok, map[string]any{"source_id": "s", "path": "docs", "ttl_hours": 24})

	// 目录分享 → zip
	w := get(t, h, "/api/v1/s/"+sv.Token+"/download")
	if w.Code != http.StatusOK {
		t.Fatalf("目录分享下载应 200，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("目录分享应为 zip，实际 %q", ct)
	}
	if !strings.Contains(w.Body.String(), "a.txt") {
		t.Fatalf("zip 内容应含 docs 下的 a.txt")
	}
	// 预览目录 → 400
	if w = get(t, h, "/api/v1/s/"+sv.Token+"/raw"); w.Code != http.StatusBadRequest {
		t.Fatalf("目录预览应 400，实际 %d", w.Code)
	}
}

func TestSharePasswordFlow(t *testing.T) {
	h, _ := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	sv := createShare(t, h, adminTok, map[string]any{
		"source_id": "s", "path": "hello.txt", "ttl_hours": 24, "password": "8888",
	})

	// 信息接口：has_password=true, unlocked=false
	w := get(t, h, "/api/v1/s/"+sv.Token)
	var info map[string]any
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["has_password"] != true || info["unlocked"] != false {
		t.Fatalf("带码分享信息不符: %s", w.Body.String())
	}

	// 未解锁直接下载 → 401
	if w = get(t, h, "/api/v1/s/"+sv.Token+"/download"); w.Code != http.StatusUnauthorized {
		t.Fatalf("未解锁下载应 401，实际 %d", w.Code)
	}

	// 错误码 5 次 → 第 6 次被限速 429
	for i := 0; i < 5; i++ {
		if w = postJSON(t, h, "/api/v1/s/"+sv.Token+"/access", "", map[string]string{"password": "0000"}); w.Code != http.StatusUnauthorized {
			t.Fatalf("错误提取码应 401（第 %d 次），实际 %d", i+1, w.Code)
		}
	}
	if w = postJSON(t, h, "/api/v1/s/"+sv.Token+"/access", "", map[string]string{"password": "0000"}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("连续错 6 次应 429，实际 %d", w.Code)
	}

	// 新 IP 不受影响：直接带正确码 → 200 + grant cookie
	req := httptest.NewRequest(http.MethodPost, "/api/v1/s/"+sv.Token+"/access", strings.NewReader(`{"password":"8888"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.9.9.9:1234"
	wr := httptest.NewRecorder()
	h.ServeHTTP(wr, req)
	if wr.Code != http.StatusOK {
		t.Fatalf("正确提取码应 200，实际 %d: %s", wr.Code, wr.Body.String())
	}
	var grant *http.Cookie
	for _, ck := range wr.Result().Cookies() {
		if strings.HasPrefix(ck.Name, "ufile_sg_") {
			grant = ck
		}
	}
	if grant == nil {
		t.Fatal("正确提取码后应下发 grant cookie")
	}

	// 带 grant cookie 下载成功；无码预览接口同样放行
	dl := func(url string, ck *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, url, nil)
		r.RemoteAddr = "10.9.9.9:1234"
		if ck != nil {
			r.AddCookie(ck)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w = dl("/api/v1/s/"+sv.Token+"/download", grant); w.Code != http.StatusOK || w.Body.String() != "hello share" {
		t.Fatalf("带 grant 下载应 200 + 正确内容，实际 %d: %s", w.Code, w.Body.String())
	}
	if w = dl("/api/v1/s/"+sv.Token+"/raw", grant); w.Code != http.StatusOK {
		t.Fatalf("带 grant 预览应 200，实际 %d", w.Code)
	}
	// 无 grant 预览仍 401
	if w = dl("/api/v1/s/"+sv.Token+"/raw", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 grant 预览应 401，实际 %d", w.Code)
	}
}

func TestShareExpiry(t *testing.T) {
	h, shares := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	sv := createShare(t, h, adminTok, map[string]any{"source_id": "s", "path": "hello.txt", "ttl_hours": 24})

	// 白盒把过期时间拨到过去，验证过期后全部端点 410
	if err := shares.ExpireForTest(sv.Token); err != nil {
		t.Fatalf("测试钩子失败: %v", err)
	}
	if w := get(t, h, "/api/v1/s/"+sv.Token); w.Code != http.StatusGone {
		t.Fatalf("过期分享应 410，实际 %d", w.Code)
	}
	if w := get(t, h, "/api/v1/s/"+sv.Token+"/download"); w.Code != http.StatusGone {
		t.Fatalf("过期下载应 410，实际 %d", w.Code)
	}
}

func TestShareSourceRemoved(t *testing.T) {
	h, _ := newShareServer(t)
	adminTok := bootstrapAdmin(t, h)

	sv := createShare(t, h, adminTok, map[string]any{"source_id": "s", "path": "hello.txt", "ttl_hours": 24})
	// 删除源
	if w := del(t, h, "/api/v1/sources/s", adminTok); w.Code != http.StatusOK {
		t.Fatalf("删除源应 200，实际 %d", w.Code)
	}
	// 分享信息仍可查（记录在），但下载报源不可用
	if w := get(t, h, "/api/v1/s/"+sv.Token+"/download"); w.Code != http.StatusNotFound {
		t.Fatalf("源删除后下载应 404，实际 %d", w.Code)
	}
}

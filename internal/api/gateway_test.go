package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"ufile/internal/apitoken"
	"ufile/internal/audit"
	"ufile/internal/driver"
	_ "ufile/internal/driver/local"
	"ufile/internal/fulltext"
	"ufile/internal/source"
)

func setupGatewayTestServer(t *testing.T, gatewaySecret string, requireGateway bool) (*Server, *audit.Log, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "files")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	auditDir := filepath.Join(tmp, "audit")
	alog, err := audit.Open(auditDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	srcs, err := source.NewManager(tmp, []source.Seed{
		{ID: "local", Name: "Local", Type: "local", Config: map[string]any{"root": root}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	tm, err := apitoken.NewManager(tmp)
	if err != nil {
		t.Fatal(err)
	}
	_, rawTok, err := tm.Create(apitoken.CreateOptions{
		Name:           "a-guard",
		CreatedBy:      "admin",
		Scopes:         []apitoken.Scope{apitoken.ScopeSourcesRead, apitoken.ScopeFilesRead, apitoken.ScopeFilesWrite, apitoken.ScopeFilesDelete},
		AllowedSources: []string{"local"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ft, err := fulltext.NewEngine(tmp)
	if err != nil {
		t.Fatal(err)
	}
	webFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}

	srv, err := New(
		srcs,
		nil,
		alog,
		nil,
		nil,
		webFS,
		WithAPITokens(tm),
		WithFullText(ft),
		WithGatewaySecret(gatewaySecret),
		WithRequireGatewayForAPIToken(requireGateway),
	)
	if err != nil {
		t.Fatal(err)
	}
	return srv, alog, rawTok, root, auditDir
}

func signGatewayReq(req *http.Request, secret, traceID, actor string, ts time.Time) {
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	sig := GatewaySignature(secret, tsStr, req.Method, req.URL.Path, traceID, actor)
	req.Header.Set("X-Gateway-Timestamp", tsStr)
	req.Header.Set("X-Gateway-Signature", sig)
	req.Header.Set("X-Trace-ID", traceID)
	req.Header.Set("X-Agent-Actor", actor)
}

func TestGatewayAuthAndTraceReconciliation(t *testing.T) {
	const secret = "gw-secret-test-123"
	srv, alog, rawTok, _, _ := setupGatewayTestServer(t, secret, true)

	// 1. 直连 API Token（无网关签名）必须被拒绝
	reqDirect := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local/files?path=", nil)
	reqDirect.Header.Set("Authorization", "Bearer "+rawTok)
	reqDirect.Header.Set("X-Trace-ID", "forged-trace")
	reqDirect.Header.Set("X-Agent-Actor", "forged-actor")
	wDirect := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wDirect, reqDirect)
	if wDirect.Code != http.StatusForbidden {
		t.Fatalf("expected 403 GATEWAY_REQUIRED for direct uft_ token, got %d body=%s", wDirect.Code, wDirect.Body.String())
	}

	// 2. 错误签名被拒绝
	reqBadSig := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local/files?path=", nil)
	reqBadSig.Header.Set("Authorization", "Bearer "+rawTok)
	reqBadSig.Header.Set("X-Gateway-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	reqBadSig.Header.Set("X-Gateway-Signature", "deadbeef")
	wBadSig := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wBadSig, reqBadSig)
	if wBadSig.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 GATEWAY_AUTH_FAILED, got %d", wBadSig.Code)
	}

	// 3. 合法网关签名放行，并记录可信 TraceID / AgentActor + 全量读审计
	for _, tc := range []struct {
		path    string
		traceID string
	}{
		{"/api/v1/sources/local/files?path=", "tr_list_01"},
		{"/api/v1/sources/local/stat?path=hello.txt", "tr_stat_01"},
		{"/api/v1/search?q=hello&source=local", "tr_search_02"},
		{"/api/v1/sources/local/download?path=hello.txt", "tr_dl_03"},
		{"/api/v1/sources/local/raw?path=hello.txt", "tr_raw_04"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+rawTok)
		signGatewayReq(req, secret, tc.traceID, "agent-reader", time.Now())
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s failed: %d %s", tc.path, w.Code, w.Body.String())
		}
	}

	entries, err := alog.Read(20)
	if err != nil {
		t.Fatal(err)
	}
	wantTraces := map[string]string{
		"file.list":     "tr_list_01",
		"file.stat":     "tr_stat_01",
		"file.search":   "tr_search_02",
		"file.download": "tr_dl_03",
		"file.raw":      "tr_raw_04",
	}
	for action, wantTrace := range wantTraces {
		found := false
		for _, e := range entries {
			if e.Action == action && e.TraceID == wantTrace && e.AgentActor == "agent-reader" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing audit entry for action=%s trace=%s; got=%+v", action, wantTrace, entries)
		}
	}
}

func TestGatewayBlocksDirectLinksAndPresign(t *testing.T) {
	const secret = "gw-secret-test-123"
	srv, _, rawTok, _, _ := setupGatewayTestServer(t, secret, true)

	for _, path := range []string{
		"/api/v1/sources/local/download?path=hello.txt&direct=1",
		"/api/v1/sources/local/presign?path=hello.txt",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+rawTok)
		signGatewayReq(req, secret, "tr_bypass", "agent-reader", time.Now())
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %s under gateway mode, got %d body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestGatewayRejectsSpoofedHeadersWithoutSecret(t *testing.T) {
	// 未配置网关签名时，直连请求伪造 X-Trace-ID / X-Agent-Actor 不得写入审计
	srv, alog, rawTok, _, _ := setupGatewayTestServer(t, "", false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local/files?path=", nil)
	req.Header.Set("Authorization", "Bearer "+rawTok)
	req.Header.Set("X-Trace-ID", "spoofed-trace")
	req.Header.Set("X-Agent-Actor", "spoofed-actor")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	entries, _ := alog.Read(10)
	for _, e := range entries {
		if e.TraceID != "" || e.AgentActor != "" {
			t.Fatalf("unverified headers leaked into audit entry: %+v", e)
		}
	}
}

func TestGatewayFailClosedAuditBlocksWrite(t *testing.T) {
	const secret = "gw-secret-test-123"
	srv, _, rawTok, root, auditDir := setupGatewayTestServer(t, secret, true)

	// 先创建一次审计文件，再将其设为只读以模拟审计落盘故障
	auditFile := filepath.Join(auditDir, "audit.log")
	if err := os.WriteFile(auditFile, []byte{}, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(auditFile, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(auditFile, 0o600)

	body, _ := json.Marshal(map[string]string{"path": "should-not-exist"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/local/mkdir", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawTok)
	req.Header.Set("Content-Type", "application/json")
	signGatewayReq(req, secret, "tr_mkdir_fail", "agent-coder", time.Now())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 AUDIT_UNAVAILABLE when audit log is unwritable, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("directory should NOT have been created when audit failed (err=%v)", err)
	}
	_ = driver.ErrInvalidPath
}

func TestGatewayUploadIfNotExistsPreventsOverwrite(t *testing.T) {
	const secret = "gw-secret-test-123"
	srv, _, rawTok, root, _ := setupGatewayTestServer(t, secret, true)

	makeUploadReq := func(filename, content, ifNotExists string) *http.Request {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if ifNotExists != "" {
			_ = mw.WriteField("if_not_exists", ifNotExists)
		}
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/local/upload", &buf)
		req.Header.Set("Authorization", "Bearer "+rawTok)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		signGatewayReq(req, secret, "tr_upload_01", "agent-coder", time.Now())
		return req
	}

	// 1. 网关默认开启 if_not_exists：尝试上传同名 hello.txt 应返回 409 Conflict 且不覆盖原内容
	w1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w1, makeUploadReq("hello.txt", "overwritten!", ""))
	if w1.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when overwriting existing file via gateway, got %d body=%s", w1.Code, w1.Body.String())
	}
	got, _ := os.ReadFile(filepath.Join(root, "hello.txt"))
	if string(got) != "hello world" {
		t.Fatalf("existing file was overwritten: %q", string(got))
	}

	// 2. 上传新文件 new.txt 应成功返回 200
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, makeUploadReq("new.txt", "brand new", ""))
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for new file upload, got %d body=%s", w2.Code, w2.Body.String())
	}
}

func TestGatewayUploadExpectedETag(t *testing.T) {
	const secret = "gw-secret-test-123"
	srv, _, rawTok, root, _ := setupGatewayTestServer(t, secret, true)

	// 先通过 stat 获取 hello.txt 的当前 ETag
	statReq := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local/stat?path=hello.txt", nil)
	statReq.Header.Set("Authorization", "Bearer "+rawTok)
	signGatewayReq(statReq, secret, "tr_stat_etag", "agent-coder", time.Now())
	statRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statRec, statReq)
	if statRec.Code != http.StatusOK {
		t.Fatalf("stat failed: %d %s", statRec.Code, statRec.Body.String())
	}
	var st driver.Entry
	if err := json.Unmarshal(statRec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}

	makeETagReq := func(etag, content string) *http.Request {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "hello.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		_ = mw.WriteField("expected_etag", etag)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/local/upload", &buf)
		req.Header.Set("Authorization", "Bearer "+rawTok)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		signGatewayReq(req, secret, "tr_upload_etag", "agent-coder", time.Now())
		return req
	}

	// 1. 传入 expected_etag（未显式传 if_not_exists=false）时，不应被网关默认 if_not_exists 遮蔽，
	//    ETag 不匹配应返回 409 PRECONDITION_FAILED（而非 ALREADY_EXISTS）且不覆盖原内容
	wBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wBad, makeETagReq("wrong-etag", "should not write"))
	if wBad.Code != http.StatusConflict {
		t.Fatalf("expected 409 for mismatched expected_etag, got %d body=%s", wBad.Code, wBad.Body.String())
	}
	var badResp struct {
		Items []struct {
			Code string `json:"code"`
		} `json:"items"`
	}
	if err := json.Unmarshal(wBad.Body.Bytes(), &badResp); err != nil {
		t.Fatal(err)
	}
	if len(badResp.Items) != 1 || badResp.Items[0].Code != "PRECONDITION_FAILED" {
		t.Fatalf("expected PRECONDITION_FAILED (not ALREADY_EXISTS), got %s", wBad.Body.String())
	}

	// 2. 若驱动支持 ETag，则匹配时应成功更新
	if st.ETag != "" {
		wOK := httptest.NewRecorder()
		srv.Handler().ServeHTTP(wOK, makeETagReq(st.ETag, "updated content"))
		if wOK.Code != http.StatusOK {
			t.Fatalf("expected 200 for matching expected_etag, got %d body=%s", wOK.Code, wOK.Body.String())
		}
		got, _ := os.ReadFile(filepath.Join(root, "hello.txt"))
		if string(got) != "updated content" {
			t.Fatalf("expected updated content, got %q", string(got))
		}
	}
}

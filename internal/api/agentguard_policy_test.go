package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"ufile/internal/apitoken"
	"ufile/internal/audit"
	"ufile/internal/auth"
	_ "ufile/internal/driver/local"
	"ufile/internal/source"
)

func setupPolicyTestServer(t *testing.T, gatewaySecret string) (*Server, *auth.Store, string, string) {
	t.Helper()
	tmp := t.TempDir()
	booksRoot := filepath.Join(tmp, "books")
	if err := os.MkdirAll(booksRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	srcs, err := source.NewManager(tmp, []source.Seed{
		{ID: "books", Name: "Books", Type: "local", Config: map[string]any{"root": booksRoot}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	authStore, err := auth.NewStore(tmp, []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}

	tm, err := apitoken.NewManager(tmp)
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(filepath.Join(tmp, "audit"), nil)
	if err != nil {
		t.Fatal(err)
	}

	webFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	srv, err := New(
		srcs,
		authStore,
		alog,
		nil,
		nil,
		webFS,
		WithAPITokens(tm),
		WithGatewaySecret(gatewaySecret),
		WithRequireGatewayForAPIToken(true),
	)
	if err != nil {
		t.Fatal(err)
	}

	h := srv.Handler()
	wBoot := postJSON(t, h, "/api/v1/auth/bootstrap", "", map[string]string{"username": "admin", "password": "AdminPass123!"})
	if wBoot.Code != http.StatusCreated && wBoot.Code != http.StatusOK {
		t.Fatalf("bootstrap failed: %d %s", wBoot.Code, wBoot.Body.String())
	}
	adminTok := wBoot.Header().Get("X-Auth-Token")

	wUser := postJSON(t, h, "/api/v1/auth/users", adminTok, map[string]any{
		"username": "agentuser",
		"password": "AgentPass123!",
		"role":     "user",
	})
	if wUser.Code != http.StatusCreated && wUser.Code != http.StatusOK {
		t.Fatalf("create user failed: %d %s", wUser.Code, wUser.Body.String())
	}
	userTok, _, err := authStore.Login("agentuser", "AgentPass123!", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return srv, authStore, adminTok, userTok
}

func assertStrictPolicySchema(t *testing.T, raw []byte) AgentGuardPolicy {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	allowedTop := map[string]bool{"version": true, "default_effect": true, "rules": true}
	for k := range top {
		if !allowedTop[k] {
			t.Fatalf("unexpected top-level field: %q", k)
		}
	}
	rulesRaw, _ := top["rules"].([]any)
	allowedRule := map[string]bool{
		"name": true, "effect": true, "operations": true, "path_patterns": true,
		"max_bytes": true, "blocked_extensions": true, "allowed_trust_levels": true, "reason": true,
	}
	for i, item := range rulesRaw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("rule[%d] is not object", i)
		}
		for k := range m {
			if !allowedRule[k] {
				t.Fatalf("rule[%d] has unexpected field %q", i, k)
			}
		}
	}
	var pol AgentGuardPolicy
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("unmarshal typed policy: %v", err)
	}
	if pol.Version != "1" || pol.DefaultEffect != "deny" {
		t.Fatalf("unexpected version/default_effect: %+v", pol)
	}
	return pol
}

func TestAgentGuardPolicyAuthAndETag(t *testing.T) {
	const secret = "gw-policy-secret-123"
	srv, _, adminTok, userTok := setupPolicyTestServer(t, secret)

	// 1. 匿名访问被拒绝
	reqAnon := httptest.NewRequest(http.MethodGet, "/api/v1/agentguard/policy", nil)
	wAnon := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wAnon, reqAnon)
	if wAnon.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous request, got %d", wAnon.Code)
	}

	// 2. 普通用户（非管理员、无网关签名）被拒绝
	reqUser := httptest.NewRequest(http.MethodGet, "/api/v1/agentguard/policy", nil)
	reqUser.Header.Set("Authorization", "Bearer "+userTok)
	wUser := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wUser, reqUser)
	if wUser.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin user, got %d", wUser.Code)
	}

	// 3. 管理员 JWT 可拉取策略并获得 ETag
	reqAdmin := httptest.NewRequest(http.MethodGet, "/api/v1/agentguard/policy", nil)
	reqAdmin.Header.Set("Authorization", "Bearer "+adminTok)
	wAdmin := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wAdmin, reqAdmin)
	if wAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin, got %d: %s", wAdmin.Code, wAdmin.Body.String())
	}
	etag := wAdmin.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected non-empty ETag header")
	}
	pol := assertStrictPolicySchema(t, wAdmin.Body.Bytes())
	if len(pol.Rules) != 4 {
		t.Fatalf("expected 4 default rules for books source, got %d", len(pol.Rules))
	}

	// 4. 网关签名请求 + If-None-Match 命中时返回 304
	reqGW304 := httptest.NewRequest(http.MethodGet, "/api/v1/agentguard/policy", nil)
	signGatewayReq(reqGW304, secret, "tr-sync-1", "a-guard-sync", time.Now())
	reqGW304.Header.Set("If-None-Match", etag)
	wGW304 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wGW304, reqGW304)
	if wGW304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", wGW304.Code)
	}
	if wGW304.Body.Len() != 0 {
		t.Fatalf("expected empty body on 304, got %d bytes", wGW304.Body.Len())
	}
}

func TestAgentGuardPolicyUpdatesOnSourceAndPermissionChanges(t *testing.T) {
	const secret = "gw-policy-secret-456"
	srv, _, adminTok, _ := setupPolicyTestServer(t, secret)

	fetchPolicy := func() (AgentGuardPolicy, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agentguard/policy", nil)
		signGatewayReq(req, secret, "tr-fetch", "a-guard", time.Now())
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("fetch policy failed: %d %s", w.Code, w.Body.String())
		}
		return assertStrictPolicySchema(t, w.Body.Bytes()), w.Header().Get("ETag")
	}

	_, etag1 := fetchPolicy()

	// 1. 新增文件源后策略与 ETag 自动更新
	wsRoot := t.TempDir()
	addBody, _ := json.Marshal(map[string]any{
		"id":     "workspace",
		"name":   "Workspace",
		"type":   "local",
		"config": map[string]any{"root": wsRoot},
	})
	reqAdd := httptest.NewRequest(http.MethodPost, "/api/v1/sources", bytes.NewReader(addBody))
	reqAdd.Header.Set("Authorization", "Bearer "+adminTok)
	reqAdd.Header.Set("Content-Type", "application/json")
	wAdd := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wAdd, reqAdd)
	if wAdd.Code != http.StatusCreated {
		t.Fatalf("add source failed: %d %s", wAdd.Code, wAdd.Body.String())
	}

	pol2, etag2 := fetchPolicy()
	if etag2 == etag1 {
		t.Fatal("expected ETag to change after adding source")
	}
	if len(pol2.Rules) != 7 {
		t.Fatalf("expected 7 rules for 2 sources, got %d", len(pol2.Rules))
	}

	// 2. 配置用户 ACL 策略并创建 API Token 后，策略按 Token Scope/AllowedSources 与 ACL 收敛
	aclBody, _ := json.Marshal(map[string]any{
		"policies": []auth.Policy{
			{SourceID: "books", Actions: []auth.Action{auth.ActionRead}, PathPrefixes: []string{"public"}},
		},
	})
	reqACL := httptest.NewRequest(http.MethodPut, "/api/v1/auth/users/agentuser/policies", bytes.NewReader(aclBody))
	reqACL.Header.Set("Authorization", "Bearer "+adminTok)
	reqACL.Header.Set("Content-Type", "application/json")
	wACL := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wACL, reqACL)
	if wACL.Code != http.StatusOK {
		t.Fatalf("update policies failed: %d %s", wACL.Code, wACL.Body.String())
	}

	// 创建仅授予 agentuser 读取 books 权限的 API Token
	_, _, err := srv.tokens.Create(apitoken.CreateOptions{
		Name:           "agent-read-books",
		CreatedBy:      "agentuser",
		Scopes:         []apitoken.Scope{apitoken.ScopeSourcesRead, apitoken.ScopeFilesRead},
		AllowedSources: []string{"books"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.invalidateAgentGuardPolicy()

	pol3, etag3 := fetchPolicy()
	if etag3 == etag2 {
		t.Fatal("expected ETag to change after permission/token update")
	}
	// 仅应包含 books 的 redact-books-reads 与 allow-list-and-stat，且路径收敛到 books/public/*
	if len(pol3.Rules) != 2 {
		t.Fatalf("expected 2 rules when restricted to read books/public, got %d: %+v", len(pol3.Rules), pol3.Rules)
	}
	if pol3.Rules[0].Name != "redact-books-reads" || len(pol3.Rules[0].PathPatterns) != 2 || pol3.Rules[0].PathPatterns[0] != "books/public/*" {
		t.Fatalf("unexpected read rule: %+v", pol3.Rules[0])
	}

	// 3. 删除源后策略再次更新
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/v1/sources/workspace", nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminTok)
	wDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete source failed: %d %s", wDel.Code, wDel.Body.String())
	}

	// 4. 修改用户 ACL 扩展写权限后 ETag 再次变化
	aclBody2, _ := json.Marshal(map[string]any{
		"policies": []auth.Policy{
			{SourceID: "books", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}, PathPrefixes: []string{"public", "workspace"}},
		},
	})
	reqACL2 := httptest.NewRequest(http.MethodPut, "/api/v1/auth/users/agentuser/policies", bytes.NewReader(aclBody2))
	reqACL2.Header.Set("Authorization", "Bearer "+adminTok)
	reqACL2.Header.Set("Content-Type", "application/json")
	wACL2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wACL2, reqACL2)
	if wACL2.Code != http.StatusOK {
		t.Fatalf("update policies 2 failed: %d %s", wACL2.Code, wACL2.Body.String())
	}

	pol4, etag4 := fetchPolicy()
	if etag4 == etag3 {
		t.Fatal("expected ETag to change after ACL update")
	}
	if len(pol4.Rules[0].PathPatterns) != 4 {
		t.Fatalf("expected 4 path patterns after adding workspace prefix, got %+v", pol4.Rules[0].PathPatterns)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAgentGuardPolicySyncNotify(t *testing.T) {
	const secret = "gw-policy-secret-notify"
	const adminSecret = "ag-admin-secret-notify"
	srv, _, adminTok, _ := setupPolicyTestServer(t, secret)

	ch := make(chan *http.Request, 1)
	var capturedBody []byte
	WithAgentGuardSyncNotify("http://127.0.0.1:8765", adminSecret, "ufile")(srv)
	srv.agSyncClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			capturedBody, _ = io.ReadAll(r.Body)
			ch <- r
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
				Header:     make(http.Header),
			}, nil
		}),
	}

	wsRoot := t.TempDir()
	addBody, _ := json.Marshal(map[string]any{
		"id":     "notes",
		"name":   "Notes",
		"type":   "local",
		"config": map[string]any{"root": wsRoot},
	})
	reqAdd := httptest.NewRequest(http.MethodPost, "/api/v1/sources", bytes.NewReader(addBody))
	reqAdd.Header.Set("Authorization", "Bearer "+adminTok)
	reqAdd.Header.Set("Content-Type", "application/json")
	wAdd := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wAdd, reqAdd)
	if wAdd.Code != http.StatusCreated {
		t.Fatalf("add source failed: %d %s", wAdd.Code, wAdd.Body.String())
	}

	select {
	case notifyReq := <-ch:
		if notifyReq.Method != http.MethodPost || notifyReq.URL.Path != "/_admin/policy/sync" {
			t.Fatalf("unexpected notify request: %s %s", notifyReq.Method, notifyReq.URL.String())
		}
		ts := notifyReq.Header.Get("X-Gateway-Timestamp")
		traceID := notifyReq.Header.Get("X-Trace-ID")
		actor := notifyReq.Header.Get("X-Agent-Actor")
		sig := notifyReq.Header.Get("X-Gateway-Signature")
		expected := GatewaySignature(adminSecret, ts, http.MethodPost, "/_admin/policy/sync", traceID, actor)
		if sig == "" || sig != expected {
			t.Fatalf("invalid notify signature: got %q want %q", sig, expected)
		}
		var body map[string]string
		if err := json.Unmarshal(capturedBody, &body); err != nil || body["app_id"] != "ufile" {
			t.Fatalf("unexpected notify body: %s", string(capturedBody))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async policy sync notification")
	}
}

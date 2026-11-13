package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ufile/internal/apitoken"
	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/driver"
	"ufile/internal/fulltext"
	"ufile/internal/mount"
	"ufile/internal/source"
	"ufile/internal/task"
	"ufile/internal/webhook"
)

func TestM4EndToEndAPI(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	dataDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dirA, "contract.txt"), []byte("年度云存储采购框架协议 2026 核心条款"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirA, "dept"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "dept", "report.md"), []byte("# 季度技术架构演进报告\n零落盘流式管道设计"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := source.NewManager(dataDir, []source.Seed{
		{ID: "src-a", Name: "源A", Type: "local", Config: map[string]any{"root": dirA}},
		{ID: "src-b", Name: "源B", Type: "local", Config: map[string]any{"root": dirB}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewStore(dataDir, []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	alog, _ := audit.Open(dataDir, nil)

	mntMgr, err := mount.NewManager(dataDir, func(id string) (driver.FSDriver, error) {
		src, err := mgr.Get(id)
		if err != nil {
			return nil, err
		}
		return src.Driver(), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	tokMgr, err := apitoken.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	whMgr, err := webhook.NewManager(dataDir, webhook.Config{AllowPrivateTargets: true})
	if err != nil {
		t.Fatal(err)
	}
	defer whMgr.Stop()

	ftEngine, err := fulltext.NewEngine(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	taskMgr, err := task.NewManager(dataDir, task.Config{}, func(id string) (driver.FSDriver, error) {
		if id == mount.VirtualSourceID {
			return mntMgr, nil
		}
		src, err := mgr.Get(id)
		if err != nil {
			return nil, err
		}
		return src.Driver(), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer taskMgr.Stop()

	srv, err := New(mgr, store, alog, nil, nil, testWebFS(t),
		WithTasks(taskMgr),
		WithAPITokens(tokMgr),
		WithWebhooks(whMgr),
		WithMounts(mntMgr),
		WithFullText(ftEngine),
	)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// 引导管理员
	w := postJSON(t, h, "/api/v1/auth/bootstrap", "", map[string]string{"username": "admin", "password": "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("bootstrap 失败: %d %s", w.Code, w.Body.String())
	}
	adminTok := w.Header().Get("X-Auth-Token")

	// —— 1. 测试 M4.1 跨源流式传输任务 ——
	w = postJSON(t, h, "/api/v1/tasks/transfer", adminTok, map[string]any{
		"type":             "copy",
		"source_id":        "src-a",
		"target_source_id": "src-b",
		"paths":            []string{"contract.txt", "dept"},
		"dst_dir":          "backup",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("提交传输任务应返回 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var createdTask task.Task
	_ = json.Unmarshal(w.Body.Bytes(), &createdTask)

	// 等待传输任务完成
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := taskMgr.Get(createdTask.ID)
		if err == nil && tk.Status == task.StatusSuccess {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dirB, "backup", "contract.txt")); err != nil {
		t.Fatalf("跨源复制目标文件未生成: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "backup", "dept", "report.md")); err != nil {
		t.Fatalf("跨源复制目标子目录文件未生成: %v", err)
	}

	// —— 2. 测试 M4.2 开放 API Token 细粒度鉴权 ——
	w = postJSON(t, h, "/api/v1/tokens", adminTok, map[string]any{
		"name":            "只读CI令牌",
		"scopes":          []string{"files:read", "sources:read"},
		"allowed_sources": []string{"src-a"},
		"ttl_hours":       24,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 API Token 应返回 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var tokResp struct {
		PlainToken string `json:"plain_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tokResp)
	if !strings.HasPrefix(tokResp.PlainToken, "uft_") {
		t.Fatalf("plain_token 应以 uft_ 开头: %s", tokResp.PlainToken)
	}

	// 用该只读 Token 读取允许源 src-a → 200
	w = getAuth(t, h, "/api/v1/sources/src-a/files?path=", tokResp.PlainToken)
	if w.Code != http.StatusOK {
		t.Fatalf("只读 Token 访问授权源 src-a 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	// 用该只读 Token 读取未授权源 src-b → 403
	w = getAuth(t, h, "/api/v1/sources/src-b/files?path=", tokResp.PlainToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("只读 Token 访问未授权源 src-b 应 403，实际 %d: %s", w.Code, w.Body.String())
	}
	// 用该只读 Token 执行写操作 mkdir → 403 SCOPE_FORBIDDEN
	w = postJSON(t, h, "/api/v1/sources/src-a/mkdir", tokResp.PlainToken, map[string]string{"path": "newdir"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("只读 Token 写操作应 403，实际 %d: %s", w.Code, w.Body.String())
	}

	// —— 3. 测试 M4.2 Webhook 创建与 Ping 测试 ——
	var pingReceived bool
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-UFile-Event") == webhook.EventPing {
			pingReceived = true
		}
		rw.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w = postJSON(t, h, "/api/v1/webhooks", adminTok, map[string]any{
		"name":    "测试钩子",
		"url":     ts.URL,
		"secret":  "sec123",
		"events":  []string{"*"},
		"enabled": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 Webhook 应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var whEp webhook.Endpoint
	_ = json.Unmarshal(w.Body.Bytes(), &whEp)

	w = postJSON(t, h, "/api/v1/webhooks/"+whEp.ID+"/test", adminTok, map[string]any{})
	if w.Code != http.StatusOK || !pingReceived {
		t.Fatalf("Webhook Ping 测试失败: code=%d pingReceived=%v body=%s", w.Code, pingReceived, w.Body.String())
	}

	// —— 4. 测试 M4.3 虚拟多挂载点树 (VFS) ——
	w = postJSON(t, h, "/api/v1/mounts", adminTok, map[string]any{
		"mount_path":  "/知识库/研发部",
		"source_id":   "src-a",
		"source_path": "dept",
		"readonly":    true,
		"remark":      "研发文档只读挂载",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建挂载点应 201，实际 %d: %s", w.Code, w.Body.String())
	}

	// 浏览虚拟源根目录 "@vfs" 应看到合成目录 "知识库"
	w = getAuth(t, h, "/api/v1/sources/@vfs/files?path=", adminTok)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "知识库") {
		t.Fatalf("浏览 @vfs 根应包含合成目录 '知识库'，实际 %d: %s", w.Code, w.Body.String())
	}

	// 浏览具体挂载点 "/知识库/研发部" 应看到 report.md
	w = getAuth(t, h, "/api/v1/sources/@vfs/files?path=知识库/研发部", adminTok)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "report.md") {
		t.Fatalf("浏览 @vfs/知识库/研发部 应看到 report.md，实际 %d: %s", w.Code, w.Body.String())
	}

	// 只读挂载点禁止 mkdir → 403 MOUNT_READONLY
	w = postJSON(t, h, "/api/v1/sources/@vfs/mkdir", adminTok, map[string]string{"path": "知识库/研发部/sub"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("只读挂载点写操作应返回 403，实际 %d: %s", w.Code, w.Body.String())
	}

	// —— 5. 测试 M4.3 全文检索引擎 ——
	srcA, _ := mgr.Get("src-a")
	ftEngine.IndexSource(context.Background(), "src-a", srcA.Driver())

	w = getAuth(t, h, "/api/v1/search?q=零落盘流式管道&mode=fulltext", adminTok)
	if w.Code != http.StatusOK {
		t.Fatalf("全文检索接口应返回 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "report.md") || !strings.Contains(w.Body.String(), "零落盘流式管道") {
		t.Fatalf("全文检索结果应包含 report.md 与命中摘要 snippet，实际: %s", w.Body.String())
	}
}

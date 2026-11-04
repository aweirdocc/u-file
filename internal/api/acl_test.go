package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/source"
)

func TestGranularACLAndMultiUserIsolation(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	mgr, err := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "src-a", Name: "源A", Type: "local", Config: map[string]any{"root": dir1}},
		{ID: "src-b", Name: "源B", Type: "local", Config: map[string]any{"root": dir2}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewStore(t.TempDir(), []byte("test-jwt-secret-32-bytes-xxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	alog, _ := audit.Open(t.TempDir(), nil)
	srv, err := New(mgr, store, alog, nil, nil, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// 1. 引导创建管理员并准备目录结构
	w := postJSON(t, h, "/api/v1/auth/bootstrap", "", map[string]string{"username": "admin", "password": "password123"})
	adminTok := w.Header().Get("X-Auth-Token")

	postJSON(t, h, "/api/v1/sources/src-a/mkdir", adminTok, map[string]string{"path": "dept1"})
	postJSON(t, h, "/api/v1/sources/src-a/mkdir", adminTok, map[string]string{"path": "dept2"})

	// 2. 创建普通协作用户 alice：仅授权 src-a 的 dept1 目录（read + write，无 delete）
	w = postJSON(t, h, "/api/v1/auth/users", adminTok, map[string]any{
		"username": "alice",
		"password": "password123",
		"role":     "user",
		"remark":   "部门1成员",
		"policies": []map[string]any{
			{
				"source_id":   "src-a",
				"actions":     []string{"read", "write"},
				"path_prefix": "dept1",
			},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 alice 应 201，实际 %d: %s", w.Code, w.Body.String())
	}

	w = postJSON(t, h, "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": "password123"})
	if w.Code != http.StatusOK {
		t.Fatalf("alice 登录失败: %d %s", w.Code, w.Body.String())
	}
	aliceTok := w.Header().Get("X-Auth-Token")

	// 3. 源级隔离：alice 列源只能看到 src-a，看不到 src-b
	w = getAuth(t, h, "/api/v1/sources", aliceTok)
	if w.Code != http.StatusOK {
		t.Fatalf("alice 列源失败: %d", w.Code)
	}
	var srcs []struct {
		ID     string `json:"id"`
		Access struct {
			Actions    []string `json:"actions"`
			PathPrefix string   `json:"path_prefix"`
		} `json:"access"`
	}
	json.Unmarshal(w.Body.Bytes(), &srcs)
	if len(srcs) != 1 || srcs[0].ID != "src-a" || srcs[0].Access.PathPrefix != "dept1" {
		t.Fatalf("alice 应仅看到 src-a (prefix=dept1)，实际: %s", w.Body.String())
	}

	// 直接请求未授权的 src-b → 403
	if w = getAuth(t, h, "/api/v1/sources/src-b/files", aliceTok); w.Code != http.StatusForbidden {
		t.Fatalf("alice 访问未授权源 src-b 应 403，实际 %d", w.Code)
	}

	// 4. 路径前缀隔离：alice 访问 src-a 的根目录或 dept2 应 403，访问 dept1 应 200
	if w = getAuth(t, h, "/api/v1/sources/src-a/files?path=", aliceTok); w.Code != http.StatusForbidden {
		t.Fatalf("alice 访问 src-a 根目录应 403，实际 %d", w.Code)
	}
	if w = getAuth(t, h, "/api/v1/sources/src-a/files?path=dept2", aliceTok); w.Code != http.StatusForbidden {
		t.Fatalf("alice 访问 src-a/dept2 应 403，实际 %d", w.Code)
	}
	if w = getAuth(t, h, "/api/v1/sources/src-a/files?path=dept1", aliceTok); w.Code != http.StatusOK {
		t.Fatalf("alice 访问 src-a/dept1 应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	// 5. 操作权限细分：alice 在 dept1 下有 write 权限（可 mkdir / rename），但无 delete 权限
	if w = postJSON(t, h, "/api/v1/sources/src-a/mkdir", aliceTok, map[string]string{"path": "dept1/sub"}); w.Code != http.StatusCreated {
		t.Fatalf("alice 在 dept1 下建目录应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	if w = postJSON(t, h, "/api/v1/sources/src-a/mkdir", aliceTok, map[string]string{"path": "dept2/sub"}); w.Code != http.StatusForbidden {
		t.Fatalf("alice 在 dept2 下建目录应 403，实际 %d", w.Code)
	}
	// 尝试通过 rename 移出授权目录 dept1 → 403
	if w = postJSON(t, h, "/api/v1/sources/src-a/rename", aliceTok, map[string]string{"from": "dept1/sub", "to": "dept2/sub"}); w.Code != http.StatusForbidden {
		t.Fatalf("alice 跨出前缀目录重命名应 403，实际 %d", w.Code)
	}
	// 在 dept1 内部重命名 → 200
	if w = postJSON(t, h, "/api/v1/sources/src-a/rename", aliceTok, map[string]string{"from": "dept1/sub", "to": "dept1/sub2"}); w.Code != http.StatusOK {
		t.Fatalf("alice 在授权目录内重命名应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	// 尝试删除 → 403（无 delete 权限）
	if w = postJSON(t, h, "/api/v1/sources/src-a/delete", aliceTok, map[string]any{"paths": []string{"dept1/sub2"}}); w.Code != http.StatusForbidden {
		t.Fatalf("alice 无 delete 权限删除应 403，实际 %d", w.Code)
	}

	// 6. 管理员动态追加 alice 的 delete 权限 → 立即生效（无需重登）
	w = putJSON(t, h, "/api/v1/auth/users/alice/policies", adminTok, map[string]any{
		"policies": []map[string]any{
			{
				"source_id":   "src-a",
				"actions":     []string{"read", "write", "delete"},
				"path_prefix": "dept1",
			},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新策略应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if w = postJSON(t, h, "/api/v1/sources/src-a/delete", aliceTok, map[string]any{"paths": []string{"dept1/sub2"}}); w.Code != http.StatusOK {
		t.Fatalf("追加 delete 权限后删除应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	// 7. 多目录树勾选（path_prefixes）：在 src-a 勾选 dept1 与 dept2，未勾选新目录 dept3
	postJSON(t, h, "/api/v1/sources/src-a/mkdir", adminTok, map[string]string{"path": "dept3"})
	w = putJSON(t, h, "/api/v1/auth/users/alice/policies", adminTok, map[string]any{
		"policies": []map[string]any{
			{
				"source_id":     "src-a",
				"actions":       []string{"read", "write"},
				"path_prefixes": []string{"dept1", "dept2", "dept1/sub"}, // dept1/sub 应自动收敛合并至 dept1
			},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("设置多目录策略应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	// 浏览根目录时仅可见勾选的 dept1 与 dept2，不可见 dept3
	w = getAuth(t, h, "/api/v1/sources/src-a/files?path=", aliceTok)
	if w.Code != http.StatusOK {
		t.Fatalf("多目录树勾选下浏览根应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var listRes struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	json.Unmarshal(w.Body.Bytes(), &listRes)
	if len(listRes.Entries) != 2 {
		t.Fatalf("根目录应仅过滤出 2 个已勾选目录(dept1, dept2)，实际 %d: %s", len(listRes.Entries), w.Body.String())
	}
	// 直接访问未勾选的 dept3 仍返回 403
	if w = getAuth(t, h, "/api/v1/sources/src-a/files?path=dept3", aliceTok); w.Code != http.StatusForbidden {
		t.Fatalf("访问未勾选的 dept3 应 403，实际 %d", w.Code)
	}

	// 8. 管理员禁用 alice → alice 的现有 token 立即 401
	w = putJSON(t, h, "/api/v1/auth/users/alice", adminTok, map[string]any{"disabled": true})
	if w.Code != http.StatusOK {
		t.Fatalf("禁用 alice 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if w = getAuth(t, h, "/api/v1/sources", aliceTok); w.Code != http.StatusUnauthorized {
		t.Fatalf("禁用后访问应立即 401，实际 %d", w.Code)
	}
}

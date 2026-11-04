package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	return getAuth(t, h, url, "")
}

// testWebFS 构造一个最小前端资源（index.html），供 API 单测使用。
func testWebFS(t *testing.T) fs.FS {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><html><body>test-ui</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return os.DirFS(dir)
}

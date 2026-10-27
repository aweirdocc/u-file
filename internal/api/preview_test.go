package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ufile/internal/source"

	// 注册内建驱动
	_ "ufile/internal/driver/all"
)

func newPreviewServer(t *testing.T) *gin.Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello preview"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte("<script>alert(1)</script>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pic.png"), []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte{0x4d, 0x5a, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "s", Name: "s", Type: "local", Config: map[string]any{"root": dir}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(mgr)
	if err != nil {
		t.Fatal(err)
	}
	return srv.engine
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(context.Background())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRawInlineHeaders(t *testing.T) {
	h := newPreviewServer(t)

	w := get(t, h, "/api/v1/sources/s/raw?path=hello.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("raw txt 应 200，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("txt Content-Type 不符: %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("应为 inline: %q", cd)
	}
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("inline 响应必须带 CSP 与 nosniff")
	}
}

func TestRawRejectsNonPreviewable(t *testing.T) {
	h := newPreviewServer(t)

	// exe 不在白名单
	w := get(t, h, "/api/v1/sources/s/raw?path=app.exe")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("exe 预览应 415，得到 %d", w.Code)
	}

	// html 允许预览，但必须强制 text/plain 直出（脚本失效，配合 CSP 双保险）
	w = get(t, h, "/api/v1/sources/s/raw?path=page.html")
	if w.Code != http.StatusOK {
		t.Fatalf("html 预览应 200（按纯文本），得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("html 必须以 text/plain 直出，得到 %q", ct)
	}
}

func TestRawRangeStillWorks(t *testing.T) {
	h := newPreviewServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/s/raw?path=pic.png", nil)
	req.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("Range 预览应 206，得到 %d", w.Code)
	}
	if got := w.Body.Bytes(); len(got) != 4 {
		t.Fatalf("Range 长度不符: %d", len(got))
	}
}

func TestDownloadStaysAttachment(t *testing.T) {
	h := newPreviewServer(t)

	w := get(t, h, "/api/v1/sources/s/download?path=hello.txt")
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("download 仍应 attachment: %q", cd)
	}
}

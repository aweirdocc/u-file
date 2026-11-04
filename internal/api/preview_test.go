package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	_ "ufile/internal/driver/all"
	"ufile/internal/office"
	"ufile/internal/source"
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
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(mgr, nil, nil, nil, nil, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	return srv.engine
}

func getPreview(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(context.Background())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRawInlineHeaders(t *testing.T) {
	h := newPreviewServer(t)

	w := getPreview(t, h, "/api/v1/sources/s/raw?path=hello.txt")
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
	w := getPreview(t, h, "/api/v1/sources/s/raw?path=app.exe")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("exe 预览应 415，得到 %d", w.Code)
	}

	// html 允许预览，但必须强制 text/plain 直出（脚本失效，配合 CSP 双保险）
	w = getPreview(t, h, "/api/v1/sources/s/raw?path=page.html")
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

	w := getPreview(t, h, "/api/v1/sources/s/download?path=hello.txt")
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("download 仍应 attachment: %q", cd)
	}
}

func TestThumbAndOfficeEndpoints(t *testing.T) {
	dir := t.TempDir()

	// 1. 生成一张 800x400 的大图 photo.png
	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			img.Set(x, y, color.RGBA{R: 50, G: 120, B: 200, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), pngBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. 生成一个测试 docx 文件
	var docxBuf bytes.Buffer
	zw := zip.NewWriter(&docxBuf)
	f, _ := zw.Create("word/document.xml")
	f.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Hello Office Preview</w:t></w:r></w:p>
  </w:body>
</w:document>`))
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "spec.docx"), docxBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := source.NewManager(t.TempDir(), []source.Seed{
		{ID: "local-1", Name: "本地测试", Type: "local", Config: map[string]any{"root": dir}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(mgr, nil, nil, nil, nil, testWebFS(t))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// 测试动态缩略图：/api/v1/sources/local-1/thumb?path=photo.png&w=200&h=200
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local-1/thumb?path=photo.png&w=200&h=200", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("thumb status = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("unexpected content-type: %s", ct)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}
	outImg, err := jpeg.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode jpeg thumb failed: %v", err)
	}
	if outImg.Bounds().Dx() != 200 || outImg.Bounds().Dy() != 100 {
		t.Fatalf("unexpected thumb bounds: %dx%d", outImg.Bounds().Dx(), outImg.Bounds().Dy())
	}

	// 测试 If-None-Match 304 命中
	req304 := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local-1/thumb?path=photo.png&w=200&h=200", nil)
	req304.Header.Set("If-None-Match", etag)
	w304 := httptest.NewRecorder()
	h.ServeHTTP(w304, req304)
	if w304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", w304.Code)
	}

	// 测试 Office 预览：/api/v1/sources/local-1/office?path=spec.docx
	reqDoc := httptest.NewRequest(http.MethodGet, "/api/v1/sources/local-1/office?path=spec.docx", nil)
	wDoc := httptest.NewRecorder()
	h.ServeHTTP(wDoc, reqDoc)
	if wDoc.Code != http.StatusOK {
		t.Fatalf("office status = %d: %s", wDoc.Code, wDoc.Body.String())
	}
	var parsed office.Result
	if err := json.Unmarshal(wDoc.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal office result: %v", err)
	}
	if parsed.Kind != "docx" || parsed.Docx == nil || len(parsed.Docx.Blocks) != 1 {
		t.Fatalf("unexpected office result: %+v", parsed)
	}
}

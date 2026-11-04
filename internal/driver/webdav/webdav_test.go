package webdav

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ufile/internal/driver"
)

// fakeWebDAV 内存级最小 WebDAV 服务端，覆盖 PROPFIND / GET(Range) / PUT / MKCOL / DELETE / MOVE。
type fakeWebDAV struct {
	mu    sync.Mutex
	user  string
	pass  string
	dirs  map[string]bool
	files map[string][]byte
}

func newFakeWebDAV(user, pass string) *fakeWebDAV {
	return &fakeWebDAV{
		user:  user,
		pass:  pass,
		dirs:  map[string]bool{"": true, "dav": true, "dav/workspace": true},
		files: map[string][]byte{},
	}
}

func (f *fakeWebDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.user != "" || f.pass != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	p := strings.Trim(r.URL.Path, "/")

	switch r.Method {
	case "PROPFIND":
		depth := r.Header.Get("Depth")
		if isDir := f.dirs[p]; isDir {
			var buf bytes.Buffer
			buf.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
			writeDirResp(&buf, "/"+p+"/")
			if depth == "1" {
				prefix := p
				if prefix != "" {
					prefix += "/"
				}
				var names []string
				for d := range f.dirs {
					if strings.HasPrefix(d, prefix) && d != p {
						rest := strings.TrimPrefix(d, prefix)
						if !strings.Contains(rest, "/") {
							names = append(names, "D:"+d)
						}
					}
				}
				for fn := range f.files {
					if strings.HasPrefix(fn, prefix) {
						rest := strings.TrimPrefix(fn, prefix)
						if !strings.Contains(rest, "/") {
							names = append(names, "F:"+fn)
						}
					}
				}
				sort.Strings(names)
				for _, item := range names {
					if strings.HasPrefix(item, "D:") {
						writeDirResp(&buf, "/"+strings.TrimPrefix(item, "D:")+"/")
					} else {
						fn := strings.TrimPrefix(item, "F:")
						writeFileResp(&buf, "/"+fn, len(f.files[fn]))
					}
				}
			}
			buf.WriteString(`</D:multistatus>`)
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write(buf.Bytes())
			return
		}
		if data, ok := f.files[p]; ok {
			var buf bytes.Buffer
			buf.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
			writeFileResp(&buf, "/"+p, len(data))
			buf.WriteString(`</D:multistatus>`)
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write(buf.Bytes())
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case http.MethodGet:
		data, ok := f.files[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, path.Base(p), time.Now(), bytes.NewReader(data))

	case "MKCOL":
		if f.dirs[p] {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		if !f.dirs[parent] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.dirs[p] = true
		w.WriteHeader(http.StatusCreated)

	case http.MethodPut:
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		if !f.dirs[parent] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.files[p] = b
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		if f.dirs[p] {
			delete(f.dirs, p)
			prefix := p + "/"
			for k := range f.dirs {
				if strings.HasPrefix(k, prefix) {
					delete(f.dirs, k)
				}
			}
			for k := range f.files {
				if strings.HasPrefix(k, prefix) {
					delete(f.files, k)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, ok := f.files[p]; ok {
			delete(f.files, p)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case "MOVE":
		dest := r.Header.Get("Destination")
		u, err := url.Parse(dest)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		dstPath := strings.Trim(u.Path, "/")
		if data, ok := f.files[p]; ok {
			delete(f.files, p)
			f.files[dstPath] = data
			w.WriteHeader(http.StatusCreated)
			return
		}
		if f.dirs[p] {
			delete(f.dirs, p)
			f.dirs[dstPath] = true
			prefix := p + "/"
			for k, v := range f.files {
				if strings.HasPrefix(k, prefix) {
					delete(f.files, k)
					f.files[dstPath+"/"+strings.TrimPrefix(k, prefix)] = v
				}
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func escapeHref(rawPath string) string {
	var b strings.Builder
	for seg := range strings.SplitSeq(strings.Trim(rawPath, "/"), "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(url.PathEscape(seg))
	}
	if strings.HasSuffix(rawPath, "/") {
		b.WriteByte('/')
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

func writeDirResp(buf *bytes.Buffer, href string) {
	fmt.Fprintf(buf, `<D:response><D:href>%s</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, escapeHref(href))
}

func writeFileResp(buf *bytes.Buffer, href string, size int) {
	fmt.Fprintf(buf, `<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getcontentlength>%d</D:getcontentlength><D:getlastmodified>Wed, 04 Nov 2026 06:00:00 GMT</D:getlastmodified><D:getetag>"etag-%d"</D:getetag><D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, escapeHref(href), size, size)
}

func TestWebDAVLifecycle(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV("alice@example.com", "app-secret"))
	defer srv.Close()

	drv, err := New(map[string]any{
		"endpoint": srv.URL + "/dav",
		"username": "alice@example.com",
		"password": "app-secret",
		"root":     "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := drv.(driver.Writer)
	ctx := context.Background()

	// 1. 初始目录为空
	res, err := drv.List(ctx, "", driver.ListOptions{})
	if err != nil {
		t.Fatalf("List root failed: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("expected empty root, got %+v", res.Entries)
	}

	// 2. 上传文件（自动创建父目录 docs/2026）
	payload := []byte("hello jianguoyun webdav stream!")
	if err := w.Upload(ctx, "docs/2026/Note 1.txt", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// 3. Stat 校验
	st, err := drv.Stat(ctx, "docs/2026/Note 1.txt")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if st.IsDir || st.Size != int64(len(payload)) || st.Name != "Note 1.txt" {
		t.Fatalf("unexpected Stat entry: %+v", st)
	}

	// 4. Open 完整读取与 Range 切片读取
	rc, err := drv.Open(ctx, "docs/2026/Note 1.txt", driver.OpenOptions{Offset: 6, Length: 10})
	if err != nil {
		t.Fatalf("Open range failed: %v", err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if string(part) != "jianguoyun" {
		t.Fatalf("expected 'jianguoyun', got %q", string(part))
	}

	// 5. Rename 改名（含字面量 % 与空格的特殊文件名，验证不会发生二次 URL 解码破坏）
	if err := w.Rename(ctx, "docs/2026/Note 1.txt", "docs/2026/100%bar %20.txt"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	if _, err := drv.Stat(ctx, "docs/2026/Note 1.txt"); err != driver.ErrNotFound {
		t.Fatalf("old path should be NotFound, got %v", err)
	}

	// 6. List 子目录
	subList, err := drv.List(ctx, "docs/2026", driver.ListOptions{})
	if err != nil || len(subList.Entries) != 1 || subList.Entries[0].Name != "100%bar %20.txt" {
		t.Fatalf("List subdir unexpected: %+v, err=%v", subList, err)
	}

	// 7. Delete 目录
	if err := w.Delete(ctx, "docs"); err != nil {
		t.Fatalf("Delete dir failed: %v", err)
	}
	if _, err := drv.Stat(ctx, "docs/2026/100%bar %20.txt"); err != driver.ErrNotFound {
		t.Fatalf("expected NotFound after dir delete, got %v", err)
	}
}

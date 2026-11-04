package gdrive

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ufile/internal/driver"
)

type fakeGItem struct {
	ID       string
	Name     string
	ParentID string
	MimeType string
	Data     []byte
	Trashed  bool
	ModTime  string
}

type fakeGDriveServer struct {
	mu      sync.Mutex
	nextID  int
	items   map[string]*fakeGItem
	srvURL  string
}

func newFakeGDriveServer() *fakeGDriveServer {
	return &fakeGDriveServer{
		nextID: 1,
		items: map[string]*fakeGItem{
			"root": {ID: "root", Name: "root", MimeType: folderMimeType},
		},
	}
}

var (
	reParent = regexp.MustCompile(`'([^']+)' in parents`)
	reName   = regexp.MustCompile(`name = '((?:\\'|[^'])*)'`)
)

func (s *fakeGDriveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"mock-access-token","expires_in":3600}`))
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/uploadSession/") {
		if r.Header.Get("Authorization") != "Bearer mock-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	// Resumable upload session PUT
	case strings.HasPrefix(r.URL.Path, "/uploadSession/") && r.Method == http.MethodPut:
		id := strings.TrimPrefix(r.URL.Path, "/uploadSession/")
		it, ok := s.items[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		it.Data = data
		s.writeItemJSON(w, it, http.StatusOK)

	// Init resumable upload (new file)
	case r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost:
		var req struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		parent := "root"
		if len(req.Parents) > 0 {
			parent = req.Parents[0]
		}
		id := fmt.Sprintf("f-%d", s.nextID)
		s.nextID++
		s.items[id] = &fakeGItem{
			ID:       id,
			Name:     req.Name,
			ParentID: parent,
			MimeType: "application/octet-stream",
			ModTime:  time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Location", s.srvURL+"/uploadSession/"+id)
		w.WriteHeader(http.StatusOK)

	// Init resumable upload (overwrite existing file)
	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files/") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		if _, ok := s.items[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", s.srvURL+"/uploadSession/"+id)
		w.WriteHeader(http.StatusOK)

	// List / Lookup files
	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet:
		q := r.URL.Query().Get("q")
		var parentID, wantName string
		if m := reParent.FindStringSubmatch(q); len(m) == 2 {
			parentID = m[1]
		}
		if m := reName.FindStringSubmatch(q); len(m) == 2 {
			wantName = strings.ReplaceAll(m[1], `\'`, `'`)
			wantName = strings.ReplaceAll(wantName, `\\`, `\`)
		}
		var matched []*fakeGItem
		for _, it := range s.items {
			if it.ID == "root" || it.Trashed {
				continue
			}
			if parentID != "" && it.ParentID != parentID {
				continue
			}
			if wantName != "" && it.Name != wantName {
				continue
			}
			matched = append(matched, it)
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].Name < matched[j].Name })
		out := make([]map[string]any, 0, len(matched))
		for _, it := range matched {
			out = append(out, map[string]any{
				"id":           it.ID,
				"name":         it.Name,
				"mimeType":     it.MimeType,
				"size":         strconv.Itoa(len(it.Data)),
				"modifiedTime": it.ModTime,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})

	// Create folder
	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodPost:
		var req struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		parent := "root"
		if len(req.Parents) > 0 {
			parent = req.Parents[0]
		}
		id := fmt.Sprintf("d-%d", s.nextID)
		s.nextID++
		it := &fakeGItem{
			ID:       id,
			Name:     req.Name,
			ParentID: parent,
			MimeType: req.MimeType,
			ModTime:  time.Now().UTC().Format(time.RFC3339),
		}
		s.items[id] = it
		s.writeItemJSON(w, it, http.StatusOK)

	// Download media
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		it, ok := s.items[id]
		if !ok || it.Trashed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, it.Name, time.Now(), bytes.NewReader(it.Data))

	// Patch file (trash or rename/move)
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		it, ok := s.items[id]
		if !ok || it.Trashed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Name    string `json:"name"`
			Trashed *bool  `json:"trashed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Trashed != nil && *req.Trashed {
			it.Trashed = true
		}
		if req.Name != "" {
			it.Name = req.Name
		}
		if addP := r.URL.Query().Get("addParents"); addP != "" {
			it.ParentID = addP
		}
		s.writeItemJSON(w, it, http.StatusOK)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *fakeGDriveServer) writeItemJSON(w http.ResponseWriter, it *fakeGItem, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":           it.ID,
		"name":         it.Name,
		"mimeType":     it.MimeType,
		"size":         strconv.Itoa(len(it.Data)),
		"modifiedTime": it.ModTime,
	})
}

func TestGDriveOAuth2Lifecycle(t *testing.T) {
	fake := newFakeGDriveServer()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.srvURL = srv.URL

	drv, err := New(map[string]any{
		"auth_type":     "oauth2",
		"client_id":     "cid",
		"client_secret": "csec",
		"refresh_token": "rtok",
		"api_base":      srv.URL,
		"token_url":     srv.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := drv.(driver.Writer)
	ctx := context.Background()

	// 1. 上传带单引号文件名（验证 q 查询转义）
	content := []byte("google drive stream data 0123456789")
	if err := w.Upload(ctx, "team/Bob's Report.txt", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// 2. Stat 验证
	st, err := drv.Stat(ctx, "team/Bob's Report.txt")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if st.Size != int64(len(content)) || st.Name != "Bob's Report.txt" {
		t.Fatalf("unexpected Stat: %+v", st)
	}

	// 3. Range 读取验证
	rc, err := drv.Open(ctx, "team/Bob's Report.txt", driver.OpenOptions{Offset: 7, Length: 5})
	if err != nil {
		t.Fatalf("Open range failed: %v", err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if string(part) != "drive" {
		t.Fatalf("expected 'drive', got %q", string(part))
	}

	// 4. 移动并重命名
	if err := w.Rename(ctx, "team/Bob's Report.txt", "archive/2026.txt"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	if _, err := drv.Stat(ctx, "team/Bob's Report.txt"); err != driver.ErrNotFound {
		t.Fatalf("old path should be ErrNotFound, got %v", err)
	}
	if st2, err := drv.Stat(ctx, "archive/2026.txt"); err != nil || st2.Size != int64(len(content)) {
		t.Fatalf("new path Stat failed: %v, %+v", err, st2)
	}

	// 5. 删除（移入回收站 trashed=true）
	if err := w.Delete(ctx, "archive/2026.txt"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := drv.Stat(ctx, "archive/2026.txt"); err != driver.ErrNotFound {
		t.Fatalf("deleted file should be ErrNotFound, got %v", err)
	}
}

func TestGDriveServiceAccountAuth(t *testing.T) {
	fake := newFakeGDriveServer()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.srvURL = srv.URL

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	saJSON, _ := json.Marshal(map[string]string{
		"client_email": "bot@project.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    srv.URL + "/token",
	})

	drv, err := New(map[string]any{
		"auth_type":            "service_account",
		"service_account_json": string(saJSON),
		"api_base":             srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.List(context.Background(), "", driver.ListOptions{}); err != nil {
		t.Fatalf("Service Account List failed: %v", err)
	}
}

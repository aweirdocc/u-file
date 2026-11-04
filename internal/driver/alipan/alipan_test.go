package alipan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ufile/internal/driver"
)

type fakeAliItem struct {
	FileID   string
	ParentID string
	Name     string
	Type     string // "folder" | "file"
	Data     []byte
	Trashed  bool
}

type fakeAlipanServer struct {
	mu      sync.Mutex
	nextID  int
	items   map[string]*fakeAliItem
	srvURL  string
}

func newFakeAlipanServer() *fakeAlipanServer {
	return &fakeAlipanServer{
		nextID: 1,
		items: map[string]*fakeAliItem{
			"root": {FileID: "root", Name: "root", Type: "folder"},
		},
	}
}

func (s *fakeAlipanServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.URL.Path == "/oauth/access_token":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ali-access-1","refresh_token":"rotated-refresh-token-2","expires_in":7200}`))

	case r.URL.Path == "/adrive/v1.0/user/getDriveInfo":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_drive_id":"drv-default","backup_drive_id":"drv-backup","resource_drive_id":"drv-resource"}`))

	case r.URL.Path == "/adrive/v1.0/openFile/list":
		var req struct {
			ParentFileID string `json:"parent_file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var out []map[string]any
		for _, it := range s.items {
			if it.FileID == "root" || it.Trashed || it.ParentID != req.ParentFileID {
				continue
			}
			out = append(out, map[string]any{
				"file_id":    it.FileID,
				"name":       it.Name,
				"type":       it.Type,
				"size":       len(it.Data),
				"updated_at": time.Now().UTC().Format(time.RFC3339),
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": out})

	case r.URL.Path == "/adrive/v1.0/openFile/create":
		var req struct {
			ParentFileID string `json:"parent_file_id"`
			Name         string `json:"name"`
			Type         string `json:"type"`
			PartInfoList []struct {
				PartNumber int `json:"part_number"`
			} `json:"part_info_list"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		// 若同名已存在则复用
		var target *fakeAliItem
		for _, it := range s.items {
			if !it.Trashed && it.ParentID == req.ParentFileID && it.Name == req.Name && it.Type == req.Type {
				target = it
				break
			}
		}
		if target == nil {
			id := fmt.Sprintf("ali-%d", s.nextID)
			s.nextID++
			target = &fakeAliItem{
				FileID:   id,
				ParentID: req.ParentFileID,
				Name:     req.Name,
				Type:     req.Type,
			}
			s.items[id] = target
		} else if req.Type == "file" {
			target.Data = nil
		}

		var parts []map[string]any
		for _, p := range req.PartInfoList {
			parts = append(parts, map[string]any{
				"part_number": p.PartNumber,
				"upload_url":  fmt.Sprintf("%s/partUpload/%s/%d", s.srvURL, target.FileID, p.PartNumber),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_id":        target.FileID,
			"upload_id":      "up-" + target.FileID,
			"part_info_list": parts,
		})

	case strings.HasPrefix(r.URL.Path, "/partUpload/") && r.Method == http.MethodPut:
		segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/partUpload/"), "/")
		fileID := segs[0]
		it, ok := s.items[fileID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		chunk, _ := io.ReadAll(r.Body)
		it.Data = append(it.Data, chunk...)
		w.WriteHeader(http.StatusOK)

	case r.URL.Path == "/adrive/v1.0/openFile/complete":
		var req struct {
			FileID string `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		it, ok := s.items[req.FileID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_id":    it.FileID,
			"name":       it.Name,
			"type":       it.Type,
			"size":       len(it.Data),
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		})

	case r.URL.Path == "/adrive/v1.0/openFile/getDownloadUrl":
		var req struct {
			FileID string `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := s.items[req.FileID]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url": s.srvURL + "/cdnDownload/" + req.FileID,
		})

	case strings.HasPrefix(r.URL.Path, "/cdnDownload/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/cdnDownload/")
		it, ok := s.items[id]
		if !ok || it.Trashed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, it.Name, time.Now(), bytes.NewReader(it.Data))

	case r.URL.Path == "/adrive/v1.0/openFile/recyclebin/trash":
		var req struct {
			FileID string `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		it, ok := s.items[req.FileID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.Trashed = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))

	case r.URL.Path == "/adrive/v1.0/openFile/update":
		var req struct {
			FileID string `json:"file_id"`
			Name   string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		it, ok := s.items[req.FileID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.Name = req.Name
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))

	case r.URL.Path == "/adrive/v1.0/openFile/move":
		var req struct {
			FileID         string `json:"file_id"`
			ToParentFileID string `json:"to_parent_file_id"`
			NewName        string `json:"new_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		it, ok := s.items[req.FileID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.ParentID = req.ToParentFileID
		if req.NewName != "" {
			it.Name = req.NewName
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestAlipanLifecycleAndTokenRotation(t *testing.T) {
	fake := newFakeAlipanServer()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.srvURL = srv.URL

	var rotatedToken string
	drv, err := New(map[string]any{
		"refresh_token":             "init-refresh-token-1",
		"oauth_token_url":           srv.URL + "/oauth/access_token",
		"api_base":                  srv.URL,
		"drive_type":                "backup",
		driver.RefreshTokenSaverKey: func(tok string) { rotatedToken = tok },
	})
	if err != nil {
		t.Fatal(err)
	}
	w := drv.(driver.Writer)
	p := drv.(driver.Presigner)
	ctx := context.Background()

	// 1. 上传文件（自动创建 movies/2026 目录 + 触发首次 Token 刷新与轮转回调）
	payload := []byte("alipan-video-stream-payload-9876543210")
	if err := w.Upload(ctx, "movies/2026/demo.mp4", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if rotatedToken != "rotated-refresh-token-2" {
		t.Fatalf("expected rotated refresh_token callback, got %q", rotatedToken)
	}

	// 2. PresignGet 直链验证
	directURL, err := p.PresignGet(ctx, "movies/2026/demo.mp4", 30*time.Minute)
	if err != nil || !strings.Contains(directURL, "/cdnDownload/") {
		t.Fatalf("PresignGet failed: url=%q, err=%v", directURL, err)
	}

	// 3. Open Range 读取验证
	rc, err := drv.Open(ctx, "movies/2026/demo.mp4", driver.OpenOptions{Offset: 7, Length: 12})
	if err != nil {
		t.Fatalf("Open range failed: %v", err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if string(part) != "video-stream" {
		t.Fatalf("expected 'video-stream', got %q", string(part))
	}

	// 4. Rename 同目录改名 + 跨目录移动
	if err := w.Rename(ctx, "movies/2026/demo.mp4", "movies/archive/final.mp4"); err != nil {
		t.Fatalf("Rename across dir failed: %v", err)
	}
	if st, err := drv.Stat(ctx, "movies/archive/final.mp4"); err != nil || st.Size != int64(len(payload)) {
		t.Fatalf("Stat after move failed: %+v, err=%v", st, err)
	}

	// 5. Delete（移入云盘回收站）
	if err := w.Delete(ctx, "movies/archive/final.mp4"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := drv.Stat(ctx, "movies/archive/final.mp4"); err != driver.ErrNotFound {
		t.Fatalf("expected ErrNotFound after trash, got %v", err)
	}
}

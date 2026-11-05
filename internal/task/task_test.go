package task

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ufile/internal/driver"
	"ufile/internal/driver/local"
)

func setupLocalPair(t *testing.T) (string, string, DriverResolver) {
	t.Helper()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()

	srcDrv, err := local.New(map[string]any{"root": srcRoot})
	if err != nil {
		t.Fatal(err)
	}
	dstDrv, err := local.New(map[string]any{"root": dstRoot})
	if err != nil {
		t.Fatal(err)
	}

	resolver := func(id string) (driver.FSDriver, error) {
		switch id {
		case "src":
			return srcDrv, nil
		case "dst":
			return dstDrv, nil
		default:
			return nil, errors.New("unknown source")
		}
	}
	return srcRoot, dstRoot, resolver
}

func waitForStatus(t *testing.T, mgr *Manager, id string, want TaskStatus) *Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := mgr.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Status == want {
			return tk
		}
		if tk.Status == StatusFailed && want != StatusFailed {
			t.Fatalf("task %s unexpectedly failed: %s", id, tk.ErrorMsg)
		}
		time.Sleep(15 * time.Millisecond)
	}
	tk, _ := mgr.Get(id)
	t.Fatalf("timeout waiting for task %s status=%s, got=%v (err=%s)", id, want, tk.Status, tk.ErrorMsg)
	return nil
}

func TestCrossSourceCopyAndMove(t *testing.T) {
	srcRoot, dstRoot, resolver := setupLocalPair(t)
	if err := os.MkdirAll(filepath.Join(srcRoot, "docs", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "docs", "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "docs", "sub", "b.txt"), []byte("nested content 12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, Config{Workers: 2}, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	// 1. 跨源递归复制目录 docs -> backup/docs
	cpTask, err := mgr.Submit(TransferRequest{
		Type:           TypeCopy,
		UserID:         "admin",
		SourceID:       "src",
		TargetSourceID: "dst",
		Paths:          []string{"docs"},
		DstDir:         "backup",
	})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, mgr, cpTask.ID, StatusSuccess)
	if done.TransferredBytes != int64(len("hello world")+len("nested content 12345")) {
		t.Fatalf("unexpected transferred bytes: %d", done.TransferredBytes)
	}
	gotA, err := os.ReadFile(filepath.Join(dstRoot, "backup", "docs", "a.txt"))
	if err != nil || string(gotA) != "hello world" {
		t.Fatalf("copied file a.txt mismatch: %v %q", err, gotA)
	}
	gotB, err := os.ReadFile(filepath.Join(dstRoot, "backup", "docs", "sub", "b.txt"))
	if err != nil || string(gotB) != "nested content 12345" {
		t.Fatalf("copied file b.txt mismatch: %v %q", err, gotB)
	}
	// 源文件仍在
	if _, err := os.Stat(filepath.Join(srcRoot, "docs", "a.txt")); err != nil {
		t.Fatalf("source should still exist after copy: %v", err)
	}

	// 2. 跨源移动目录 docs -> moved/docs
	mvTask, err := mgr.Submit(TransferRequest{
		Type:           TypeMove,
		UserID:         "admin",
		SourceID:       "src",
		TargetSourceID: "dst",
		Paths:          []string{"docs"},
		DstDir:         "moved",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, mvTask.ID, StatusSuccess)

	// 目标已写入
	if _, err := os.Stat(filepath.Join(dstRoot, "moved", "docs", "sub", "b.txt")); err != nil {
		t.Fatalf("moved file missing at destination: %v", err)
	}
	// 源端已被清理
	if _, err := os.Stat(filepath.Join(srcRoot, "docs")); !os.IsNotExist(err) {
		t.Fatalf("source directory should be removed after move, got err=%v", err)
	}
}

// flakyWriter 模拟前 N 次写入报错、之后成功的驱动包装器，测试移动补偿与自动重试。
type flakyWriter struct {
	driver.FSDriver
	w         driver.Writer
	failTimes int32
	calls     atomic.Int32
}

func (f *flakyWriter) Upload(ctx context.Context, path string, r io.Reader, size int64) error {
	c := f.calls.Add(1)
	if c <= f.failTimes {
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 4))
		return errors.New("simulated network glitch")
	}
	return f.w.Upload(ctx, path, r, size)
}
func (f *flakyWriter) Delete(ctx context.Context, path string) error { return f.w.Delete(ctx, path) }
func (f *flakyWriter) Mkdir(ctx context.Context, path string) error  { return f.w.Mkdir(ctx, path) }
func (f *flakyWriter) Rename(ctx context.Context, from, to string) error {
	return f.w.Rename(ctx, from, to)
}

func TestMoveCompensationAndRetry(t *testing.T) {
	srcRoot, dstRoot, baseResolver := setupLocalPair(t)
	srcFile := filepath.Join(srcRoot, "important.bin")
	payload := bytes.Repeat([]byte("ABCD"), 1024)
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDrv, _ := baseResolver("dst")
	flakyDst := &flakyWriter{
		FSDriver:  dstDrv,
		w:         dstDrv.(driver.Writer),
		failTimes: 5, // 超过最大重试次数 2，使首次任务最终失败
	}

	resolver := func(id string) (driver.FSDriver, error) {
		if id == "dst" {
			return flakyDst, nil
		}
		return baseResolver(id)
	}

	mgr, err := NewManager(t.TempDir(), Config{
		Workers:        1,
		MaxRetries:     2,
		RetryBaseDelay: 10 * time.Millisecond,
	}, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	tk, err := mgr.Submit(TransferRequest{
		Type:           TypeMove,
		UserID:         "admin",
		SourceID:       "src",
		TargetSourceID: "dst",
		Paths:          []string{"important.bin"},
		DstDir:         "",
	})
	if err != nil {
		t.Fatal(err)
	}

	failed := waitForStatus(t, mgr, tk.ID, StatusFailed)
	if failed.RetryCount != 2 {
		t.Fatalf("expected 2 retries, got %d", failed.RetryCount)
	}

	// 关键断言：移动失败时，源文件必须毫发无损！
	raw, err := os.ReadFile(srcFile)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("source file must remain intact after failed move! err=%v", err)
	}

	// 现在把失败阈值调小，手动触发 Retry
	flakyDst.failTimes = 0
	if _, err := mgr.Retry(tk.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, tk.ID, StatusSuccess)

	// 成功后目标文件完整，源文件被删除
	dstRaw, err := os.ReadFile(filepath.Join(dstRoot, "important.bin"))
	if err != nil || !bytes.Equal(dstRaw, payload) {
		t.Fatalf("destination file corrupted after retry: %v", err)
	}
	if _, err := os.Stat(srcFile); !os.IsNotExist(err) {
		t.Fatalf("source file should be deleted after successful move")
	}
}

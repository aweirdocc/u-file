package local

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"ufile/internal/driver"
)

func newTestDriver(t *testing.T) driver.FSDriver {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("docs/a.txt", "hello world 123")
	mustWrite("photos/b.bin", "0123456789abcdef")
	mustWrite("top.txt", "top")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := New(map[string]any{"root": root})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestList(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	res, err := d.List(ctx, "", driver.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 4 {
		t.Fatalf("根目录应有 4 个条目，得到 %d：%v", len(res.Entries), res.Entries)
	}
	if res.Entries[0].Name != "docs" || !res.Entries[0].IsDir {
		t.Fatalf("排序后首个条目应为 docs 目录：%v", res.Entries[0])
	}

	res, err = d.List(ctx, "docs", driver.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "docs/a.txt" {
		t.Fatalf("docs 目录列表不符：%v", res.Entries)
	}

	if _, err := d.List(ctx, "no/such", driver.ListOptions{}); err != driver.ErrNotFound {
		t.Fatalf("不存在的目录应返回 ErrNotFound，得到 %v", err)
	}
}

func TestStat(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	e, err := d.Stat(ctx, "docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.IsDir || e.Size != int64(len("hello world 123")) || e.Name != "a.txt" {
		t.Fatalf("文件 Stat 结果不符：%+v", e)
	}

	e, err = d.Stat(ctx, "docs")
	if err != nil || !e.IsDir {
		t.Fatalf("目录 Stat 结果不符：%+v err=%v", e, err)
	}

	if _, err := d.Stat(ctx, "ghost"); err != driver.ErrNotFound {
		t.Fatalf("不存在的对象应返回 ErrNotFound，得到 %v", err)
	}
}

func TestOpenFullAndRange(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	rc, err := d.Open(ctx, "docs/a.txt", driver.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello world 123" {
		t.Fatalf("全文读取不符：%q", got)
	}

	rc, err = d.Open(ctx, "docs/a.txt", driver.OpenOptions{Offset: 6, Length: 5})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "world" {
		t.Fatalf("Range 读取不符：%q", got)
	}
}

func TestInvalidPath(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	for _, p := range []string{"../escape", "a/../../b", "a\\b"} {
		if _, err := d.List(ctx, p, driver.ListOptions{}); err != driver.ErrInvalidPath {
			t.Fatalf("路径 %q 应返回 ErrInvalidPath，得到 %v", p, err)
		}
		if _, err := d.Open(ctx, p, driver.OpenOptions{}); err != driver.ErrInvalidPath {
			t.Fatalf("路径 %q 应返回 ErrInvalidPath，得到 %v", p, err)
		}
	}
}

func TestOpenIsSeekable(t *testing.T) {
	// 本地文件的流应实现 io.Seeker，HTTP 层可直接用于 ServeContent。
	d := newTestDriver(t)
	rc, err := d.Open(context.Background(), "docs/a.txt", driver.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, ok := rc.(io.Seeker); !ok {
		t.Fatal("local.Open 返回的流应实现 io.Seeker")
	}
}

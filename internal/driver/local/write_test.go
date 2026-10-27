package local

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ufile/internal/driver"
)

func testDriver(t *testing.T) *Local {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(map[string]any{"root": root})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Local)
}

func TestUploadAndList(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	err := d.Upload(ctx, "new/b.txt", bytes.NewReader([]byte("hello upload")), 12)
	if err != nil {
		t.Fatal(err)
	}
	// 内容正确
	rc, err := d.Open(ctx, "new/b.txt", driver.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello upload" {
		t.Fatalf("内容不符: %q", got)
	}
	// 列表可见
	res, err := d.List(ctx, "new", driver.ListOptions{})
	if err != nil || len(res.Entries) != 1 || res.Entries[0].Name != "b.txt" {
		t.Fatalf("列表不符: %+v err=%v", res, err)
	}
	// 覆盖写
	if err := d.Upload(ctx, "new/b.txt", strings.NewReader("v2"), 2); err != nil {
		t.Fatal(err)
	}
	rc, _ = d.Open(ctx, "new/b.txt", driver.OpenOptions{})
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "v2" {
		t.Fatalf("覆盖写不符: %q", got)
	}
	// 根路径拒绝
	if err := d.Upload(ctx, "", strings.NewReader("x"), 1); err == nil {
		t.Fatal("上传到根应拒绝")
	}
}

func TestDeleteFileAndDir(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	// 删文件
	if err := d.Delete(ctx, "docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "docs/a.txt"); err != driver.ErrNotFound {
		t.Fatalf("删除后应 NotFound: %v", err)
	}
	// 删目录（递归）
	if err := d.Upload(ctx, "dir/sub/f.txt", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "dir/sub/f.txt"); err != driver.ErrNotFound {
		t.Fatal("递归删除失败")
	}
	// 删根拒绝
	if err := d.Delete(ctx, ""); err == nil {
		t.Fatal("删除根应拒绝")
	}
	if err := d.Delete(ctx, "../escape"); err == nil {
		t.Fatal(".. 应拒绝")
	}
}

func TestMkdirAndRename(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if err := d.Mkdir(ctx, "x/y/z"); err != nil {
		t.Fatal(err)
	}
	st, err := d.Stat(ctx, "x/y/z")
	if err != nil || !st.IsDir {
		t.Fatalf("多级建目录失败: %+v %v", st, err)
	}
	// 重命名文件
	if err := d.Rename(ctx, "docs/a.txt", "x/y/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "x/y/renamed.txt"); err != nil {
		t.Fatal("重命名后应可 Stat")
	}
	if _, err := d.Stat(ctx, "docs/a.txt"); err != driver.ErrNotFound {
		t.Fatal("旧路径应不存在")
	}
	// 重命名目录
	if err := d.Rename(ctx, "x/y", "x/z2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "x/z2/renamed.txt"); err != nil {
		t.Fatal("目录重命名后内容应跟随")
	}
	// 不能把目录移进自己
	if err := d.Rename(ctx, "x", "x/inside"); err == nil {
		t.Fatal("移进自己应拒绝")
	}
	// 源不存在
	if err := d.Rename(ctx, "ghost", "x"); err != driver.ErrNotFound {
		t.Fatalf("应 ErrNotFound: %v", err)
	}
}

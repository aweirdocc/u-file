package s3

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"ufile/internal/driver"
)

// TestS3WriteSmoke 对真实 S3 端点的写能力冒烟（上传/删除/建目录/重命名）。
// 与 TestS3Smoke 同环境变量门控；会在桶内 m2-write-smoke/ 前缀下写入测试数据并清理。
func TestS3WriteSmoke(t *testing.T) {
	if os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("未设置 TEST_S3_ENDPOINT，跳过 S3 写冒烟")
	}
	if os.Getenv("TEST_S3_ACCESS_KEY") == "" {
		t.Skip("未设置 TEST_S3_ACCESS_KEY（写操作需要凭据）")
	}
	cfg := map[string]any{
		"endpoint":       os.Getenv("TEST_S3_ENDPOINT"),
		"region":         envOr("TEST_S3_REGION", "us-east-1"),
		"bucket":         envOr("TEST_S3_BUCKET", "demo"),
		"access_key":     os.Getenv("TEST_S3_ACCESS_KEY"),
		"secret_key":     os.Getenv("TEST_S3_SECRET_KEY"),
		"use_path_style": os.Getenv("TEST_S3_PATH_STYLE") == "true",
		"prefix":         "m2-write-smoke",
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := d.(driver.Writer)
	if !ok {
		t.Fatal("s3 驱动应实现 Writer")
	}
	ctx := context.Background()

	// 1. Mkdir + Upload
	if err := w.Mkdir(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if err := w.Upload(ctx, "dir/a.txt", strings.NewReader("AAA"), 3); err != nil {
		t.Fatal(err)
	}
	if err := w.Upload(ctx, "dir/sub/b.txt", strings.NewReader("BBB"), 3); err != nil {
		t.Fatal(err)
	}

	// 2. 读回验证
	rc, err := d.Open(ctx, "dir/a.txt", driver.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "AAA" {
		t.Fatalf("读回不符: %q", got)
	}

	// 3. Rename
	if err := w.Rename(ctx, "dir/a.txt", "dir/a2.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "dir/a2.txt"); err != nil {
		t.Fatal("重命名后应可 Stat")
	}
	if _, err := d.Stat(ctx, "dir/a.txt"); err != driver.ErrNotFound {
		t.Fatal("旧键应不存在")
	}

	// 4. 目录重命名
	if err := w.Rename(ctx, "dir/sub", "dir/sub2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "dir/sub2/b.txt"); err != nil {
		t.Fatal("目录重命名后内容应跟随")
	}

	// 5. 清理（目录递归删）
	if err := w.Delete(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if res, err := d.List(ctx, "", driver.ListOptions{}); err == nil {
		for _, e := range res.Entries {
			if strings.HasPrefix(e.Path, "dir") {
				t.Fatalf("清理不彻底: %s", e.Path)
			}
		}
	}
}

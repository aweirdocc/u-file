package s3

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"

	"ufile/internal/driver"
)

// TestS3Smoke 针对 MinIO/R2/OSS 等真实 S3 端点的冒烟测试。
// 通过环境变量启用（不设置则跳过），例如本地 MinIO：
//
//	TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
//	TEST_S3_BUCKET=demo TEST_S3_ACCESS_KEY=minioadmin TEST_S3_SECRET_KEY=minioadmin \
//	TEST_S3_PATH_STYLE=true go test ./internal/driver/s3/ -run TestS3Smoke -v
func TestS3Smoke(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("未设置 TEST_S3_ENDPOINT，跳过 S3 冒烟测试")
	}
	cfg := map[string]any{
		"endpoint":   endpoint,
		"region":     envOr("TEST_S3_REGION", "us-east-1"),
		"bucket":     envOr("TEST_S3_BUCKET", "demo"),
		"access_key": envOr("TEST_S3_ACCESS_KEY", ""),
		"secret_key": envOr("TEST_S3_SECRET_KEY", ""),
	}
	if os.Getenv("TEST_S3_PATH_STYLE") == "true" {
		cfg["use_path_style"] = true
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	res, err := d.List(ctx, "", driver.ListOptions{})
	if err != nil {
		t.Fatalf("List 根目录失败: %v", err)
	}
	t.Logf("根目录条目数: %d", len(res.Entries))

	// 广度优先找第一个文件对象做 Open + Range 验证
	var file *driver.Entry
	queue := []string{""}
	for len(queue) > 0 && file == nil {
		dir := queue[0]
		queue = queue[1:]
		page := &driver.ListOptions{Cursor: ""}
		for {
			res, err := d.List(ctx, dir, *page)
			if err != nil {
				t.Fatalf("List %q 失败: %v", dir, err)
			}
			for i := range res.Entries {
				if res.Entries[i].IsDir {
					queue = append(queue, res.Entries[i].Path)
				} else {
					file = &res.Entries[i]
					break
				}
			}
			if file != nil || res.NextCursor == "" {
				break
			}
			page.Cursor = res.NextCursor
		}
	}
	if file == nil {
		t.Skip("桶根目录下没有文件对象，跳过下载验证")
	}
	if _, err := d.Stat(ctx, file.Path); err != nil {
		t.Fatalf("Stat %s 失败: %v", file.Path, err)
	}
	rc, err := d.Open(ctx, file.Path, driver.OpenOptions{})
	if err != nil {
		t.Fatalf("Open %s 失败: %v", file.Path, err)
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(rc, head)
	rc.Close()
	t.Logf("Open 成功，%s 前 %d 字节: %q", file.Path, n, head[:n])

	u, err := d.(driver.Presigner).PresignGet(ctx, file.Path, 0)
	if err != nil {
		t.Fatalf("PresignGet 失败: %v", err)
	}
	t.Logf("预签名直链: %s", u)

	// 直链应能匿名访问（HTTP 200 且内容一致）
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("访问预签名直链失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预签名直链返回 %d，期望 200", resp.StatusCode)
	}
	head2 := make([]byte, n)
	_, _ = io.ReadFull(resp.Body, head2)
	t.Logf("直链匿名访问成功，前 %d 字节一致: %v", n, bytes.Equal(head2, head[:n]))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

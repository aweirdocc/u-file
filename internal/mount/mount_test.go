package mount

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"ufile/internal/driver"
	"ufile/internal/driver/local"
)

func TestVFSMountRoutingAndOperations(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootA, "2026", "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "2026", "contracts", "companyA.pdf"), []byte("contract-pdf-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "invoices"), 0o755); err != nil {
		t.Fatal(err)
	}

	drvA, _ := local.New(map[string]any{"root": rootA})
	drvB, _ := local.New(map[string]any{"root": rootB})

	mgr, err := NewManager(t.TempDir(), func(id string) (driver.FSDriver, error) {
		switch id {
		case "oss-finance":
			return drvA, nil
		case "sftp-tax":
			return drvB, nil
		default:
			return nil, errors.New("not found")
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. 注册两个挂载点（对齐 docs/06 5.1 示例）
	if _, err := mgr.Add(UpsertOptions{
		MountPath:  "/财务文档/合同",
		SourceID:   "oss-finance",
		SourcePath: "/2026/contracts",
		Readonly:   true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Add(UpsertOptions{
		MountPath:  "/财务文档/发票",
		SourceID:   "sftp-tax",
		SourcePath: "/invoices",
		Readonly:   false,
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 2. 列根目录：应自动合成 "财务文档" 虚拟目录
	rootList, err := mgr.List(ctx, "", driver.ListOptions{})
	if err != nil || len(rootList.Entries) != 1 || rootList.Entries[0].Name != "财务文档" {
		t.Fatalf("unexpected root list: %v %+v", err, rootList)
	}

	// 3. 列 "财务文档"：应自动合成 "合同" 与 "发票" 两个挂载目录
	subList, err := mgr.List(ctx, "财务文档", driver.ListOptions{})
	if err != nil || len(subList.Entries) != 2 {
		t.Fatalf("unexpected sub list: %v %+v", err, subList)
	}

	// 4. 读取 "/财务文档/合同/companyA.pdf"
	rc, err := mgr.Open(ctx, "财务文档/合同/companyA.pdf", driver.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "contract-pdf-bytes" {
		t.Fatalf("unexpected file content: %q", got)
	}

	// 5. 只读挂载点禁止写入
	err = mgr.Upload(ctx, "财务文档/合同/new.pdf", bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, ErrMountReadOnly) {
		t.Fatalf("expected ErrMountReadOnly, got %v", err)
	}

	// 6. 可写挂载点允许上传并直接落到物理源 sftp-tax/invoices/inv01.xml
	if err := mgr.Upload(ctx, "财务文档/发票/inv01.xml", bytes.NewReader([]byte("<inv/>")), 6); err != nil {
		t.Fatalf("upload to writable mount failed: %v", err)
	}
	diskBytes, err := os.ReadFile(filepath.Join(rootB, "invoices", "inv01.xml"))
	if err != nil || string(diskBytes) != "<inv/>" {
		t.Fatalf("physical file not written properly: %v %q", err, diskBytes)
	}
}

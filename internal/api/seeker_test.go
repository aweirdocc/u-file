package api

import (
	"io"
	"testing"

	"ufile/internal/driver"
)

type fakeDrv struct{ driver.FSDriver }

func TestReopenSeekerType(t *testing.T) {
	var drv driver.FSDriver = fakeDrv{}
	rc := io.NopCloser(io.Reader(nil))
	rs := wrapReadSeeker(t.Context(), drv, "x", rc, 100)
	if _, ok := rs.(io.ReadSeeker); !ok {
		t.Fatalf("wrapReadSeeker 结果未实现 io.ReadSeeker: %T", rs)
	}
}

func TestReopenSeekerSeekEnd(t *testing.T) {
	// http.ServeContent 的 sizeFunc 依赖 Seek(0, SeekEnd) 探测文件大小
	var drv driver.FSDriver = fakeDrv{}
	rc := io.NopCloser(io.Reader(nil))
	rs := wrapReadSeeker(t.Context(), drv, "x", rc, 100)
	n, err := rs.Seek(0, io.SeekEnd)
	if err != nil || n != 100 {
		t.Fatalf("SeekEnd 应返回 size=100，得到 n=%d err=%v", n, err)
	}
	n, err = rs.Seek(0, io.SeekStart)
	if err != nil || n != 0 {
		t.Fatalf("SeekStart 应回到 0，得到 n=%d err=%v", n, err)
	}
}

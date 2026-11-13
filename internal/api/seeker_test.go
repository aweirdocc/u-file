package api

import (
	"context"
	"io"
	"strings"
	"testing"

	"ufile/internal/driver"
)

type fakeDrv struct{ driver.FSDriver }

// trackingCloser 记录关闭次数、不可 Seek 的流（模拟 S3/WebDAV 等网络响应体）。
type trackingCloser struct {
	io.Reader
	closes int
}

func (c *trackingCloser) Close() error { c.closes++; return nil }

// openFakeDrv 每次返回同一个流，记录 Open 次数。
type openFakeDrv struct {
	driver.FSDriver
	rc  io.ReadCloser
	ops int
}

func (d *openFakeDrv) Open(_ context.Context, _ string, _ driver.OpenOptions) (io.ReadCloser, error) {
	d.ops++
	return d.rc, nil
}

// 回归测试：Range 断点续传触发 Seek 重开流后，包装器必须承担新流的关闭。
// 旧实现 serveInline 仅 defer rc.Close()（最初的流），重开流无人关闭 → 连接泄漏。
func TestReopenSeekerCloseClosesReopenedStream(t *testing.T) {
	first := &trackingCloser{Reader: strings.NewReader("0123456789")}
	reopened := &trackingCloser{Reader: strings.NewReader("0123456789")}
	drv := &openFakeDrv{rc: reopened}
	rs := wrapReadSeeker(t.Context(), drv, "x", first, 10)

	if _, ok := rs.(*reopenSeeker); !ok {
		t.Fatalf("不可 Seek 的流应返回 *reopenSeeker，得到 %T", rs)
	}

	buf := make([]byte, 4)
	if _, err := rs.Read(buf); err != nil { // 读最初流
		t.Fatal(err)
	}
	if _, err := rs.Seek(6, io.SeekStart); err != nil { // 关旧流
		t.Fatal(err)
	}
	if first.closes != 1 {
		t.Fatalf("Seek 后最初的流应恰好关闭 1 次，实际 %d 次", first.closes)
	}
	if _, err := rs.Read(buf); err != nil { // 重开新流并读取
		t.Fatal(err)
	}
	if drv.ops != 1 || reopened.closes != 0 {
		t.Fatalf("重开后 ops=1/未关闭，实际 ops=%d closes=%d", drv.ops, reopened.closes)
	}

	// serveInline 的关闭路径：对包装器做 io.Closer 断言后关闭
	cl, ok := rs.(io.Closer)
	if !ok {
		t.Fatal("reopenSeeker 应实现 io.Closer")
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened.closes != 1 {
		t.Fatalf("重开流应被关闭 1 次，实际 %d 次", reopened.closes)
	}
}

// 回归测试：Seek 之后未发生 Read（未重开）时，关闭包装器不应触发多余 Open，
// 最初的流也只关闭一次。
func TestReopenSeekerCloseWithoutReopen(t *testing.T) {
	first := &trackingCloser{Reader: strings.NewReader("0123456789")}
	reopened := &trackingCloser{Reader: strings.NewReader("0123456789")}
	drv := &openFakeDrv{rc: reopened}
	rs := wrapReadSeeker(t.Context(), drv, "x", first, 10)

	if _, err := rs.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := rs.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if drv.ops != 0 {
		t.Fatalf("不应触发重开，实际 Open %d 次", drv.ops)
	}
	if first.closes != 1 {
		t.Fatalf("最初的流应关闭 1 次，实际 %d 次", first.closes)
	}
}

// trackingFile 可 Seek 且记录关闭次数的流（模拟本地文件）。
type trackingFile struct {
	io.ReadSeeker
	closes int
}

func (f *trackingFile) Close() error { f.closes++; return nil }

// 可 Seek 的流（本地文件）：包装器 Close 应落到原始流且恰好一次。
func TestWrapReadSeekerSeekableClose(t *testing.T) {
	ts := &trackingFile{ReadSeeker: strings.NewReader("0123456789")}
	rs := wrapReadSeeker(t.Context(), &fakeDrv{}, "x", ts, 10)
	if _, ok := rs.(io.Closer); !ok {
		t.Fatal("可 Seek 包装应保留 Closer")
	}
	if err := rs.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if ts.closes != 1 {
		t.Fatalf("原始流应关闭 1 次，实际 %d 次", ts.closes)
	}
}

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

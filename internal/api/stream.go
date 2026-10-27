package api

import (
	"context"
	"fmt"
	"io"

	"ufile/internal/driver"
)

// readSeekCloser 组合已有能力（如 os.File 本身可 Seek，直接复用）。
type readSeekCloser struct {
	io.ReadCloser
	io.Seeker
}

// wrapReadSeeker 把驱动返回的流包装成 http.ServeContent 要求的 io.ReadSeeker：
//   - 流本身可 Seek（本地文件）→ 直接复用；
//   - 流不可 Seek（S3 响应体等网络流）→ 用“重开流”实现 Seek：
//     Seek 到新位置时关闭旧流、以该 Offset 重新 Open。
//     这样 ServeContent 的 Range/断点续传能力得以保留，且全程不整载内存。
//
// size 是该文件的已知大小（来自 Stat）。ServeContent 的 sizeFunc 会
// Seek(0, SeekEnd) 求文件大小，reopenSeeker 依赖 size 支持该语义。
func wrapReadSeeker(ctx context.Context, drv driver.FSDriver, path string, rc io.ReadCloser, size int64) io.ReadSeeker {
	if sk, ok := rc.(io.Seeker); ok {
		return readSeekCloser{ReadCloser: rc, Seeker: sk}
	}
	return &reopenSeeker{ctx: ctx, drv: drv, path: path, rc: rc, pos: 0, size: size}
}

// reopenSeeker 以“关闭旧流 + 按新 Offset 重开”实现 Seek。
type reopenSeeker struct {
	ctx  context.Context
	drv  driver.FSDriver
	path string
	rc   io.ReadCloser
	pos  int64
	size int64
}

func (r *reopenSeeker) Read(p []byte) (int, error) {
	if r.rc == nil {
		rc, err := r.drv.Open(r.ctx, r.path, driver.OpenOptions{Offset: r.pos})
		if err != nil {
			return 0, err
		}
		r.rc = rc
	}
	return r.rc.Read(p)
}

func (r *reopenSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		// ServeContent 用 Seek(0, SeekEnd) 探测文件大小；用 Stat 的 size 换算
		if offset != 0 {
			return 0, fmt.Errorf("reopenSeeker: SeekEnd 仅支持 offset=0，得到 %d", offset)
		}
		return r.size, nil
	default:
		return 0, fmt.Errorf("reopenSeeker: unsupported whence %d", whence)
	}
	if offset != r.pos && r.rc != nil {
		_ = r.rc.Close()
		r.rc = nil
	}
	r.pos = offset
	return r.pos, nil
}

func (r *reopenSeeker) Close() error {
	if r.rc != nil {
		return r.rc.Close()
	}
	return nil
}

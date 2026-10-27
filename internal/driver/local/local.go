// Package local 本地磁盘文件源。
// M0 用作联调与冒烟的基准实现；同样遵循“Open 只返回流”的驱动约定。
package local

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ufile/internal/driver"
)

func init() { driver.Register("local", New) }

type Local struct{ root string }

func New(cfg map[string]any) (driver.FSDriver, error) {
	root, err := driver.RequiredStr(cfg, "root")
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New("local: root 不是目录")
	}
	return &Local{root: abs}, nil
}

func (d *Local) Scheme() string { return "local" }

func (d *Local) Capabilities() driver.Capabilities {
	return driver.Capabilities{List: true, Stat: true, Download: true}
}

// resolve 把驱动相对路径映射为 root 下的绝对路径。
// CleanPath 已拒绝 .. 与反斜杠；此处再兜底校验前缀，防符号链接/盘符巧合越界。
func (d *Local) resolve(p string) (string, error) {
	rel, err := driver.CleanPath(p)
	if err != nil {
		return "", err
	}
	full := filepath.Join(d.root, filepath.FromSlash(rel))
	if rel != "" && !strings.HasPrefix(full, d.root+string(filepath.Separator)) {
		return "", driver.ErrInvalidPath
	}
	return full, nil
}

func (d *Local) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	full, err := d.resolve(dir)
	if err != nil {
		return nil, err
	}
	parent, err := driver.CleanPath(dir)
	if err != nil {
		return nil, err
	}
	dirents, err := os.ReadDir(full)
	if err != nil {
		return nil, mapErr(err)
	}
	res := &driver.ListResult{Entries: make([]driver.Entry, 0, len(dirents))}
	for _, de := range dirents {
		info, err := de.Info()
		if err != nil {
			continue // 条目在列取瞬间消失（被删除等），跳过
		}
		child := de.Name()
		if parent != "" {
			child = parent + "/" + de.Name()
		}
		res.Entries = append(res.Entries, driver.Entry{
			Path:    child,
			Name:    de.Name(),
			IsDir:   de.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return res, nil
}

func (d *Local) Stat(ctx context.Context, path string) (*driver.Entry, error) {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return nil, err
	}
	full, err := d.resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, mapErr(err)
	}
	return &driver.Entry{
		Path:    rel,
		Name:    baseName(rel),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

func (d *Local) Open(ctx context.Context, path string, opt driver.OpenOptions) (io.ReadCloser, error) {
	full, err := d.resolve(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, mapErr(err)
	}
	if opt.Offset > 0 {
		if _, err := f.Seek(opt.Offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	var rc io.ReadCloser = f
	if opt.Length > 0 {
		// 注意不能用 io.NopCloser（Close 是空操作，会泄漏底层文件句柄）
		rc = limitedCloser{Reader: io.LimitReader(f, opt.Length), Closer: f}
	}
	return rc, nil
}

// limitedCloser 把 LimitReader 与底层 Closer 组合成一个 ReadCloser。
type limitedCloser struct {
	io.Reader
	io.Closer
}

func mapErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return driver.ErrNotFound
	}
	return err
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

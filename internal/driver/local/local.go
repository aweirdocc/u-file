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
	return driver.Capabilities{List: true, Stat: true, Download: true, Write: true}
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

// —— 写能力（Writer）——

// Upload 写入文件（父目录不存在时自动创建；覆盖已存在文件）。
func (d *Local) Upload(ctx context.Context, path string, r io.Reader, size int64) error {
	full, err := d.resolve(path)
	if err != nil {
		return err
	}
	if full == filepath.Clean(d.root) {
		return driver.ErrInvalidPath // 不允许覆盖源根
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return mapErr(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		os.Remove(full) // 写一半失败不留半个文件
		return err
	}
	return nil
}

// Delete 删除文件或目录（目录递归删除）。根目录不可删。
func (d *Local) Delete(ctx context.Context, path string) error {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return err
	}
	if rel == "" {
		return driver.ErrInvalidPath // 源根不可删
	}
	full, err := d.resolve(path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil {
		return mapErr(err)
	}
	return nil
}

// Mkdir 创建目录（含父目录）。
func (d *Local) Mkdir(ctx context.Context, path string) error {
	full, err := d.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return mapErr(err)
	}
	return nil
}

// Rename 重命名/移动（目标已存在文件时覆盖；目标为非空目录时报错）。
func (d *Local) Rename(ctx context.Context, from, to string) error {
	src, err := d.resolve(from)
	if err != nil {
		return err
	}
	if src == filepath.Clean(d.root) {
		return driver.ErrInvalidPath
	}
	dst, err := d.resolve(to)
	if err != nil {
		return err
	}
	if dst == filepath.Clean(d.root) || strings.HasPrefix(dst+string(filepath.Separator), src+string(filepath.Separator)) {
		return driver.ErrInvalidPath // 不能把目录移进自己
	}
	if _, err := os.Stat(src); err != nil {
		return mapErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		// 跨盘符等场景 Rename 失败，退化为复制+删除
		if info, statErr := os.Stat(src); statErr == nil && info.IsDir() {
			return err // 目录跨盘复制复杂，直接报错（M2 边界）
		}
		return d.copyFileFallback(src, dst, err)
	}
	return nil
}

func (d *Local) copyFileFallback(src, dst string, origErr error) error {
	in, err := os.Open(src)
	if err != nil {
		return origErr
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
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

// Package ftp FTP / FTPS 文件源（jlaffaye/ftp）。
//
// 配置：host, port(默认21), username, password, tls(true=FTPS 显式 TLS),
// insecure_tls(跳过证书校验), root(默认根目录)。
// 连接策略：每个驱动实例持有一个连接池（这里简化为互斥的单连接 + 按需重拨）——
// FTP 协议单连接串行，够 MVP/M2 用；高并发场景后续再扩池。
package ftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jlaffaye/ftp"

	"ufile/internal/driver"
)

func init() { driver.Register("ftp", New) }

type FTP struct {
	mu      sync.Mutex
	conn    *ftp.ServerConn
	host    string
	port    int
	user    string
	pass    string
	tls     bool
	insec   bool
	root    string // 源根前缀，非空时以 / 结尾
	lastUse time.Time
}

func New(cfg map[string]any) (driver.FSDriver, error) {
	host, err := driver.RequiredStr(cfg, "host")
	if err != nil {
		return nil, err
	}
	port := int(driver.Int(cfg, "port", 21))
	if port <= 0 {
		port = 21
	}
	root := strings.Trim(driver.Str(cfg, "root", ""), "/")
	if root != "" {
		root += "/"
	}
	if driver.Bool(cfg, "tls", false) && driver.Bool(cfg, "insecure_tls", false) {
		log.Printf("[warn] FTPS 源 %s 已关闭证书校验（insecure_tls=true），连接易遭中间人攻击", host)
	}
	return &FTP{
		host:  host,
		port:  port,
		user:  driver.Str(cfg, "username", "anonymous"),
		pass:  driver.Str(cfg, "password", "anonymous"),
		tls:   driver.Bool(cfg, "tls", false),
		insec: driver.Bool(cfg, "insecure_tls", false),
		root:  root,
	}, nil
}

func (d *FTP) Scheme() string { return "ftp" }

func (d *FTP) Capabilities() driver.Capabilities {
	return driver.Capabilities{List: true, Stat: true, Download: true, Write: true}
}

// dial 建立新连接。
func (d *FTP) dial(ctx context.Context) (*ftp.ServerConn, error) {
	addr := fmt.Sprintf("%s:%d", d.host, d.port)
	// 自定义拨号：TCP 连接 + banner/USER/PASS/TYPE 交换整体限时，
	// 防“可连但不响应”的服务器使拨号 goroutine 永久阻塞
	handshake := &stuckDeadlineConn{}
	dialFunc := func(network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		handshake.Conn = c
		_ = handshake.SetDeadline(time.Now().Add(dialTimeout))
		return handshake, nil
	}
	opts := []ftp.DialOption{ftp.DialWithDialFunc(dialFunc)}
	if d.tls {
		opts = append(opts, ftp.DialWithTLS(implicitTLSConfig(d.insec)))
	}
	// ctx 感知：并发拨号时超时取消
	type res struct {
		c   *ftp.ServerConn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ftp.Dial(addr, opts...)
		if err != nil {
			ch <- res{err: err}
			return
		}
		if err := c.Login(d.user, d.pass); err != nil {
			c.Quit()
			ch <- res{err: fmt.Errorf("登录失败: %w", err)}
			return
		}
		// 二进制模式（避免 ASCII 传输破坏二进制文件）
		_ = c.Type(ftp.TransferTypeBinary)
		handshake.release() // 握手完成，解除限时
		ch <- res{c: c}
	}()
	select {
	case <-ctx.Done():
		// 后台拨号仍在继续：成功建连的连接由回收 goroutine 关闭，避免孤儿连接泄漏
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Quit()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.c, r.err
	}
}

// 控制连接操作的兜底上限：远端挂起时强制断连，避免调用方永久阻塞。
const (
	opTimeout    = 2 * time.Minute
	probeTimeout = 30 * time.Second
)

// dialTimeout TCP 连接 + 握手（banner/登录/TYPE）整体限时；var 仅为测试可覆盖。
var dialTimeout = 15 * time.Second

// stuckDeadlineConn 握手期限连接：release 后所有 SetDeadline 请求归零，
// 使库在传输收尾时设置的临时 deadline 不再受握手时限影响（库不暴露底层连接，
// 只能在自定义拨号层包一层）。
type stuckDeadlineConn struct {
	net.Conn
	released atomic.Bool
}

func (c *stuckDeadlineConn) release() { c.released.Store(true) }

func (c *stuckDeadlineConn) SetDeadline(t time.Time) error {
	if c.released.Load() {
		t = time.Time{}
	}
	return c.Conn.SetDeadline(t)
}

// withConn 串行执行一次控制连接操作，探活失败或连接级错误时重拨重试一次。
// 持锁贯穿全程：FTP 控制连接不可并发复用，命令交叉会串话。timeout 为单次
// 操作的兜底上限；时长由数据量决定的上传传 0（仅感知 ctx 取消）。
func (d *FTP) withConn(ctx context.Context, timeout time.Duration, fn func(c *ftp.ServerConn) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if c := d.conn; c != nil {
		if err := driver.RunWatchdog(ctx, probeTimeout, func() { go c.Quit() }, c.NoOp); err == nil {
			if err := d.runOpLocked(ctx, c, timeout, fn); !isConnErr(err) && !isAbort(err) {
				return err
			}
		}
		// 探活失败或连接已死：重拨
		_ = c.Quit()
		d.conn = nil
	}
	c2, err := d.dial(ctx)
	if err != nil {
		return err
	}
	d.conn = c2
	return d.runOpLocked(ctx, c2, timeout, fn)
}

// runOpLocked 执行单次操作；连接级错误或看门狗中止时丢弃池内引用（调用方必须持有 d.mu）。
func (d *FTP) runOpLocked(ctx context.Context, c *ftp.ServerConn, timeout time.Duration, fn func(c *ftp.ServerConn) error) error {
	err := driver.RunWatchdog(ctx, timeout, func() { go c.Quit() }, func() error { return fn(c) })
	if err != nil && (isConnErr(err) || isAbort(err)) && d.conn == c {
		_ = c.Quit()
		d.conn = nil
	}
	return err
}

// isAbort 判断错误是否来自看门狗中止（连接已被 kill 作废，需重拨）。
func isAbort(err error) bool {
	var ae *driver.AbortError
	return errors.As(err, &ae)
}

func isConnErr(err error) bool {
	var te *textproto.Error
	_ = te // 协议错误不算连接错误
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "reset")
}

func (d *FTP) full(p string) (string, error) {
	rel, err := driver.CleanPath(p)
	if err != nil {
		return "", err
	}
	return "/" + d.root + rel, nil // FTP 绝对路径
}

func (d *FTP) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	full, err := d.full(dir)
	if err != nil {
		return nil, err
	}
	rel, _ := driver.CleanPath(dir)
	var entries []driver.Entry
	err = d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error {
		list, err := c.List(full)
		if err != nil {
			return err
		}
		entries = make([]driver.Entry, 0, len(list))
		for _, it := range list {
			name := path.Base(it.Name)
			if name == "." || name == ".." || it.Name == full {
				continue
			}
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			e := driver.Entry{
				Path:  child,
				Name:  name,
				IsDir: it.Type == ftp.EntryTypeFolder,
				Size:  int64(it.Size),
			}
			e.ModTime = it.Time
			entries = append(entries, e)
		}
		return nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("ftp: List %s: %w", full, err)
	}
	return &driver.ListResult{Entries: entries}, nil
}

func (d *FTP) Stat(ctx context.Context, p string) (*driver.Entry, error) {
	rel, err := driver.CleanPath(p)
	if err != nil {
		return nil, err
	}
	if rel == "" {
		return &driver.Entry{IsDir: true}, nil
	}
	full, err := d.full(p)
	if err != nil {
		return nil, err
	}
	var entry *driver.Entry
	err = d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error {
		// MLST 优先（RFC 3659），退化用 List 父目录
		if e, err := c.GetEntry(full); err == nil && e != nil {
			entry = toEntry(rel, e)
			return nil
		}
		parent := path.Dir(full)
		list, err := c.List(parent)
		if err != nil {
			return err
		}
		for _, it := range list {
			if it.Name == full || path.Base(it.Name) == path.Base(full) {
				entry = toEntry(rel, it)
				return nil
			}
		}
		return driver.ErrNotFound
	})
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, driver.ErrNotFound
	}
	return entry, nil
}

func toEntry(rel string, it *ftp.Entry) *driver.Entry {
	name := path.Base(rel)
	e := &driver.Entry{
		Path:  rel,
		Name:  name,
		IsDir: it.Type == ftp.EntryTypeFolder,
		Size:  int64(it.Size),
	}
	e.ModTime = it.Time
	return e
}

func (d *FTP) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	full, err := d.full(p)
	if err != nil {
		return nil, err
	}
	if rel, _ := driver.CleanPath(p); rel == "" {
		return nil, driver.ErrInvalidPath
	}
	var rc io.ReadCloser
	// 每次下载专用连接：FTP 控制连接上的数据传输会互踩，不复用池内连接
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, err
	}
	// RETR 命令交换阶段（数据连接建立）受看门狗保护；之后的数据流读取
	// 由 pooledReadCloser 生命周期管理（挂起的下载随请求 ctx/连接关闭终止）
	var r *ftp.Response
	werr := driver.RunWatchdog(ctx, probeTimeout, func() { go conn.Quit() }, func() error {
		var err error
		r, err = conn.RetrFrom(full, uint64(max0(opt.Offset)))
		return err
	})
	if werr != nil {
		go conn.Quit()
		if isNotFound(werr) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("ftp: Retr %s: %w", full, werr)
	}
	rc = &pooledReadCloser{ReadCloser: r, conn: conn}
	return rc, nil
}

func max0(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// pooledReadCloser 关闭流的同时断开专用连接。
type pooledReadCloser struct {
	io.ReadCloser
	conn *ftp.ServerConn
}

func (p *pooledReadCloser) Close() error {
	err := p.ReadCloser.Close()
	p.conn.Quit()
	return err
}

// —— 写能力 ——

func (d *FTP) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	full, err := d.full(p)
	if err != nil {
		return err
	}
	// 父目录确保存在（走控制连接池）
	if err := d.ensureDir(ctx, path.Dir(full)); err != nil {
		return err
	}
	// 传输走专用连接：大文件传输时长不定，不长期占用控制连接池，
	// ctx 取消时由看门狗强制断连终止传输
	conn, err := d.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { go conn.Quit() }()
	return driver.RunWatchdog(ctx, 0, func() { go conn.Quit() }, func() error {
		return conn.Stor(full, r)
	})
}

func (d *FTP) Delete(ctx context.Context, p string) error {
	rel, err := driver.CleanPath(p)
	if err != nil {
		return err
	}
	if rel == "" {
		return driver.ErrInvalidPath
	}
	full, err := d.full(p)
	if err != nil {
		return err
	}
	// 先试文件
	ferr := d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error { return c.Delete(full) })
	if ferr == nil {
		return nil
	}
	// 再按目录递归删
	derr := d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error { return c.RemoveDirRecur(full) })
	if derr == nil {
		return nil
	}
	if isNotFound(ferr) && isNotFound(derr) {
		return driver.ErrNotFound
	}
	return derr
}

func (d *FTP) Mkdir(ctx context.Context, p string) error {
	full, err := d.full(p)
	if err != nil {
		return err
	}
	return d.ensureDir(ctx, full)
}

// ensureDir 逐级创建目录（已存在忽略）。
func (d *FTP) ensureDir(ctx context.Context, full string) error {
	full = strings.TrimSuffix(full, "/")
	if full == "" || full == "/" {
		return nil
	}
	return d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error {
		return ensureDirOn(c, full)
	})
}

// ensureDirOn 在指定连接上逐级创建目录（已存在忽略）。
func ensureDirOn(c *ftp.ServerConn, full string) error {
	parts := strings.Split(strings.TrimPrefix(full, "/"), "/")
	cur := ""
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		if err := c.MakeDir(cur); err != nil {
			// 已存在则忽略
			if _, lerr := c.List(cur); lerr != nil {
				return fmt.Errorf("ftp: MKD %s: %w", cur, err)
			}
		}
	}
	return nil
}

func (d *FTP) Rename(ctx context.Context, from, to string) error {
	fromRel, err := driver.CleanPath(from)
	if err != nil {
		return err
	}
	toRel, err := driver.CleanPath(to)
	if err != nil {
		return err
	}
	if toRel == fromRel || strings.HasPrefix(toRel+"/", fromRel+"/") {
		return driver.ErrInvalidPath
	}
	src, err := d.full(from)
	if err != nil {
		return err
	}
	dst, err := d.full(to)
	if err != nil {
		return err
	}
	if err := d.ensureDir(ctx, path.Dir(dst)); err != nil {
		return err
	}
	return d.withConn(ctx, opTimeout, func(c *ftp.ServerConn) error { return c.Rename(src, dst) })
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code == 550 || te.Code == 450
	}
	msg := err.Error()
	return strings.Contains(msg, "550") || strings.Contains(msg, "not found") || strings.Contains(msg, "No such")
}

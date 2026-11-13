// Package sftp SFTP 文件源（SSH File Transfer Protocol，pkg/sftp）。
//
// 配置：host, port(默认22), username,
//   - password 认证：password
//   - 私钥认证：private_key（PEM 内容或文件路径，二选一）+ private_key_passphrase（可选）
//
// host_key_policy: "accept-new"（默认：未知主机记录告警并接受）/ "insecure"（不校验）。
// 说明：M2 简化实现 known_hosts 追加式记录（accept-new 语义），完整 TOFU 校验后续强化。
// 连接策略：每驱动实例持有单个 SSH 会话 + 互斥复用，断线重拨；下载与 FTP 同理
// 不独占（SFTP 子系统支持单连接多路并发请求，无 FTP 数据通道问题）。
package sftpdrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"ufile/internal/driver"
)

func init() { driver.Register("sftp", New) }

type SFTP struct {
	mu       sync.Mutex
	client   *sftp.Client
	host     string
	port     int
	user     string
	password string
	privKey  string // PEM 内容或文件路径
	keyPass  string
	root     string // 源根前缀，非空时以 / 结尾
	known    map[string]bool
}

func New(cfg map[string]any) (driver.FSDriver, error) {
	host, err := driver.RequiredStr(cfg, "host")
	if err != nil {
		return nil, err
	}
	port := int(driver.Int(cfg, "port", 22))
	if port <= 0 {
		port = 22
	}
	user := driver.Str(cfg, "username", "")
	if user == "" {
		return nil, errors.New("sftp: 缺少 username")
	}
	if driver.Str(cfg, "password", "") == "" && driver.Str(cfg, "private_key", "") == "" {
		return nil, errors.New("sftp: 需要配置 password 或 private_key 之一")
	}
	root := strings.Trim(driver.Str(cfg, "root", ""), "/")
	if root != "" {
		root += "/"
	}
	return &SFTP{
		host:     host,
		port:     port,
		user:     user,
		password: driver.Str(cfg, "password", ""),
		privKey:  driver.Str(cfg, "private_key", ""),
		keyPass:  driver.Str(cfg, "private_key_passphrase", ""),
		root:     root,
		known:    map[string]bool{},
	}, nil
}

func (d *SFTP) Scheme() string { return "sftp" }

func (d *SFTP) Capabilities() driver.Capabilities {
	return driver.Capabilities{List: true, Stat: true, Download: true, Write: true}
}

// authMethods 组装认证方式（密码 + 私钥，按配置取交集的可用项）。
func (d *SFTP) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if d.password != "" {
		methods = append(methods, ssh.Password(d.password))
	}
	if d.privKey != "" {
		pemBytes, err := loadKeyBytes(d.privKey)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if d.keyPass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pemBytes, []byte(d.keyPass))
		} else {
			signer, err = ssh.ParsePrivateKey(pemBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("sftp: 解析私钥失败: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(methods) == 0 {
		return nil, errors.New("sftp: 需要配置 password 或 private_key 之一")
	}
	return methods, nil
}

// loadKeyBytes 私钥参数既支持 PEM 原文也支持文件路径。
func loadKeyBytes(s string) ([]byte, error) {
	if strings.Contains(s, "-----BEGIN") {
		return []byte(s), nil
	}
	raw, err := os.ReadFile(s)
	if err != nil {
		// 不是文件路径也不是 PEM → 报可读错误
		if !pemContains([]byte(s)) && filepath.IsAbs(s) || os.IsNotExist(err) {
			return nil, fmt.Errorf("sftp: 私钥文件读取失败 %s: %w", s, err)
		}
		return nil, fmt.Errorf("sftp: private_key 既不是 PEM 内容也不是可读文件路径")
	}
	return raw, nil
}

func pemContains(b []byte) bool { return strings.Contains(string(b), "-----BEGIN") }

// 控制通道操作的兜底上限：远端挂起时强制断连，避免调用方永久阻塞。
const (
	opTimeout    = 2 * time.Minute
	probeTimeout = 30 * time.Second
)

// runOp 取客户端并带看门狗执行一次操作。pkg/sftp 客户端本身并发安全，
// 故不加驱动级串行；超时/取消时关闭共享客户端（整体作废，其余在途操作
// 会一次性报错，下次操作探活失败自动重拨）。
func (d *SFTP) runOp(ctx context.Context, timeout time.Duration, fn func(c *sftp.Client) error) error {
	cli, err := d.getClient(ctx)
	if err != nil {
		return err
	}
	err = driver.RunWatchdog(ctx, timeout, func() { _ = cli.Close() }, func() error { return fn(cli) })
	if err != nil && (isAbort(err) || isConnErr(err)) {
		// 连接已作废：丢弃池内引用
		d.mu.Lock()
		if d.client == cli {
			_ = cli.Close()
			d.client = nil
		}
		d.mu.Unlock()
	}
	return err
}

// getClient 取（必要时重拨）SFTP 客户端。
func (d *SFTP) getClient(ctx context.Context) (*sftp.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		// 探活（受看门狗保护：远端挂起时强制断连，不阻塞后续操作）
		cli := d.client
		if err := driver.RunWatchdog(ctx, probeTimeout, func() { _ = cli.Close() }, func() error {
			_, err := cli.Stat("/")
			return err
		}); err == nil {
			return cli, nil
		}
		_ = cli.Close()
		d.client = nil
	}
	addr := net.JoinHostPort(d.host, fmt.Sprint(d.port))
	methods, err := d.authMethods()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            d.user,
		Auth:            methods,
		HostKeyCallback: d.hostKeyCallback(),
		Timeout:         15 * time.Second,
	}
	type res struct {
		c *sftp.Client
		e error
	}
	ch := make(chan res, 1)
	go func() {
		// 自定义握手限时：ssh.Dial 的 Timeout 只覆盖 TCP 连接，
		// 版本交换/KEX/认证无期限，静默服务器会永久阻塞
		tcp, err := net.DialTimeout("tcp", addr, cfg.Timeout)
		if err != nil {
			ch <- res{e: fmt.Errorf("sftp: SSH 连接失败: %w", err)}
			return
		}
		_ = tcp.SetDeadline(time.Now().Add(cfg.Timeout))
		conn, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
		if err != nil {
			_ = tcp.Close()
			ch <- res{e: fmt.Errorf("sftp: SSH 连接失败: %w", err)}
			return
		}
		client := ssh.NewClient(conn, chans, reqs)
		c, err := sftp.NewClient(client)
		if err != nil {
			client.Close()
			ch <- res{e: fmt.Errorf("sftp: 建立子系统失败: %w", err)}
			return
		}
		_ = tcp.SetDeadline(time.Time{}) // 握手完成，解除限时（后续操作由看门狗兜底）
		ch <- res{c: c}
	}()
	select {
	case <-ctx.Done():
		// 后台拨号仍在继续：成功建连的客户端由回收 goroutine 关闭，避免孤儿连接泄漏
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		if r.e != nil {
			return nil, r.e
		}
		d.client = r.c
		return r.c, nil
	}
}

func (d *SFTP) hostKeyCallback() ssh.HostKeyCallback {
	// accept-new 语义：首次记录，之后必须匹配；insecure 则不校验
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if key == nil {
			return errors.New("sftp: 服务器未提供主机密钥")
		}
		id := string(key.Marshal())
		if d.known[hostname+"|"+id] {
			return nil
		}
		firstForHost := true
		for k := range d.known {
			if strings.HasPrefix(k, hostname+"|") {
				firstForHost = false
				break
			}
		}
		if firstForHost {
			d.known[hostname+"|"+id] = true
			return nil
		}
		// 主机密钥与首次记录不一致
		return fmt.Errorf("sftp: 主机密钥与首次连接记录不一致（可能为中间人攻击或服务器重装）: %s", hostname)
	}
}

func (d *SFTP) full(p string) (string, error) {
	rel, err := driver.CleanPath(p)
	if err != nil {
		return "", err
	}
	return "/" + d.root + rel, nil
}

func (d *SFTP) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	full, err := d.full(dir)
	if err != nil {
		return nil, err
	}
	rel, _ := driver.CleanPath(dir)
	var infos []os.FileInfo
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		var err error
		infos, err = c.ReadDir(full)
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("sftp: ReadDir %s: %w", full, err)
	}
	res := &driver.ListResult{Entries: make([]driver.Entry, 0, len(infos))}
	for _, info := range infos {
		child := info.Name()
		if rel != "" {
			child = rel + "/" + info.Name()
		}
		res.Entries = append(res.Entries, driver.Entry{
			Path:    child,
			Name:    info.Name(),
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return res, nil
}

func (d *SFTP) Stat(ctx context.Context, p string) (*driver.Entry, error) {
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
	var info os.FileInfo
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		var err error
		info, err = c.Stat(full)
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("sftp: Stat %s: %w", full, err)
	}
	return &driver.Entry{
		Path:    rel,
		Name:    path.Base(rel),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

func (d *SFTP) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	full, err := d.full(p)
	if err != nil {
		return nil, err
	}
	if rel, _ := driver.CleanPath(p); rel == "" {
		return nil, driver.ErrInvalidPath
	}
	var f *sftp.File
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		var err error
		f, err = c.Open(full)
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("sftp: Open %s: %w", full, err)
	}
	if opt.Offset > 0 {
		if _, err := f.Seek(opt.Offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// —— 写能力 ——

func (d *SFTP) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	full, err := d.full(p)
	if err != nil {
		return err
	}
	var f *sftp.File
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		if err := ensureDir(c, path.Dir(full)); err != nil {
			return err
		}
		var err error
		f, err = c.Create(full) // 覆盖写
		return err
	})
	if err != nil {
		return fmt.Errorf("sftp: Create %s: %w", full, err)
	}
	// 数据流传输不设墙上时钟（时长由数据量决定），仅感知 ctx 取消；
	// 取消时关闭共享客户端终止写入（其余在途操作会报错并自动重拨）
	werr := driver.RunWatchdog(ctx, 0, func() { _ = f.Close() }, func() error {
		_, err := io.Copy(f, r)
		return err
	})
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("sftp: 写入 %s: %w", full, werr)
	}
	if cerr != nil {
		return fmt.Errorf("sftp: 关闭 %s: %w", full, cerr)
	}
	return nil
}

func (d *SFTP) Delete(ctx context.Context, p string) error {
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
	var info os.FileInfo
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		var err error
		info, err = c.Stat(full)
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return driver.ErrNotFound
		}
		return err
	}
	if info.IsDir() {
		return d.runOp(ctx, opTimeout, func(c *sftp.Client) error { return removeAll(c, full) })
	}
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error { return c.Remove(full) })
	if err != nil {
		return fmt.Errorf("sftp: Remove %s: %w", full, err)
	}
	return nil
}

func (d *SFTP) Mkdir(ctx context.Context, p string) error {
	full, err := d.full(p)
	if err != nil {
		return err
	}
	return d.runOp(ctx, opTimeout, func(c *sftp.Client) error { return ensureDir(c, full) })
}

func (d *SFTP) Rename(ctx context.Context, from, to string) error {
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
	err = d.runOp(ctx, opTimeout, func(c *sftp.Client) error {
		if err := ensureDir(c, path.Dir(dst)); err != nil {
			return err
		}
		if err := c.PosixRename(src, dst); err != nil {
			// PosixRename 不被部分服务器支持时退化为 stat+rename（不可覆盖语义）
			if _, serr := c.Stat(dst); serr == nil {
				return fmt.Errorf("sftp: 目标已存在且服务器不支持覆盖重命名: %w", err)
			}
			return fmt.Errorf("sftp: Rename %s→%s: %w", src, dst, err)
		}
		return nil
	})
	return err
}

// ensureDir 逐级创建目录（已存在忽略）。
func ensureDir(cli *sftp.Client, full string) error {
	full = strings.TrimSuffix(full, "/")
	if full == "" || full == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(full, "/"), "/")
	cur := ""
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		if _, err := cli.Stat(cur); err == nil {
			continue
		}
		if err := cli.Mkdir(cur); err != nil {
			// 并发创建等场景：再探一次
			if _, serr := cli.Stat(cur); serr != nil {
				return fmt.Errorf("sftp: Mkdir %s: %w", cur, err)
			}
		}
	}
	return nil
}

// removeAll 递归删除目录。
func removeAll(cli *sftp.Client, full string) error {
	infos, err := cli.ReadDir(full)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	for _, info := range infos {
		child := full + "/" + info.Name()
		if info.IsDir() {
			if err := removeAll(cli, child); err != nil {
				return err
			}
		} else if err := cli.Remove(child); err != nil {
			return err
		}
	}
	return cli.RemoveDirectory(full)
}

// isAbort 判断错误是否来自看门狗中止（共享客户端已被关闭，需重拨）。
func isAbort(err error) bool {
	var ae *driver.AbortError
	return errors.As(err, &ae)
}

// isConnErr 判断是否连接级错误（传输通道已断，需重拨）。
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "reset")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not exist") ||
		strings.Contains(err.Error(), "no such file")
}

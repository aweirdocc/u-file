package sftpdrv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// 回归测试：ctx 取消时 getClient 必须立即返回（不跟随后台拨号），
// 且不污染驱动状态——随后再次调用仍能正常发起拨号。
// 注：完整 SSH 握手需要真实服务端（由环境变量门控的 LiveSmoke 承担），
// 此处用“可连但静默”的服务器验证取消路径。
func TestGetClientCtxCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept() // 接受后保持静默（不发 SSH 版本串）
		if err == nil {
			time.Sleep(10 * time.Second)
			_ = c.Close()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	d, err := New(map[string]any{"host": host, "port": port, "username": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	f := d.(*SFTP)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.getClient(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时，实际 %v", err)
	}

	// 取消后驱动状态未被污染，仍可再次拨号
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if _, err := f.getClient(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("再次拨号应同样立即返回，实际 %v", err)
	}
}

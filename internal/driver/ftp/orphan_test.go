package ftp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeFTPServer 最小 FTP 服务器：按 jlaffaye/ftp 的握手序列应答
// （banner→USER→PASS→FEAT→TYPE→OPTS UTF8），并记录是否收到 QUIT。
type fakeFTPServer struct {
	ln       net.Listener
	quitSeen chan string
}

func newFakeFTPServer(t *testing.T) *fakeFTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeFTPServer{ln: ln, quitSeen: make(chan string, 1)}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeFTPServer) addr() string { return s.ln.Addr().String() }

func (s *fakeFTPServer) serve() {
	c, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	// 延迟响应：制造“ctx 先取消、拨号后成功”的孤儿拨号场景
	time.Sleep(300 * time.Millisecond)

	r := bufio.NewReader(c)
	send := func(line string) { _, _ = c.Write([]byte(line + "\r\n")) }
	send("220 fake ready")
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(cmd, "USER"):
			send("331 need password")
		case strings.HasPrefix(cmd, "PASS"):
			send("230 logged in")
		case cmd == "FEAT":
			send("211-Features:")
			send("211 End")
		case strings.HasPrefix(cmd, "TYPE"), strings.HasPrefix(cmd, "OPTS"):
			send("200 ok")
		case strings.HasPrefix(cmd, "QUIT"):
			select {
			case s.quitSeen <- cmd:
			default:
			}
			return
		default:
			send("502 not implemented")
		}
	}
}

// 回归测试：ctx 取消时 dial 必须立即返回，且后台拨号成功建连后，
// 连接由回收逻辑主动关闭（服务端观察到 QUIT），不再泄漏孤儿连接。
func TestDialOrphanRecovery(t *testing.T) {
	srv := newFakeFTPServer(t)
	host, portStr, _ := net.SplitHostPort(srv.addr())
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	d, err := New(map[string]any{"host": host, "port": port, "username": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	f := d.(*FTP)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.dial(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时，实际 %v", err)
	}

	select {
	case line := <-srv.quitSeen:
		if !strings.HasPrefix(line, "QUIT") {
			t.Fatalf("应收到 QUIT，实际 %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("孤儿连接未被回收（未观察到 QUIT）")
	}
}

// 回归测试：握手期静默（可连但不响应）的服务器，dial 应在握手限时内失败，
// 而不是永久阻塞。
func TestDialHandshakeDeadline(t *testing.T) {
	orig := dialTimeout
	dialTimeout = 500 * time.Millisecond
	t.Cleanup(func() { dialTimeout = orig })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept() // 接受连接后保持静默（不发 banner）
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
	d, err := New(map[string]any{"host": host, "port": port})
	if err != nil {
		t.Fatal(err)
	}
	f := d.(*FTP)

	start := time.Now()
	if _, err := f.dial(context.Background()); err == nil {
		t.Fatal("静默服务器应使握手失败")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("握手应在限时内失败，实际耗时 %s", elapsed)
	}
}

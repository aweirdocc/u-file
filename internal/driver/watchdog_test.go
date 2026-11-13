package driver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWatchdogSuccess(t *testing.T) {
	err := RunWatchdog(t.Context(), time.Minute, func() { t.Fatal("不应触发 kill") }, func() error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunWatchdogTimeoutKills(t *testing.T) {
	var killed atomic.Bool
	unblock := make(chan struct{})
	var once atomic.Bool // kill 只应触发一次
	err := RunWatchdog(t.Context(), 30*time.Millisecond, func() {
		if once.Swap(true) {
			t.Error("kill 被调用两次")
		}
		killed.Store(true)
		close(unblock)
	}, func() error {
		<-unblock // 模拟阻塞中的网络读写，被 kill 解除
		return errors.New("use of closed connection")
	})
	var ae *AbortError
	if !errors.As(err, &ae) || ae.Timeout == 0 {
		t.Fatalf("应返回超时 AbortError，实际 %v (%T)", err, err)
	}
	if !killed.Load() {
		t.Fatal("应已触发 kill")
	}
}

func TestRunWatchdogCtxCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	unblock := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := RunWatchdog(ctx, 0, func() { close(unblock) }, func() error {
		<-unblock
		return errors.New("use of closed connection")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 ctx 取消错误，实际 %v", err)
	}
}

// kill 自身挂起时（无法解除 fn 阻塞），看门狗应在宽限期后放弃等待并返回错误，
// 而不是跟着一起挂死。
func TestRunWatchdogStuckKillGrace(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- RunWatchdog(t.Context(), 30*time.Millisecond, func() { /* kill 无效：fn 仍阻塞 */ }, func() error {
			time.Sleep(10 * time.Second) // 模拟无法解除的阻塞
			return nil
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("kill 无效时仍应返回超时错误")
		}
	case <-time.After(WatchdogGrace + 3*time.Second):
		t.Fatal("看门狗不应随 fn 一起挂死")
	}
}

// 无墙上时钟（timeout=0）时仅感知 ctx：正常完成的操作不受影响。
func TestRunWatchdogNoWallClock(t *testing.T) {
	err := RunWatchdog(t.Context(), 0, func() { t.Fatal("不应触发 kill") }, func() error {
		time.Sleep(30 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

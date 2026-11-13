package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "100.64.0.1", "fe80::1", "fc00::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "224.0.0.1",
	}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拒绝", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "203.0.114.1"}
	for _, s := range allowed {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被拒绝", s)
		}
	}
}

// 默认配置下投递到 127.0.0.1 必须在建连阶段被拒绝，且接收端收不到任何请求。
func TestWebhookBlocksPrivateTargetByDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr, err := NewManager(t.TempDir(), Config{HTTPTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	ep, err := mgr.Add(UpsertOptions{URL: srv.URL, Events: []string{"*"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.Test(context.Background(), ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("内网目标应被拒绝，得到 %+v", res)
	}
	if hits.Load() != 0 {
		t.Fatalf("接收端不应收到请求，实际 %d 次", hits.Load())
	}
}

// 公网地址 302 跳转到内网时不得跟随重定向。
func TestWebhookDoesNotFollowRedirect(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
	}))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	// 允许内网仅为让测试能连到 redirector；重定向本身仍不得被跟随
	mgr, err := NewManager(t.TempDir(), Config{HTTPTimeout: time.Second, AllowPrivateTargets: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	ep, _ := mgr.Add(UpsertOptions{URL: redirector.URL, Events: []string{"*"}, Enabled: true})
	res, _ := mgr.Test(context.Background(), ep.ID)
	if res.StatusCode != http.StatusFound || res.OK {
		t.Fatalf("应直接返回 302 且视为失败，得到 %+v", res)
	}
	if internalHits.Load() != 0 {
		t.Fatal("不应跟随重定向访问到内网地址")
	}
}

func TestBlockedTargetError(t *testing.T) {
	c := newHTTPClient(Config{HTTPTimeout: time.Second})
	_, err := c.Get("http://127.0.0.1:1/")
	if err == nil || !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("期望 ErrBlockedTarget，得到 %v", err)
	}
}

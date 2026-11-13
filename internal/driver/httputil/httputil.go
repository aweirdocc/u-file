// Package httputil 提供云盘与 WebDAV 驱动共用的 HTTP 客户端、代理与 Range 头构造工具。
package httputil

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"ufile/internal/driver"
)

// Options HTTP 客户端构造选项。
type Options struct {
	ProxyURL    string // 可选：http://, https://, socks5://
	InsecureTLS bool   // 是否跳过服务端证书校验
}

// NewClient 构造面向流式文件传输的 HTTP 客户端。
// 注意：不设置全局 Client.Timeout，避免大文件流式下载/上传在中途超时中断；
// 连接、TLS 握手与响应头等待通过 Transport 限制。
func NewClient(opt Options) (*http.Client, error) {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if opt.ProxyURL != "" {
		u, err := url.Parse(opt.ProxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("httputil: 无效的 proxy_url %q", opt.ProxyURL)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if opt.InsecureTLS {
		log.Printf("[warn] 已为 HTTP 驱动关闭 TLS 证书校验（insecure_tls=true），连接易遭中间人窃听凭据与数据，仅建议用于受信内网自签证书")
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式开启
	}
	return &http.Client{Transport: tr}, nil
}

// FormatRangeHeader 根据 OpenOptions 生成 HTTP Range 请求头（空串表示从头完整读取）。
func FormatRangeHeader(opt driver.OpenOptions) string {
	if opt.Offset <= 0 && opt.Length <= 0 {
		return ""
	}
	start := max(opt.Offset, int64(0))
	if opt.Length > 0 {
		return fmt.Sprintf("bytes=%d-%d", start, start+opt.Length-1)
	}
	return fmt.Sprintf("bytes=%d-", start)
}

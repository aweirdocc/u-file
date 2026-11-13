package api

// 安全加固相关的装配项与工具函数：受信代理、Cookie Secure、访问日志脱敏。

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// WithTrustedProxies 声明受信反向代理（IP 或 CIDR）。只有来自这些地址的请求，
// 其 X-Forwarded-For / X-Real-IP 才会被采信为客户端 IP；默认（空）不信任任何代理头。
func WithTrustedProxies(proxies []string) Option {
	return func(s *Server) { s.trustedProxies = proxies }
}

// WithSecureCookie 强制所有会话/授权 Cookie 带 Secure 标记。
// 直连 TLS 时会自动加 Secure；TLS 在反向代理终止（后端为明文 HTTP）时需开启本项。
func WithSecureCookie(on bool) Option {
	return func(s *Server) { s.secureCookie = on }
}

// cookieSecure 判断本次响应下发的 Cookie 是否应带 Secure。
func (s *Server) cookieSecure(c *gin.Context) bool {
	return s.secureCookie || c.Request.TLS != nil
}

// sensitiveQueryKeys 访问日志中需要打码的查询参数（?token= 用于 SSE/直链鉴权）。
var sensitiveQueryKeys = []string{"token", "access_token", "password"}

// redactQuery 把 URL 中敏感查询参数的值替换为 REDACTED，避免会话凭据落入访问日志。
func redactQuery(rawPath string) string {
	i := strings.IndexByte(rawPath, '?')
	if i < 0 {
		return rawPath
	}
	q, err := url.ParseQuery(rawPath[i+1:])
	if err != nil {
		return rawPath[:i] + "?<unparsable>"
	}
	changed := false
	for _, k := range sensitiveQueryKeys {
		if _, ok := q[k]; ok {
			q.Set(k, "REDACTED")
			changed = true
		}
	}
	if !changed {
		return rawPath
	}
	return rawPath[:i] + "?" + q.Encode()
}

// accessLogFormatter 与 gin 默认格式一致，仅对查询串中的敏感参数脱敏。
func accessLogFormatter(p gin.LogFormatterParams) string {
	return fmt.Sprintf("[GIN] %v | %3d | %13v | %15s | %-7s %#v\n%s",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode,
		p.Latency,
		p.ClientIP,
		p.Method,
		redactQuery(p.Path),
		p.ErrorMessage,
	)
}

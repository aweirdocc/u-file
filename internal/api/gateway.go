package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/audit"
)

const gatewayTimestampSkew = 5 * time.Minute

// WithGatewaySecret 配置 AgentGuard 网关签名密钥（HMAC-SHA256），
// 启用后默认对网关来源的高风险写/删操作开启 fail-closed 审计。
func WithGatewaySecret(secret string) Option {
	return func(s *Server) {
		s.gatewaySecret = secret
		if secret != "" {
			s.gatewayFailClosedAudit = true
		}
	}
}

// WithRequireGatewayForAPIToken 强制所有 uft_ API Token 请求必须通过已认证的网关进入，
// 防止持有或泄漏 Token 的客户端绕过 a-guard 直连 u-file。
func WithRequireGatewayForAPIToken(required bool) Option {
	return func(s *Server) {
		s.requireGatewayForToken = required
	}
}

// WithGatewayFailClosedAudit 控制网关来源的高风险写/删操作是否在审计写入失败时阻断业务。
func WithGatewayFailClosedAudit(enabled bool) Option {
	return func(s *Server) {
		s.gatewayFailClosedAudit = enabled
	}
}

// GatewaySignature 计算网关请求签名（hex 编码的 HMAC-SHA256）。
// 签名原文格式：<ts>\n<METHOD>\n<PATH>\n<trace_id>\n<agent_actor>
func GatewaySignature(secret, ts, method, path, traceID, actor string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + strings.ToUpper(method) + "\n" + path + "\n" + traceID + "\n" + actor))
	return hex.EncodeToString(mac.Sum(nil))
}

// gatewayAuthMiddleware 校验来自 a-guard 网关的签名，并仅在验签通过后信任 Trace/Actor 头。
func (s *Server) gatewayAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		sig := strings.TrimSpace(c.GetHeader("X-Gateway-Signature"))
		if sig == "" || s.gatewaySecret == "" {
			// 未配置网关密钥或未携带签名：绝不信任请求头中的 Trace/Actor，防止直连伪造。
			c.Next()
			return
		}

		tsStr := strings.TrimSpace(c.GetHeader("X-Gateway-Timestamp"))
		tsSec, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "GATEWAY_AUTH_FAILED", "message": "无效的网关时间戳"})
			return
		}
		reqTime := time.Unix(tsSec, 0)
		diff := time.Since(reqTime)
		if diff < 0 {
			diff = -diff
		}
		if diff > gatewayTimestampSkew {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "GATEWAY_AUTH_FAILED", "message": "网关签名已过期"})
			return
		}

		traceID := strings.TrimSpace(c.GetHeader("X-Trace-ID"))
		actor := strings.TrimSpace(c.GetHeader("X-Agent-Actor"))
		expected := GatewaySignature(s.gatewaySecret, tsStr, c.Request.Method, c.Request.URL.Path, traceID, actor)

		sigBytes, decErr := hex.DecodeString(strings.ToLower(sig))
		expBytes, _ := hex.DecodeString(expected)
		if decErr != nil || !hmac.Equal(sigBytes, expBytes) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "GATEWAY_AUTH_FAILED", "message": "网关签名校验失败"})
			return
		}

		c.Set("gateway_verified", true)
		if traceID != "" {
			c.Set("trace_id", traceID)
		}
		if actor != "" {
			c.Set("agent_actor", actor)
		}
		c.Next()
	}
}

func (s *Server) buildAuditEntry(c *gin.Context, action string, detail any, overrideUser string) audit.Entry {
	user, _ := c.Get("user")
	name := fmt.Sprint(user)
	if user == nil {
		name = ""
	}
	if overrideUser != "" {
		name = overrideUser
	}
	entry := audit.Entry{
		Time:   time.Now(),
		User:   name,
		IP:     c.ClientIP(),
		Action: action,
		Detail: detail,
	}
	if c.GetBool("gateway_verified") {
		entry.TraceID = c.GetString("trace_id")
		entry.AgentActor = c.GetString("agent_actor")
	}
	return entry
}

// auditPrecheck 在网关 fail-closed 模式下执行存储变更前预检，审计不可写时直接阻断。
func (s *Server) auditPrecheck(c *gin.Context) bool {
	if s.audit == nil || !c.GetBool("gateway_verified") || !s.gatewayFailClosedAudit {
		return true
	}
	if err := s.audit.CheckWritable(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "AUDIT_UNAVAILABLE", "message": "审计日志不可写，已拒绝高风险操作"})
		return false
	}
	return true
}

// auditActionStrict 在网关 fail-closed 模式下执行严格审计落盘，失败时返回 500。
func (s *Server) auditActionStrict(c *gin.Context, action string, detail any) bool {
	if s.audit == nil {
		return true
	}
	entry := s.buildAuditEntry(c, action, detail, "")
	if c.GetBool("gateway_verified") && s.gatewayFailClosedAudit {
		if err := s.audit.RecordStrict(entry); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": "AUDIT_FAILED", "message": "审计日志写入失败"})
			return false
		}
		return true
	}
	s.audit.Record(entry)
	return true
}

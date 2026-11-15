package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"ufile/internal/audit"
)

// listAuditLogs 查询最近的操作审计日志（仅管理员可见）。
// 支持 query 参数：
//   - limit: 返回条数（默认 200，最大 1000）
//   - action: 按动作前缀或子串过滤（如 file. / auth. / acl.denied）
//   - user: 按操作用户或 agent_actor 子串过滤
//   - q: 综合关键字过滤（匹配 user/agent_actor/action/ip/trace_id/detail）
func (s *Server) listAuditLogs(c *gin.Context) {
	if s.audit == nil {
		c.JSON(http.StatusOK, gin.H{"items": []audit.Entry{}, "total": 0})
		return
	}

	limit := 200
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	actionFilter := strings.ToLower(strings.TrimSpace(c.Query("action")))
	userFilter := strings.ToLower(strings.TrimSpace(c.Query("user")))
	kw := strings.ToLower(strings.TrimSpace(c.Query("q")))

	// 有过滤条件时扩大扫描窗口，避免仅扫最后 limit 条导致命中过少
	scanN := limit
	if actionFilter != "" || userFilter != "" || kw != "" {
		scanN = 2000
	}

	entries, err := s.audit.Read(scanN)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "AUDIT_READ_ERROR", "message": err.Error()})
		return
	}

	filtered := make([]audit.Entry, 0, len(entries))
	for _, e := range entries {
		if actionFilter != "" && !strings.Contains(strings.ToLower(e.Action), actionFilter) {
			continue
		}
		if userFilter != "" {
			u := strings.ToLower(e.User)
			ag := strings.ToLower(e.AgentActor)
			if !strings.Contains(u, userFilter) && !strings.Contains(ag, userFilter) {
				continue
			}
		}
		if kw != "" && !matchAuditKeyword(e, kw) {
			continue
		}
		filtered = append(filtered, e)
		if len(filtered) >= limit {
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items": filtered,
		"total": len(filtered),
	})
}

func matchAuditKeyword(e audit.Entry, kw string) bool {
	if strings.Contains(strings.ToLower(e.User), kw) ||
		strings.Contains(strings.ToLower(e.AgentActor), kw) ||
		strings.Contains(strings.ToLower(e.Action), kw) ||
		strings.Contains(strings.ToLower(e.IP), kw) ||
		strings.Contains(strings.ToLower(e.TraceID), kw) {
		return true
	}
	if e.Detail != nil {
		if raw, err := json.Marshal(e.Detail); err == nil {
			if strings.Contains(strings.ToLower(string(raw)), kw) {
				return true
			}
		}
	}
	return false
}

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/apitoken"
	"ufile/internal/auth"
)

const (
	agentGuardDefaultReadMaxBytes  int64 = 512 * 1024  // 512 KiB
	agentGuardDefaultWriteMaxBytes int64 = 1024 * 1024 // 1 MiB
)

var agentGuardBlockedWriteExts = []string{".sh", ".bash", ".zsh", ".exe", ".bat", ".ps1"}

// AgentGuardPolicy 严格遵循 a-guard 08-policy-generation-spec.md 第 2 节的顶层契约。
type AgentGuardPolicy struct {
	Version       string           `json:"version"`
	DefaultEffect string           `json:"default_effect"`
	Rules         []AgentGuardRule `json:"rules"`
}

// AgentGuardRule 严格遵循 a-guard 08-policy-generation-spec.md 第 2.2 节的 8 字段白名单。
type AgentGuardRule struct {
	Name               string   `json:"name"`
	Effect             string   `json:"effect"`
	Operations         []string `json:"operations"`
	PathPatterns       []string `json:"path_patterns"`
	MaxBytes           *int64   `json:"max_bytes,omitempty"`
	BlockedExtensions  []string `json:"blocked_extensions,omitempty"`
	AllowedTrustLevels []string `json:"allowed_trust_levels,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

type sourcePolicyGrant struct {
	canRead      bool
	readFull     bool
	readPrefixes []string

	canWrite      bool
	writeFull     bool
	writePrefixes []string

	canDelete      bool
	deleteFull     bool
	deletePrefixes []string

	canListStat bool
}

// WithAgentGuardSyncNotify 配置在文件源或权限变更时主动通知 a-guard HTTP Gateway 回拉同步。
func WithAgentGuardSyncNotify(adminURL, adminSecret, appID string) Option {
	return func(s *Server) {
		s.agAdminURL = strings.TrimRight(strings.TrimSpace(adminURL), "/")
		s.agAdminSecret = strings.TrimSpace(adminSecret)
		appID = strings.TrimSpace(appID)
		if appID == "" {
			appID = "ufile"
		}
		s.agAppID = appID
		if s.agSyncClient == nil {
			s.agSyncClient = &http.Client{Timeout: 5 * time.Second}
		}
	}
}

// invalidateAgentGuardPolicy 在文件源或权限配置变更时清除策略缓存，
// 促使下次拉取生成新 ETag，并在配置了 a-guard 管理地址时异步通知其立即回拉同步。
func (s *Server) invalidateAgentGuardPolicy() {
	s.policyMu.Lock()
	s.policyJSON = nil
	s.policyETag = ""
	s.policyMu.Unlock()

	if s.agAdminURL != "" && s.agAdminSecret != "" {
		go s.notifyAgentGuardPolicySync()
	}
}

func (s *Server) notifyAgentGuardPolicySync() {
	const syncPath = "/_admin/policy/sync"
	appID := s.agAppID
	if appID == "" {
		appID = "ufile"
	}
	payload, _ := json.Marshal(map[string]string{"app_id": appID})
	req, err := http.NewRequest(http.MethodPost, s.agAdminURL+syncPath, bytes.NewReader(payload))
	if err != nil {
		log.Printf("agentguard policy sync notify build error: %v", err)
		return
	}
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	traceID := fmt.Sprintf("ufile-sync-%d", time.Now().UnixNano())
	const actor = "ufile-admin"
	sig := GatewaySignature(s.agAdminSecret, tsStr, http.MethodPost, syncPath, traceID, actor)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Timestamp", tsStr)
	req.Header.Set("X-Gateway-Signature", sig)
	req.Header.Set("X-Trace-ID", traceID)
	req.Header.Set("X-Agent-Actor", actor)

	client := s.agSyncClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("agentguard policy sync notify failed: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("agentguard policy sync notify returned status %d", resp.StatusCode)
	}
}

func (s *Server) requireGatewayOrAdmin() gin.HandlerFunc {
	adminMW := s.requireRole(auth.RoleAdmin)
	return func(c *gin.Context) {
		if c.GetBool("gateway_verified") {
			c.Next()
			return
		}
		if s.auth == nil && s.gatewaySecret != "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "UNAUTHORIZED",
				"message": "需要 AgentGuard 网关签名或管理员鉴权",
			})
			return
		}
		adminMW(c)
	}
}

// getAgentGuardPolicy 导出符合 a-guard 严格 Schema 的策略 JSON（支持 ETag / If-None-Match 304 协商）。
func (s *Server) getAgentGuardPolicy(c *gin.Context) {
	data, etag, err := s.getAgentGuardPolicyBytes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "POLICY_BUILD_FAILED", "message": err.Error()})
		return
	}

	c.Header("ETag", etag)
	c.Header("Cache-Control", "no-cache")
	if etagMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

func (s *Server) getAgentGuardPolicyBytes() ([]byte, string, error) {
	s.policyMu.RLock()
	if len(s.policyJSON) > 0 && s.policyETag != "" {
		data := append([]byte(nil), s.policyJSON...)
		etag := s.policyETag
		s.policyMu.RUnlock()
		return data, etag, nil
	}
	s.policyMu.RUnlock()

	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if len(s.policyJSON) > 0 && s.policyETag != "" {
		return append([]byte(nil), s.policyJSON...), s.policyETag, nil
	}

	pol := s.buildAgentGuardPolicy()
	data, err := json.MarshalIndent(pol, "", "  ")
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	etag := fmt.Sprintf(`"%x"`, sum)

	s.policyJSON = data
	s.policyETag = etag
	return append([]byte(nil), data...), etag, nil
}

func (s *Server) buildAgentGuardPolicy() AgentGuardPolicy {
	pol := AgentGuardPolicy{
		Version:       "1",
		DefaultEffect: "deny",
		Rules:         make([]AgentGuardRule, 0),
	}

	if s.sources == nil {
		return pol
	}

	sources := s.sources.List()
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].ID < sources[j].ID
	})

	activeTokens := s.activeAPITokens(time.Now())
	hasTokens := len(activeTokens) > 0
	anyListStat := false

	for _, src := range sources {
		grant := s.evaluateSourceGrant(src.ID, activeTokens, hasTokens)
		if grant.canListStat {
			anyListStat = true
		}
		slug := sanitizeRuleSlug(src.ID)

		if grant.canWrite {
			patterns := buildWritePatterns(src.ID, grant.writeFull, grant.writePrefixes)
			if len(patterns) > 0 {
				maxB := agentGuardDefaultWriteMaxBytes
				pol.Rules = append(pol.Rules, AgentGuardRule{
					Name:              fmt.Sprintf("allow-%s-workspace-writes", slug),
					Effect:            "allow",
					Operations:        []string{"write"},
					PathPatterns:      patterns,
					MaxBytes:          &maxB,
					BlockedExtensions: append([]string(nil), agentGuardBlockedWriteExts...),
					Reason:            fmt.Sprintf("writes allowed in %s/workspace/ and %s/tmp/ up to 1 MiB (non-executable)", src.ID, src.ID),
				})
			}
		}

		if grant.canDelete {
			patterns := buildDeletePatterns(src.ID, grant.deleteFull, grant.deletePrefixes)
			if len(patterns) > 0 {
				pol.Rules = append(pol.Rules, AgentGuardRule{
					Name:         fmt.Sprintf("allow-%s-tmp-delete", slug),
					Effect:       "allow",
					Operations:   []string{"delete"},
					PathPatterns: patterns,
					Reason:       fmt.Sprintf("deletions allowed only in %s/tmp/ or *.tmp within %s", src.ID, src.ID),
				})
			}
		}

		if grant.canRead {
			patterns := buildReadPatterns(src.ID, grant.readFull, grant.readPrefixes)
			if len(patterns) > 0 {
				maxB := agentGuardDefaultReadMaxBytes
				pol.Rules = append(pol.Rules, AgentGuardRule{
					Name:         fmt.Sprintf("redact-%s-reads", slug),
					Effect:       "redact",
					Operations:   []string{"read"},
					PathPatterns: patterns,
					MaxBytes:     &maxB,
					Reason:       fmt.Sprintf("reads in %s are scanned and redacted up to 512 KiB", src.ID),
				})
			}
		}
	}

	if !hasTokens && len(sources) > 0 {
		anyListStat = true
	} else if hasTokens && !anyListStat {
		for _, tok := range activeTokens {
			if tok.HasScope(apitoken.ScopeSourcesRead) || tok.HasScope(apitoken.ScopeFilesRead) {
				anyListStat = true
				break
			}
		}
	}

	if anyListStat {
		pol.Rules = append(pol.Rules, AgentGuardRule{
			Name:         "allow-list-and-stat",
			Effect:       "allow",
			Operations:   []string{"list", "stat"},
			PathPatterns: []string{"*"},
			Reason:       "listing and stat metadata allowed",
		})
	}

	return pol
}

func (s *Server) activeAPITokens(now time.Time) []*apitoken.Token {
	if s.tokens == nil {
		return nil
	}
	all := s.tokens.List()
	out := make([]*apitoken.Token, 0, len(all))
	for _, tok := range all {
		if tok == nil || tok.Revoked {
			continue
		}
		if tok.ExpiresAt != nil && now.After(*tok.ExpiresAt) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

func (s *Server) evaluateSourceGrant(sourceID string, activeTokens []*apitoken.Token, hasTokens bool) sourcePolicyGrant {
	if !hasTokens {
		return sourcePolicyGrant{
			canRead:     true,
			readFull:    true,
			canWrite:    true,
			writeFull:   true,
			canDelete:   true,
			deleteFull:  true,
			canListStat: true,
		}
	}

	var g sourcePolicyGrant
	for _, tok := range activeTokens {
		if !tok.AllowsSource(sourceID) {
			continue
		}

		// 若启用用户体系，校验 Token 创建者的账号状态与源级/路径级 ACL。
		isAdminCreator := true
		var userPolicies []auth.Policy
		if s.auth != nil && tok.CreatedBy != "" {
			u, ok := s.auth.Get(tok.CreatedBy)
			if !ok || u.Disabled {
				continue
			}
			isAdminCreator = u.Role == auth.RoleAdmin
			userPolicies = u.Policies
		}

		if tok.HasScope(apitoken.ScopeSourcesRead) || tok.HasScope(apitoken.ScopeFilesRead) {
			if isAdminCreator || hasActionOnSource(userPolicies, sourceID, auth.ActionRead) {
				g.canListStat = true
			}
		}

		if tok.HasScope(apitoken.ScopeFilesRead) {
			if full, prefixes, ok := actionGrantForSource(isAdminCreator, userPolicies, sourceID, auth.ActionRead); ok {
				g.canRead = true
				if full {
					g.readFull = true
				} else {
					g.readPrefixes = appendUniqueStrings(g.readPrefixes, prefixes...)
				}
			}
		}

		if tok.HasScope(apitoken.ScopeFilesWrite) {
			if full, prefixes, ok := actionGrantForSource(isAdminCreator, userPolicies, sourceID, auth.ActionWrite); ok {
				g.canWrite = true
				if full {
					g.writeFull = true
				} else {
					g.writePrefixes = appendUniqueStrings(g.writePrefixes, prefixes...)
				}
			}
		}

		if tok.HasScope(apitoken.ScopeFilesDelete) {
			if full, prefixes, ok := actionGrantForSource(isAdminCreator, userPolicies, sourceID, auth.ActionDelete); ok {
				g.canDelete = true
				if full {
					g.deleteFull = true
				} else {
					g.deletePrefixes = appendUniqueStrings(g.deletePrefixes, prefixes...)
				}
			}
		}
	}
	return g
}

func hasActionOnSource(policies []auth.Policy, sourceID string, act auth.Action) bool {
	for _, p := range policies {
		if (p.SourceID == "*" || p.SourceID == sourceID) && p.HasAction(act) {
			return true
		}
	}
	return false
}

func actionGrantForSource(isAdmin bool, policies []auth.Policy, sourceID string, act auth.Action) (full bool, prefixes []string, ok bool) {
	if isAdmin {
		return true, nil, true
	}
	for _, p := range policies {
		if (p.SourceID != "*" && p.SourceID != sourceID) || !p.HasAction(act) {
			continue
		}
		ok = true
		eff := p.EffectivePrefixes()
		if len(eff) == 0 {
			return true, nil, true
		}
		prefixes = appendUniqueStrings(prefixes, eff...)
	}
	return false, prefixes, ok
}

func buildReadPatterns(sourceID string, full bool, prefixes []string) []string {
	if full || len(prefixes) == 0 {
		return []string{sourceID + "/*", sourceID + "/**"}
	}
	out := make([]string, 0, len(prefixes)*2)
	for _, p := range prefixes {
		base := sourceID + "/" + strings.Trim(p, "/")
		out = append(out, base+"/*", base+"/**")
	}
	return out
}

func buildWritePatterns(sourceID string, full bool, prefixes []string) []string {
	if full || len(prefixes) == 0 {
		return []string{
			sourceID + "/workspace/*",
			sourceID + "/workspace/**",
			sourceID + "/tmp/*",
			sourceID + "/tmp/**",
		}
	}
	out := make([]string, 0, len(prefixes)*2)
	for _, p := range prefixes {
		base := sourceID + "/" + strings.Trim(p, "/")
		out = append(out, base+"/*", base+"/**")
	}
	return out
}

func buildDeletePatterns(sourceID string, full bool, prefixes []string) []string {
	if full || len(prefixes) == 0 {
		return []string{
			sourceID + "/tmp/*",
			sourceID + "/tmp/**",
			sourceID + "/*.tmp",
			sourceID + "/**/*.tmp",
		}
	}
	out := make([]string, 0, len(prefixes)*2)
	for _, p := range prefixes {
		base := sourceID + "/" + strings.Trim(p, "/")
		out = append(out, base+"/*", base+"/**")
	}
	return out
}

func sanitizeRuleSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "source"
	}
	return out
}

func appendUniqueStrings(dst []string, items ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(items))
	for _, v := range dst {
		seen[v] = struct{}{}
	}
	for _, v := range items {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			dst = append(dst, v)
		}
	}
	sort.Strings(dst)
	return dst
}

func etagMatches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	norm := func(v string) string {
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, "W/")
		return strings.Trim(v, `"`)
	}
	target := norm(etag)
	for _, part := range strings.Split(ifNoneMatch, ",") {
		if norm(part) == target {
			return true
		}
	}
	return false
}

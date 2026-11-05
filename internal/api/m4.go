package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/apitoken"
	"ufile/internal/auth"
	"ufile/internal/fulltext"
	"ufile/internal/mount"
	"ufile/internal/source"
	"ufile/internal/task"
	"ufile/internal/webhook"
)

// Option Server 可选功能组件装配函数（M4 扩展）。
type Option func(*Server)

func WithTasks(tm *task.Manager) Option {
	return func(s *Server) { s.tasks = tm }
}

func WithAPITokens(tm *apitoken.Manager) Option {
	return func(s *Server) { s.tokens = tm }
}

func WithWebhooks(wm *webhook.Manager) Option {
	return func(s *Server) { s.webhooks = wm }
}

func WithMounts(mm *mount.Manager) Option {
	return func(s *Server) { s.mounts = mm }
}

func WithFullText(ft *fulltext.Engine) Option {
	return func(s *Server) { s.fulltext = ft }
}

func (s *Server) registerM4Routes(grp *gin.RouterGroup, authAdmin, authAny gin.HandlerFunc) {
	// —— M4.1 传输任务中心与跨源流转 ——
	if s.tasks != nil {
		grp.GET("/tasks", authAny, s.listTasks)
		grp.POST("/tasks/transfer", authAny, s.submitTransferTask)
		grp.POST("/tasks/:id/pause", authAny, s.pauseTask)
		grp.POST("/tasks/:id/resume", authAny, s.resumeTask)
		grp.POST("/tasks/:id/cancel", authAny, s.cancelTask)
		grp.POST("/tasks/:id/retry", authAny, s.retryTask)
		grp.DELETE("/tasks/:id", authAny, s.deleteTask)
		grp.POST("/tasks/clear", authAny, s.clearTasks)
		grp.GET("/tasks/events", authAny, s.taskEventsSSE)
	}

	// —— M4.2 开放 API Token 管理（admin 专用）——
	if s.tokens != nil {
		grp.GET("/tokens", authAdmin, s.listAPITokens)
		grp.POST("/tokens", authAdmin, s.createAPIToken)
		grp.POST("/tokens/:id/revoke", authAdmin, s.revokeAPIToken)
		grp.DELETE("/tokens/:id", authAdmin, s.deleteAPIToken)
	}

	// —— M4.2 事件 Webhook 管理（admin 专用）——
	if s.webhooks != nil {
		grp.GET("/webhooks", authAdmin, s.listWebhooks)
		grp.POST("/webhooks", authAdmin, s.createWebhook)
		grp.PUT("/webhooks/:id", authAdmin, s.updateWebhook)
		grp.DELETE("/webhooks/:id", authAdmin, s.deleteWebhook)
		grp.POST("/webhooks/:id/test", authAdmin, s.testWebhook)
	}

	// —— M4.3 虚拟多挂载点树 VFS ——
	if s.mounts != nil {
		grp.GET("/mounts", authAny, s.listMounts)
		grp.POST("/mounts", authAdmin, s.createMount)
		grp.PUT("/mounts/:id", authAdmin, s.updateMount)
		grp.DELETE("/mounts/:id", authAdmin, s.deleteMount)
	}

	// —— M4.3 全文检索引擎状态与重建 ——
	if s.fulltext != nil {
		grp.GET("/fulltext/status", authAny, s.fulltextStatus)
		grp.POST("/fulltext/rebuild", authAdmin, s.fulltextRebuild)
	}
}

// emitWebhook 安全触发 Webhook 事件（未启用时为空操作）。
func (s *Server) emitWebhook(eventType, sourceID string, data any) {
	if s.webhooks == nil {
		return
	}
	s.webhooks.Emit(eventType, sourceID, data)
}

// virtualSource 构造 "@vfs" 虚拟挂载视图源实例。
func (s *Server) virtualSource() *source.Source {
	if s.mounts == nil {
		return nil
	}
	return source.NewVirtual(mount.VirtualSourceID, "虚拟挂载视图 (VFS)", "vfs", s.mounts)
}

// —— M4.1 任务中心 API ——

func (s *Server) listTasks(c *gin.Context) {
	c.JSON(http.StatusOK, s.tasks.List(s.currentUsername(c), s.isAdmin(c)))
}

func (s *Server) submitTransferTask(c *gin.Context) {
	var req struct {
		Type           string   `json:"type"` // "copy" | "move"
		SourceID       string   `json:"source_id"`
		TargetSourceID string   `json:"target_source_id"`
		Paths          []string `json:"paths"`
		DstDir         string   `json:"dst_dir"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if len(req.Paths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 paths"})
		return
	}
	tType := task.TaskType(req.Type)
	if tType != task.TypeCopy && tType != task.TypeMove {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "type 必须为 copy 或 move"})
		return
	}

	// 权限校验：源路径需 read（若 move 还需 delete），目标路径需 write
	for _, p := range req.Paths {
		if !s.checkPathAccess(c, req.SourceID, p, auth.ActionRead) {
			return
		}
		if tType == task.TypeMove && !s.checkPathAccess(c, req.SourceID, p, auth.ActionDelete) {
			return
		}
	}
	if !s.checkPathAccess(c, req.TargetSourceID, req.DstDir, auth.ActionWrite) {
		return
	}

	tk, err := s.tasks.Submit(task.TransferRequest{
		Type:           tType,
		UserID:         s.currentUsername(c),
		SourceID:       req.SourceID,
		TargetSourceID: req.TargetSourceID,
		Paths:          req.Paths,
		DstDir:         req.DstDir,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "TASK_REJECTED", "message": err.Error()})
		return
	}
	s.auditAction(c, "task.transfer.submit", gin.H{
		"task_id":       tk.ID,
		"type":          tk.Type,
		"source":        tk.SourceID,
		"target_source": tk.TargetSourceID,
		"paths":         tk.Paths,
		"dst_dir":       tk.DstDir,
	})
	c.JSON(http.StatusCreated, tk)
}

func (s *Server) checkTaskOwner(c *gin.Context, id string) (*task.Task, bool) {
	tk, err := s.tasks.Get(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "TASK_NOT_FOUND", "message": "任务不存在"})
		return nil, false
	}
	if !s.isAdmin(c) && tk.UserID != s.currentUsername(c) {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "无权操作他人的传输任务"})
		return nil, false
	}
	return tk, true
}

func (s *Server) pauseTask(c *gin.Context) {
	if _, ok := s.checkTaskOwner(c, c.Param("id")); !ok {
		return
	}
	tk, err := s.tasks.Pause(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_STATE", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tk)
}

func (s *Server) resumeTask(c *gin.Context) {
	if _, ok := s.checkTaskOwner(c, c.Param("id")); !ok {
		return
	}
	tk, err := s.tasks.Resume(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_STATE", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tk)
}

func (s *Server) cancelTask(c *gin.Context) {
	if _, ok := s.checkTaskOwner(c, c.Param("id")); !ok {
		return
	}
	tk, err := s.tasks.Cancel(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_STATE", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tk)
}

func (s *Server) retryTask(c *gin.Context) {
	if _, ok := s.checkTaskOwner(c, c.Param("id")); !ok {
		return
	}
	tk, err := s.tasks.Retry(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_STATE", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tk)
}

func (s *Server) deleteTask(c *gin.Context) {
	if _, ok := s.checkTaskOwner(c, c.Param("id")); !ok {
		return
	}
	if err := s.tasks.Remove(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "TASK_NOT_FOUND", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) clearTasks(c *gin.Context) {
	n := s.tasks.ClearFinished(s.currentUsername(c), s.isAdmin(c))
	c.JSON(http.StatusOK, gin.H{"ok": true, "cleared": n})
}

// taskEventsSSE Server-Sent Events 实时推送任务状态流。
func (s *Server) taskEventsSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ch, unsub := s.tasks.Subscribe(s.currentUsername(c), s.isAdmin(c))
	defer unsub()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case list, ok := <-ch:
			if !ok {
				return
			}
			raw, err := json.Marshal(list)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
			c.Writer.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprintf(c.Writer, ": ping\n\n")
			c.Writer.Flush()
		}
	}
}

// —— M4.2 开放 API Token 接口 ——

func (s *Server) listAPITokens(c *gin.Context) {
	c.JSON(http.StatusOK, s.tokens.List())
}

func (s *Server) createAPIToken(c *gin.Context) {
	var req struct {
		Name           string           `json:"name"`
		Scopes         []apitoken.Scope `json:"scopes"`
		AllowedSources []string         `json:"allowed_sources"`
		IPWhitelist    []string         `json:"ip_whitelist"`
		TTLHours       int              `json:"ttl_hours"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	tok, plain, err := s.tokens.Create(apitoken.CreateOptions{
		Name:           req.Name,
		CreatedBy:      s.currentUsername(c),
		Scopes:         req.Scopes,
		AllowedSources: req.AllowedSources,
		IPWhitelist:    req.IPWhitelist,
		TTLHours:       req.TTLHours,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "apitoken.create", gin.H{"id": tok.ID, "name": tok.Name, "scopes": tok.Scopes})
	c.JSON(http.StatusCreated, gin.H{"token": tok, "plain_token": plain})
}

func (s *Server) revokeAPIToken(c *gin.Context) {
	id := c.Param("id")
	if err := s.tokens.Revoke(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": err.Error()})
		return
	}
	s.auditAction(c, "apitoken.revoke", gin.H{"id": id})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) deleteAPIToken(c *gin.Context) {
	id := c.Param("id")
	if err := s.tokens.Delete(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": err.Error()})
		return
	}
	s.auditAction(c, "apitoken.delete", gin.H{"id": id})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// verifyAPITokenAuth 在鉴权中间件中处理以 "uft_" 开头的开放 API Token。
func (s *Server) verifyAPITokenAuth(c *gin.Context, rawToken string, roles []auth.Role) {
	if s.tokens == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": "未启用 API Token 功能"})
		return
	}
	apiTok, err := s.tokens.Verify(rawToken, c.ClientIP())
	if err != nil {
		code := "UNAUTHORIZED"
		status := http.StatusUnauthorized
		if errors.Is(err, apitoken.ErrIPForbidden) {
			code = "IP_FORBIDDEN"
			status = http.StatusForbidden
		}
		c.AbortWithStatusJSON(status, gin.H{"code": code, "message": err.Error()})
		return
	}

	// API Token 不允许访问用户管理、Token 管理、Webhook 管理、源增删改等管理后台端点
	reqScope, allowedEndpoint := requiredScopeForEndpoint(c.Request.Method, c.FullPath())
	if !allowedEndpoint || (reqScope != "" && !apiTok.HasScope(reqScope)) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code":    "SCOPE_FORBIDDEN",
			"message": fmt.Sprintf("API Token 缺少访问该接口所需的权限作用域 (%s)", reqScope),
		})
		return
	}

	role := auth.RoleAdmin
	if s.auth != nil {
		if u, ok := s.auth.Get(apiTok.CreatedBy); ok {
			if u.Disabled {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": "Token 所属账号已禁用"})
				return
			}
			role = u.Role
		}
	}
	roleOK := false
	for _, r := range roles {
		if role == r {
			roleOK = true
			break
		}
	}
	if !roleOK {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足"})
		return
	}

	c.Set("user", apiTok.CreatedBy)
	c.Set("role", role)
	c.Set("api_token", apiTok)
	c.Next()
}

// requiredScopeForEndpoint 根据请求方法与路由模板判定 API Token 所需的 Scope。
func requiredScopeForEndpoint(method, fullPath string) (apitoken.Scope, bool) {
	switch {
	case strings.HasPrefix(fullPath, "/api/v1/sources/:id/files"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/download"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/raw"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/thumb"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/office"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/presign"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/archive"),
		fullPath == "/api/v1/search",
		fullPath == "/api/v1/fulltext/status",
		fullPath == "/api/v1/mounts" && method == http.MethodGet,
		strings.HasPrefix(fullPath, "/api/v1/tasks") && method == http.MethodGet:
		return apitoken.ScopeFilesRead, true

	case strings.HasPrefix(fullPath, "/api/v1/sources/:id/upload"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/mkdir"),
		strings.HasPrefix(fullPath, "/api/v1/sources/:id/rename"),
		strings.HasPrefix(fullPath, "/api/v1/tasks"):
		return apitoken.ScopeFilesWrite, true

	case strings.HasPrefix(fullPath, "/api/v1/sources/:id/delete"):
		return apitoken.ScopeFilesDelete, true

	case (fullPath == "/api/v1/sources" || fullPath == "/api/v1/sources/:id" || fullPath == "/api/v1/sources/:id/sync" || fullPath == "/api/v1/index/status") && method == http.MethodGet:
		return apitoken.ScopeSourcesRead, true

	case strings.HasPrefix(fullPath, "/api/v1/shares"):
		return apitoken.ScopeSharesManage, true

	default:
		return "", false
	}
}

func actionToTokenScope(act auth.Action) apitoken.Scope {
	switch act {
	case auth.ActionRead:
		return apitoken.ScopeFilesRead
	case auth.ActionWrite:
		return apitoken.ScopeFilesWrite
	case auth.ActionDelete:
		return apitoken.ScopeFilesDelete
	case auth.ActionShare:
		return apitoken.ScopeSharesManage
	default:
		return apitoken.ScopeFilesRead
	}
}

// —— M4.2 事件 Webhook 接口 ——

func (s *Server) listWebhooks(c *gin.Context) {
	c.JSON(http.StatusOK, s.webhooks.List())
}

func (s *Server) createWebhook(c *gin.Context) {
	var opt webhook.UpsertOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	ep, err := s.webhooks.Add(opt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "webhook.create", gin.H{"id": ep.ID, "url": ep.URL, "events": ep.Events})
	c.JSON(http.StatusCreated, ep)
}

func (s *Server) updateWebhook(c *gin.Context) {
	var opt webhook.UpsertOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	ep, err := s.webhooks.Update(c.Param("id"), opt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "webhook.update", gin.H{"id": ep.ID, "url": ep.URL, "enabled": ep.Enabled})
	c.JSON(http.StatusOK, ep)
}

func (s *Server) deleteWebhook(c *gin.Context) {
	id := c.Param("id")
	if err := s.webhooks.Delete(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": err.Error()})
		return
	}
	s.auditAction(c, "webhook.delete", gin.H{"id": id})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) testWebhook(c *gin.Context) {
	res, err := s.webhooks.Test(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// —— M4.3 虚拟多挂载点树 VFS 接口 ——

func (s *Server) listMounts(c *gin.Context) {
	c.JSON(http.StatusOK, s.mounts.ListMounts())
}

func (s *Server) createMount(c *gin.Context) {
	var opt mount.UpsertOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if _, err := s.sources.Get(opt.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "SOURCE_NOT_FOUND", "message": "所选物理文件源不存在"})
		return
	}
	mnt, err := s.mounts.Add(opt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if s.index != nil {
		s.index.Invalidate(mount.VirtualSourceID)
	}
	s.auditAction(c, "mount.create", gin.H{"id": mnt.ID, "mount_path": mnt.MountPath, "source_id": mnt.SourceID})
	c.JSON(http.StatusCreated, mnt)
}

func (s *Server) updateMount(c *gin.Context) {
	var opt mount.UpsertOptions
	if err := c.ShouldBindJSON(&opt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if _, err := s.sources.Get(opt.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "SOURCE_NOT_FOUND", "message": "所选物理文件源不存在"})
		return
	}
	mnt, err := s.mounts.Update(c.Param("id"), opt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if s.index != nil {
		s.index.Invalidate(mount.VirtualSourceID)
	}
	s.auditAction(c, "mount.update", gin.H{"id": mnt.ID, "mount_path": mnt.MountPath, "source_id": mnt.SourceID})
	c.JSON(http.StatusOK, mnt)
}

func (s *Server) deleteMount(c *gin.Context) {
	id := c.Param("id")
	if err := s.mounts.Remove(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": err.Error()})
		return
	}
	if s.index != nil {
		s.index.Invalidate(mount.VirtualSourceID)
	}
	s.auditAction(c, "mount.delete", gin.H{"id": id})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// —— M4.3 全文检索引擎接口 ——

func (s *Server) fulltextStatus(c *gin.Context) {
	c.JSON(http.StatusOK, s.fulltext.Status())
}

func (s *Server) fulltextRebuild(c *gin.Context) {
	var req struct {
		SourceID string `json:"source_id"`
	}
	_ = c.ShouldBindJSON(&req)

	go func(targetSource string) {
		ctx := context.Background()
		if targetSource != "" {
			if src, err := s.sources.Get(targetSource); err == nil {
				s.fulltext.IndexSource(ctx, src.ID, src.Driver())
			}
			return
		}
		for _, id := range s.sources.IDs() {
			if src, err := s.sources.Get(id); err == nil {
				s.fulltext.IndexSource(ctx, src.ID, src.Driver())
			}
		}
	}(strings.TrimSpace(req.SourceID))

	s.auditAction(c, "fulltext.rebuild", gin.H{"source_id": req.SourceID})
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "全文索引后台构建已启动"})
}

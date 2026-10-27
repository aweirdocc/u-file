package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/driver"
	"ufile/internal/index"
	"ufile/internal/source"
)

//go:embed static/index.html
var staticFS embed.FS

// textLikeExtensions 视为文本预览的扩展名（前端按此判断，后端兜底嗅探）。
var textLikeExtensions = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".log": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".xml": true, ".html": true, ".htm": true, ".css": true,
	".go": true, ".js": true, ".ts": true, ".py": true, ".java": true,
	".c": true, ".h": true, ".cpp": true, ".rs": true, ".sh": true,
	".sql": true, ".bat": true, ".ps1": true, ".diff": true, ".env": true,
}

// previewKind 返回文件可预览的类型；"" 表示不支持预览。
func previewKind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif":
		return "image"
	case ".mp4", ".webm", ".mov", ".mkv", ".m4v":
		return "video"
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac":
		return "audio"
	case ".pdf":
		return "pdf"
	case ".md", ".markdown":
		return "markdown"
	}
	if textLikeExtensions[strings.ToLower(path.Ext(name))] {
		return "text"
	}
	return ""
}

// mediaTypeByExt 由扩展名推断 MIME；未知返回 ""（交给内容嗅探）。
// 不直接信任客户端/存储上报的 Content-Type（x-amz-content-type 可被污染）。
func mediaTypeByExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		// 预览场景强制按纯文本直出：脚本失效（配合 CSP 双保险），内容仍可读
		return "text/plain; charset=utf-8"
	}
	if textLikeExtensions[ext] {
		return "text/plain; charset=utf-8"
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return ""
}

type Server struct {
	engine  *gin.Engine
	sources *source.Manager
	auth    *auth.Store
	audit   *audit.Log
	index   *index.Cache // 可为 nil（未启用索引缓存）
}

// New 构造 API 服务。authStore/auditLog 可为 nil（无鉴权模式）；
// indexCache 可为 nil（未启用索引缓存）。
func New(cfgSources *source.Manager, authStore *auth.Store, auditLog *audit.Log, indexCache *index.Cache) (*Server, error) {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{engine: gin.New(), sources: cfgSources, auth: authStore, audit: auditLog, index: indexCache}
	s.engine.Use(gin.Logger(), gin.Recovery())

	s.engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	grp := s.engine.Group("/api/v1")

	// —— 鉴权（无鉴权模式下不注册）——
	if s.auth != nil {
		grp.POST("/auth/bootstrap", s.bootstrap) // 仅无用户时可创建首个管理员
		grp.POST("/auth/login", s.login)
		grp.GET("/auth/status", s.authStatus) // 前端判断是否需要引导/登录
		grp.GET("/auth/me", s.requireRole(auth.RoleAdmin, auth.RoleGuest), s.me)
		grp.POST("/auth/users", s.requireRole(auth.RoleAdmin), s.createUser)
		grp.GET("/auth/users", s.requireRole(auth.RoleAdmin), s.listUsers)
		grp.DELETE("/auth/users/:name", s.requireRole(auth.RoleAdmin), s.deleteUser)
		grp.POST("/auth/users/:name/password", s.requireRole(auth.RoleAdmin), s.resetPassword)
		grp.POST("/auth/logout", s.requireRole(auth.RoleAdmin, auth.RoleGuest), s.logout)
	}

	// —— 源管理（admin 专用；guest 只读浏览）——
	authAdmin := s.requireRole(auth.RoleAdmin)
	authAny := s.requireRole(auth.RoleAdmin, auth.RoleGuest)
	grp.GET("/sources", authAny, s.listSources)
	grp.GET("/sources/:id", authAny, s.getSource)
	grp.GET("/sources/export", authAdmin, s.exportSources) // 全量导出含明文凭据，admin only
	grp.POST("/sources/import", authAdmin, s.importSources)
	grp.POST("/sources/test", authAdmin, s.testSource)
	grp.POST("/sources", authAdmin, s.addSource)
	grp.PUT("/sources/:id", authAdmin, s.updateSource)
	grp.DELETE("/sources/:id", authAdmin, s.removeSource)

	// —— 浏览/下载/预览（guest 可用）——
	grp.GET("/sources/:id/files", authAny, s.listFiles)
	grp.GET("/sources/:id/download", authAny, s.download)
	grp.GET("/sources/:id/raw", authAny, s.raw)
	grp.GET("/sources/:id/presign", authAny, s.presign)
	grp.GET("/sources/:id/archive", authAny, s.archive) // 多选/整目录 zip 流式打包

	// —— 写操作（admin；驱动需实现 Writer 能力）——
	grp.POST("/sources/:id/upload", authAdmin, s.upload)
	grp.POST("/sources/:id/mkdir", authAdmin, s.mkdir)
	grp.POST("/sources/:id/delete", authAdmin, s.deleteEntries)
	grp.POST("/sources/:id/rename", authAdmin, s.renameEntry)

	// —— 搜索（基于索引快照；index 未启用时不注册）——
	if s.index != nil {
		grp.GET("/search", authAny, s.search)
	}

	// —— 索引/同步状态（M1 索引缓存，index 未启用时不注册）——
	if s.index != nil {
		grp.GET("/sources/:id/sync", authAny, s.syncStatus)
		grp.POST("/sources/:id/sync", authAdmin, s.syncTrigger)
		grp.GET("/index/status", authAny, s.indexStatus)
	}

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	s.engine.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	s.engine.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "接口不存在"})
	})
	return s, nil
}

// Handler 返回根 HTTP Handler。
func (s *Server) Handler() http.Handler { return s.engine }

// requireRole 鉴权中间件。auth 未启用时直接放行（本地联调模式）。
func (s *Server) requireRole(roles ...auth.Role) gin.HandlerFunc {
	if s.auth == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		tok := bearerToken(c.Request)
		if tok == "" {
			c.Header("WWW-Authenticate", `Bearer realm="u-file"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": "未登录"})
			return
		}
		cl, err := s.auth.Verify(tok)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": "会话已过期，请重新登录"})
			return
		}
		allowed := false
		for _, r := range roles {
			if cl.Role == r {
				allowed = true
				break
			}
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足（该操作需管理员）"})
			return
		}
		c.Set("user", cl.Sub)
		c.Set("role", cl.Role)
		c.Next()
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// 浏览器原生下载/预览标签无法带 header，接受 cookie 中的 token
	if ck, err := r.Cookie("ufile_token"); err == nil {
		return ck.Value
	}
	// 兜底：?token=（下载/预览直链场景；不推荐，日志里注意不打 URL query）
	return r.URL.Query().Get("token")
}

// audit 记录一条审计日志（auth 未启用时为空操作）。
// overrideUser 供登录/引导等“尚无会话”的动作显式指定操作者。
func (s *Server) auditAction(c *gin.Context, action string, detail any) {
	s.auditActionAs(c, action, detail, "")
}

func (s *Server) auditActionAs(c *gin.Context, action string, detail any, overrideUser string) {
	if s.audit == nil {
		return
	}
	user, _ := c.Get("user")
	name := fmt.Sprint(user)
	if overrideUser != "" {
		name = overrideUser
	}
	s.audit.Record(audit.Entry{
		Time:   time.Now(),
		User:   name,
		IP:     c.ClientIP(),
		Action: action,
		Detail: detail,
	})
}

// —— 鉴权接口 ——

type bootstrapReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// bootstrap 创建首个管理员：仅当系统中还没有任何用户时可用。
// 创建成功后此入口即永久关闭（返回 403），避免裸奔窗口被抢注。
func (s *Server) bootstrap(c *gin.Context) {
	if s.auth.HasUsers() {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "管理员已存在，引导入口已关闭"})
		return
	}
	var req bootstrapReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if err := s.auth.Create(req.Username, req.Password, auth.RoleAdmin); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	tok, role, err := s.auth.Login(req.Username, req.Password, c.ClientIP())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL", "message": err.Error()})
		return
	}
	s.auditActionAs(c, "auth.bootstrap", gin.H{"user": req.Username}, req.Username)
	s.issueSession(c, tok, role)
	c.JSON(http.StatusCreated, gin.H{"user": req.Username, "role": role})
}

func (s *Server) login(c *gin.Context) {
	var req bootstrapReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	key := req.Username + "|" + c.ClientIP()
	tok, role, err := s.auth.Login(req.Username, req.Password, key)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "BAD_CREDENTIALS", "message": err.Error()})
		return
	}
	s.auditActionAs(c, "auth.login", gin.H{"user": req.Username}, req.Username)
	s.issueSession(c, tok, role)
	c.JSON(http.StatusOK, gin.H{"user": req.Username, "role": role})
}

// issueSession 设置 cookie（预览/下载标签用）并回显 token（XHR 用 Authorization 头）。
func (s *Server) issueSession(c *gin.Context, tok string, role auth.Role) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     "ufile_token",
		Value:    tok,
		Path:     "/",
		MaxAge:   int((24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	c.Header("X-Auth-Token", tok) // 前端读取后走 Authorization 头
	_ = role
}

func (s *Server) authStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"auth_enabled": s.auth != nil, "needs_bootstrap": s.auth != nil && !s.auth.HasUsers()})
}

func (s *Server) me(c *gin.Context) {
	user, _ := c.Get("user")
	role, _ := c.Get("role")
	c.JSON(http.StatusOK, gin.H{"user": user, "role": role})
}

func (s *Server) createUser(c *gin.Context) {
	var req struct {
		Username string    `json:"username"`
		Password string    `json:"password"`
		Role     auth.Role `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleGuest
	}
	if err := s.auth.Create(req.Username, req.Password, req.Role); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.create", gin.H{"target": req.Username, "role": req.Role})
	c.JSON(http.StatusCreated, gin.H{"user": req.Username, "role": req.Role})
}

type viewUser struct {
	Name      string    `json:"username"`
	Role      auth.Role `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) listUsers(c *gin.Context) {
	users := s.auth.List()
	out := make([]viewUser, 0, len(users))
	for _, u := range users {
		out = append(out, viewUser{Name: u.Name, Role: u.Role, CreatedAt: u.CreatedAt})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) deleteUser(c *gin.Context) {
	name := c.Param("name")
	if me, _ := c.Get("user"); me == name {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "不能删除当前登录账号"})
		return
	}
	if err := s.auth.Remove(name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.delete", gin.H{"target": name})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) resetPassword(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if err := s.auth.ChangePassword(c.Param("name"), req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.reset_password", gin.H{"target": c.Param("name")})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// logout 清除会话 cookie（JWT 本身无状态，服务端不维护黑名单；到期自然失效）。
func (s *Server) logout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: "ufile_token", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	s.auditAction(c, "auth.logout", nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) resolveSource(c *gin.Context) (*source.Source, bool) {
	src, err := s.sources.Get(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源不存在"})
		return nil, false
	}
	return src, true
}

// —— 源管理 ——

func (s *Server) listSources(c *gin.Context) {
	c.JSON(http.StatusOK, s.sources.List())
}

func (s *Server) getSource(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	out := *src
	out.Config = nil // 详情页不回显配置（含打码值），M1 的编辑功能再定策略
	c.JSON(http.StatusOK, out)
}

// addSource 新增文件源。请求体：
// {"id":"r2-demo","name":"显示名","type":"s3","config":{...}}
func (s *Server) addSource(c *gin.Context) {
	var req struct {
		ID     string         `json:"id"`
		Name   string         `json:"name"`
		Type   string         `json:"type"`
		Config map[string]any `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	src, err := s.sources.Add(req.ID, req.Name, req.Type, req.Config)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.auditAction(c, "source.add", gin.H{"id": req.ID, "type": req.Type})
	c.JSON(http.StatusCreated, src)
}

// updateSource 编辑已有源（ID/类型不可变）。请求体：{"name", "config"}。
// config 中凭据字段为打码值 "******" 时保留数据库里的旧凭据。
func (s *Server) updateSource(c *gin.Context) {
	var req struct {
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	src, err := s.sources.Update(c.Param("id"), req.Name, req.Config, true)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.auditAction(c, "source.update", gin.H{"id": c.Param("id")})
	c.JSON(http.StatusOK, src)
}

func (s *Server) removeSource(c *gin.Context) {
	if err := s.sources.Remove(c.Param("id")); err != nil {
		s.fail(c, err)
		return
	}
	s.auditAction(c, "source.delete", gin.H{"id": c.Param("id")})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// exportSources 导出源配置为 JSON 附件。?redact=1 时凭据打码（模板分享场景），
// 默认全量（含明文凭据，响应头已标注提示）。
func (s *Server) exportSources(c *gin.Context) {
	redact := c.Query("redact") == "1"
	doc := s.sources.Export(redact)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "EXPORT_FAILED", "message": err.Error()})
		return
	}
	name := "ufile-sources-redacted.json"
	if !redact {
		name = "ufile-sources-full.json" // 含明文凭据，文件名本身即提醒
	}
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}

// importSources 导入源配置。请求体：{"content": "<json 字符串 或 原始导出 JSON>", "overwrite": bool}
// 也兼容直接 POST 导出文件原文（Content-Type: application/json，body 即文件内容）。
func (s *Server) importSources(c *gin.Context) {
	var payload []byte
	var overwrite bool

	if strings.Contains(c.ContentType(), "multipart/form-data") {
		fh, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 file 字段"})
			return
		}
		f, err := fh.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
			return
		}
		defer f.Close()
		buf, err := io.ReadAll(io.LimitReader(f, 10<<20)) // 导入文件上限 10MB
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
			return
		}
		payload = buf
		overwrite = c.PostForm("overwrite") == "true"
	} else {
		var req struct {
			Content   json.RawMessage `json:"content"`
			Overwrite bool            `json:"overwrite"`
		}
		raw, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
			return
		}
		if err := json.Unmarshal(raw, &req); err == nil && len(req.Content) > 0 {
			payload = req.Content
			// 前端以字符串形式传 JSON 文本（{"content":"[{...}]"）→ 解出字符串再当载荷
			if isJSONString(payload) {
				var s string
				if err := json.Unmarshal(payload, &s); err == nil {
					payload = []byte(s)
				}
			}
			overwrite = req.Overwrite
		} else {
			payload = raw // 视为导出文件原文直接导入
		}
	}

	rep, err := s.sources.ImportRaw(payload, overwrite)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IMPORT_FAILED", "message": err.Error()})
		return
	}
	s.auditAction(c, "source.import", gin.H{"total": rep.Total, "added": rep.Added, "updated": rep.Updated, "failed": rep.Failed})
	c.JSON(http.StatusOK, rep)
}

// —— 浏览 ——

func (s *Server) listFiles(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	refresh := c.Query("refresh") == "1" // 手动刷新绕过索引缓存
	var res *driver.ListResult
	var err error
	if s.index != nil {
		s.index.Touch(src.ID)
		res, err = s.index.List(c.Request.Context(), src.ID, src.Driver(), c.Query("path"), driver.ListOptions{
			Cursor: c.Query("cursor"),
			Limit:  limit,
		}, refresh)
	} else {
		res, err = src.Driver().List(c.Request.Context(), c.Query("path"), driver.ListOptions{
			Cursor: c.Query("cursor"),
			Limit:  limit,
		})
	}
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// syncStatus 单源同步状态。
func (s *Server) syncStatus(c *gin.Context) {
	if _, ok := s.resolveSource(c); !ok {
		return
	}
	c.JSON(http.StatusOK, s.index.StatusOf(c.Param("id")))
}

// syncTrigger 立即触发单源同步（后台异步执行）。
func (s *Server) syncTrigger(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	go s.index.SyncSource(context.Background(), src.ID, src.Driver())
	s.auditAction(c, "index.sync_trigger", gin.H{"id": src.ID})
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "同步已开始"})
}

// indexStatus 全部源同步状态。
func (s *Server) indexStatus(c *gin.Context) {
	c.JSON(http.StatusOK, s.index.AllStatus())
}

// serveInline /raw 与 /download 共用的流式输出：disposition 决定 inline 还是 attachment。
func (s *Server) serveInline(c *gin.Context, src *source.Source, path, disposition string) {
	st, err := src.Driver().Stat(c.Request.Context(), path)
	if err != nil {
		s.fail(c, err)
		return
	}
	if st.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持下载"})
		return
	}
	rc, err := src.Driver().Open(c.Request.Context(), path, driver.OpenOptions{})
	if err != nil {
		s.fail(c, err)
		return
	}
	defer rc.Close()

	name := sanitizeFilename(st.Name)
	mt := mediaTypeByExt(name)
	if mt != "" {
		c.Header("Content-Type", mt) // 明确类型则不依赖嗅探
	}
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	// 预览（inline）的 XSS 硬化：禁脚本执行/插件；图片/文本等被浏览器当文档渲染也不落地 cookie。
	if disposition == "inline" {
		c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
		c.Header("X-Content-Type-Options", "nosniff")
	}
	http.ServeContent(c.Writer, c.Request, name, st.ModTime, wrapReadSeeker(c.Request.Context(), src.Driver(), path, rc, st.Size))
}

func (s *Server) download(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}

	// 直链模式：显式 ?direct=1 且驱动支持预签名 → 302 跳转，省网关带宽
	if c.Query("direct") == "1" {
		if p, ok := src.Driver().(driver.Presigner); ok {
			u, err := p.PresignGet(c.Request.Context(), path, 15*time.Minute)
			if err != nil {
				s.fail(c, err)
				return
			}
			c.Redirect(http.StatusFound, u)
			return
		}
	}

	s.serveInline(c, src, path, "attachment")
}

// testSource 连接测试（不落盘）。请求体与 addSource 相同（id 可省略）。
func (s *Server) testSource(c *gin.Context) {
	var req struct {
		Type   string         `json:"type"`
		Config map[string]any `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	c.JSON(http.StatusOK, s.sources.Test(ctx, req.Type, req.Config))
}

// raw 内联预览输出：仅允许白名单内的可预览类型，杜绝任意 HTML/SVG 直出钓鱼。
func (s *Server) raw(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	if previewKind(path) == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "NOT_PREVIEWABLE", "message": "该文件类型不支持预览"})
		return
	}
	s.serveInline(c, src, path, "inline")
}

func (s *Server) presign(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	p, ok := src.Driver().(driver.Presigner)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持直链"})
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	ttl := 15 * time.Minute
	if v := c.Query("ttl"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	u, err := p.PresignGet(c.Request.Context(), path, ttl)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": u, "expires_in": int(ttl.Seconds())})
}

// isJSONString 判断原始 JSON 值是否为字符串字面量（首字符为 "）。
func isJSONString(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		return c == '"'
	}
	return false
}

func (s *Server) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, source.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源不存在"})
	case errors.Is(err, source.ErrDuplicate):
		c.JSON(http.StatusConflict, gin.H{"code": "SOURCE_DUPLICATE", "message": err.Error()})
	case errors.Is(err, source.ErrBadID):
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
	case errors.Is(err, driver.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "对象不存在"})
	case errors.Is(err, driver.ErrInvalidPath):
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PATH", "message": "非法路径"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"code": "UPSTREAM_ERROR", "message": err.Error()})
	}
}

// sanitizeFilename 清理会破坏 HTTP 头的字符；非 ASCII 名由 FormatMediaType 转 RFC 5987 编码。
func sanitizeFilename(name string) string {
	if name == "" {
		return "file"
	}
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
}

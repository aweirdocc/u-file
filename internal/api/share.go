package api

// M3 分享外链：管理面（admin 或拥有 share 权限的用户）+ 公开访客面（无登录）。
// 访客凭 token 访问，提取码通过后签发作用域隔离的 grant cookie（1 小时免输码）。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/auth"
	"ufile/internal/share"
	"ufile/internal/webhook"
)

// publicShare 把存储记录转为对外视图（不暴露哈希与创建者以外的内部信息）。
func publicShare(s *share.Share) gin.H {
	out := gin.H{
		"id":           s.ID,
		"token":        s.Token,
		"source_id":    s.SourceID,
		"path":         s.Path,
		"name":         s.Name,
		"size":         s.Size,
		"is_dir":       s.IsDir,
		"has_password": s.HasPassword(),
		"expires_at":   s.ExpiresAt,
		"downloads":    s.Downloads,
		"created_by":   s.CreatedBy,
		"created_at":   s.CreatedAt,
		"revoked":      s.Revoked,
	}
	if s.MaxDownloads > 0 {
		out["max_downloads"] = s.MaxDownloads
	}
	return out
}

// createShare 创建分享。请求体：
// {"source_id","path","ttl_hours"(必填,≤720),"password"(可选),
//  "max_downloads"(可选,0=不限)}
func (s *Server) createShare(c *gin.Context) {
	if s.auth != nil && !s.auth.CanShareAny(s.currentUsername(c)) {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无外链分享权限"})
		return
	}
	var req struct {
		SourceID     string  `json:"source_id"`
		Path         string  `json:"path"`
		TTLHours     float64 `json:"ttl_hours"`
		Password     string  `json:"password"`
		MaxDownloads int64   `json:"max_downloads"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if req.Path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path"})
		return
	}
	if !s.checkPathAccess(c, req.SourceID, req.Path, auth.ActionShare) {
		return
	}
	src, err := s.sources.Get(req.SourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源不存在"})
		return
	}
	st, err := src.Driver().Stat(c.Request.Context(), req.Path)
	if err != nil {
		s.fail(c, err)
		return
	}
	username := s.currentUsername(c)
	sh, err := s.shares.Create(share.CreateInput{
		SourceID:     src.ID,
		Path:         req.Path,
		Name:         st.Name,
		Size:         st.Size,
		IsDir:        st.IsDir,
		Password:     req.Password,
		Lifetime:     time.Duration(req.TTLHours * float64(time.Hour)),
		MaxDownloads: req.MaxDownloads,
		CreatedBy:    username,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "share.create", gin.H{"id": sh.ID, "source": src.ID, "path": req.Path, "has_password": sh.HasPassword(), "max_downloads": sh.MaxDownloads})
	s.emitWebhook(webhook.EventShareCreated, src.ID, publicShare(sh))
	c.JSON(http.StatusCreated, publicShare(sh))
}

// listShares 分享管理列表（admin 查看全部；有 share 权限的用户查看自己创建的分享）。
func (s *Server) listShares(c *gin.Context) {
	username := s.currentUsername(c)
	if s.auth != nil && !s.auth.CanShareAny(username) {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无外链分享权限"})
		return
	}
	admin := s.isAdmin(c)
	list := s.shares.List()
	out := make([]gin.H, 0, len(list))
	for _, sh := range list {
		if !admin && sh.CreatedBy != username {
			continue
		}
		out = append(out, publicShare(sh))
	}
	c.JSON(http.StatusOK, out)
}

// deleteShare 撤销分享（admin 可撤销任意；普通用户仅可撤销自己创建的分享）。
func (s *Server) deleteShare(c *gin.Context) {
	username := s.currentUsername(c)
	if s.auth != nil && !s.auth.CanShareAny(username) {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无外链分享权限"})
		return
	}
	id := c.Param("id")
	if !s.isAdmin(c) {
		for _, sh := range s.shares.List() {
			if sh.ID == id && sh.CreatedBy != username {
				c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "不能撤销他人创建的分享"})
				return
			}
		}
	}
	if err := s.shares.Delete(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SHARE_NOT_FOUND", "message": "分享不存在或已删除"})
		return
	}
	s.auditAction(c, "share.delete", gin.H{"id": id})
	s.emitWebhook(webhook.EventShareDeleted, "", gin.H{"id": id, "user": username})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// resolveShare 按 URL 中的 :token 取可访问的分享，失败时写响应并返回 false。
func (s *Server) resolveShare(c *gin.Context) (*share.Share, bool) {
	sh, err := s.shares.Accessible(c.Param("token"))
	if err != nil {
		s.shareFail(c, err)
		return nil, false
	}
	return sh, true
}

// shareFail 分享公开端点的错误映射（给访客可读但不泄露内部细节）。
func (s *Server) shareFail(c *gin.Context, err error) {
	switch {
	case err == share.ErrNotFound:
		c.JSON(http.StatusNotFound, gin.H{"code": "SHARE_NOT_FOUND", "message": "分享不存在或已失效"})
	case err == share.ErrExpired:
		c.JSON(http.StatusGone, gin.H{"code": "SHARE_EXPIRED", "message": "分享链接已过期"})
	case err == share.ErrRevoked:
		c.JSON(http.StatusGone, gin.H{"code": "SHARE_REVOKED", "message": "分享已被撤销"})
	case err == share.ErrExhausted:
		c.JSON(http.StatusGone, gin.H{"code": "SHARE_EXHAUSTED", "message": "下载次数已用完"})
	case err == share.ErrBadPassword:
		c.JSON(http.StatusUnauthorized, gin.H{"code": "BAD_PASSWORD", "message": "提取码错误"})
	default:
		if strings.Contains(err.Error(), "尝试次数过多") {
			c.JSON(http.StatusTooManyRequests, gin.H{"code": "RATE_LIMITED", "message": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL", "message": "服务异常"})
	}
}

// grantCookieName 每个分享一个 cookie 名（token 哈希前 8 位），避免串权限。
func grantCookieName(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "ufile_sg_" + hex.EncodeToString(sum[:4])
}

// grantOK 判断请求是否携带对该分享有效的 grant cookie。
func (s *Server) grantOK(c *gin.Context, token string) bool {
	ck, err := c.Request.Cookie(grantCookieName(token))
	if err != nil || ck.Value == "" {
		return false
	}
	return s.shares.VerifyGrant(token, ck.Value)
}

// issueGrant 提取码验证通过后签发 grant cookie（Path 限定到该分享的路径下）。
func (s *Server) issueGrant(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     grantCookieName(token),
		Value:    s.shares.SignGrant(token),
		Path:     "/api/v1/s/" + token + "/",
		MaxAge:   int(share.GrantTTL().Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure(c),
		SameSite: http.SameSiteLaxMode,
	})
}

// shareInfo 公开分享信息：名称/大小/是否需提取码/剩余次数/过期时间。
func (s *Server) shareInfo(c *gin.Context) {
	sh, ok := s.resolveShare(c)
	if !ok {
		return
	}
	out := publicShare(sh)
	out["unlocked"] = !sh.HasPassword() || s.grantOK(c, sh.Token)
	c.JSON(http.StatusOK, out)
}

// shareAccess 校验提取码并签发 grant cookie。
func (s *Server) shareAccess(c *gin.Context) {
	token := c.Param("token")
	if _, err := s.shares.Accessible(token); err != nil {
		s.shareFail(c, err)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = c.ShouldBindJSON(&req)
	key := token + "|" + c.ClientIP()
	if err := s.shares.CheckPassword(token, req.Password, key); err != nil {
		s.shareFail(c, err)
		return
	}
	s.issueGrant(c, token)
	s.auditActionAs(c, "share.access", gin.H{"share": token}, "share:"+token)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// shareDownload 公开下载：文件流式输出（支持 Range），目录 zip 打包。
func (s *Server) shareDownload(c *gin.Context) {
	sh, ok := s.resolveShare(c)
	if !ok {
		return
	}
	if sh.HasPassword() && !s.grantOK(c, sh.Token) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "PASSWORD_REQUIRED", "message": "请先输入提取码"})
		return
	}
	src, err := s.sources.Get(sh.SourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源已不可用"})
		return
	}
	if err := s.shares.CountDownload(sh.Token); err != nil {
		s.shareFail(c, err)
		return
	}
	if sh.IsDir {
		s.archiveEntries(c, src, []string{sh.Path}, sh.Name)
		return
	}
	s.serveInline(c, src, sh.Path, "attachment")
	s.auditActionAs(c, "share.download", gin.H{"share": sh.ID, "source": sh.SourceID, "path": sh.Path}, "share:"+sh.ID)
}

// shareRaw 公开预览：与登录态 /raw 同一白名单与 inline 硬化，不计数。
func (s *Server) shareRaw(c *gin.Context) {
	sh, ok := s.resolveShare(c)
	if !ok {
		return
	}
	if sh.HasPassword() && !s.grantOK(c, sh.Token) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "PASSWORD_REQUIRED", "message": "请先输入提取码"})
		return
	}
	if sh.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持预览"})
		return
	}
	if previewKind(sh.Name) == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "NOT_PREVIEWABLE", "message": "该文件类型不支持预览"})
		return
	}
	src, err := s.sources.Get(sh.SourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源已不可用"})
		return
	}
	s.serveInline(c, src, sh.Path, "inline")
}

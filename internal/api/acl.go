package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/apitoken"
	"ufile/internal/auth"
	"ufile/internal/mount"
	"ufile/internal/source"
)

// sourceView 带当前用户权限元数据的源视图。
type sourceView struct {
	*source.Source
	Access *auth.SourceAccess `json:"access,omitempty"`
}

func (s *Server) currentUsername(c *gin.Context) string {
	u, _ := c.Get("user")
	name, _ := u.(string)
	return name
}

func (s *Server) currentRole(c *gin.Context) auth.Role {
	r, _ := c.Get("role")
	role, _ := r.(auth.Role)
	return role
}

func (s *Server) isAdmin(c *gin.Context) bool {
	if s.auth == nil {
		return true
	}
	return s.currentRole(c) == auth.RoleAdmin
}

// checkVFSAccessible 判断用户是否有权访问至少一个挂载点对应的物理源。
func (s *Server) checkVFSAccessible(user string, admin bool) bool {
	if s.mounts == nil || s.mounts.Count() == 0 {
		return false
	}
	if s.auth == nil || admin {
		return true
	}
	for _, m := range s.mounts.ListMounts() {
		if s.auth.CanAccess(user, m.SourceID, m.SourcePath, auth.ActionRead) {
			return true
		}
	}
	return false
}

// checkAPITokenSource 检查 API Token（若当前请求使用 API Token）对指定源和操作的作用域限制。
func (s *Server) checkAPITokenSource(c *gin.Context, sourceID string, act auth.Action) bool {
	v, exists := c.Get("api_token")
	if !exists {
		return true
	}
	tok, ok := v.(*apitoken.Token)
	if !ok || tok == nil {
		return true
	}
	needScope := actionToTokenScope(act)
	if !tok.HasScope(needScope) {
		c.JSON(http.StatusForbidden, gin.H{"code": "SCOPE_FORBIDDEN", "message": "API Token 缺少执行该操作的作用域: " + string(needScope)})
		return false
	}
	if sourceID != "" && sourceID != mount.VirtualSourceID && !tok.AllowsSource(sourceID) {
		c.JSON(http.StatusForbidden, gin.H{"code": "SOURCE_FORBIDDEN", "message": "API Token 无权访问该文件源"})
		return false
	}
	return true
}

// checkSourceAccess 检查当前登录用户是否对 sourceID 拥有 act 权限（不限子路径）。
// 未通过时直接写入 403 响应并返回 false。
func (s *Server) checkSourceAccess(c *gin.Context, sourceID string, act auth.Action) bool {
	if !s.checkAPITokenSource(c, sourceID, act) {
		return false
	}
	if sourceID == mount.VirtualSourceID {
		if s.mounts == nil {
			c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "虚拟挂载未启用"})
			return false
		}
		if s.auth == nil || s.isAdmin(c) {
			return true
		}
		if act == auth.ActionRead && s.checkVFSAccessible(s.currentUsername(c), s.isAdmin(c)) {
			return true
		}
		// 对写/删操作，具体路径权限在 checkPathAccess 按挂载路由判定
		if act == auth.ActionWrite || act == auth.ActionDelete {
			return true
		}
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无权访问虚拟挂载源"})
		return false
	}
	if s.auth == nil {
		return true
	}
	user := s.currentUsername(c)
	if !s.auth.CanAccessSource(user, sourceID, act) {
		s.auditAction(c, "acl.denied", gin.H{"source": sourceID, "action": act})
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无权访问该文件源或执行此操作"})
		return false
	}
	return true
}

// checkPathAccess 检查当前登录用户是否对 sourceID 的 targetPath 拥有 act 权限。
// 未通过时直接写入 403 响应并返回 false。
func (s *Server) checkPathAccess(c *gin.Context, sourceID, targetPath string, act auth.Action) bool {
	if sourceID == mount.VirtualSourceID {
		if s.mounts == nil {
			c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "虚拟挂载未启用"})
			return false
		}
		route, err := s.mounts.Resolve(targetPath)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PATH", "message": "非法路径"})
			return false
		}
		if route == nil {
			// 落在虚拟中间导航目录：仅允许 Read，写/删由驱动或此处拦截
			if act == auth.ActionRead {
				if !s.checkAPITokenSource(c, mount.VirtualSourceID, act) {
					return false
				}
				if s.checkVFSAccessible(s.currentUsername(c), s.isAdmin(c)) {
					return true
				}
				c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无权浏览虚拟挂载目录"})
				return false
			}
			c.JSON(http.StatusBadRequest, gin.H{"code": "VIRTUAL_NAV_DIR", "message": mount.ErrVirtualNavDir.Error()})
			return false
		}
		if route.Mount.Readonly && (act == auth.ActionWrite || act == auth.ActionDelete) {
			c.JSON(http.StatusForbidden, gin.H{"code": "MOUNT_READONLY", "message": mount.ErrMountReadOnly.Error()})
			return false
		}
		sourceID = route.Mount.SourceID
		targetPath = route.PhysicalPath
	}

	if !s.checkAPITokenSource(c, sourceID, act) {
		return false
	}
	if s.auth == nil {
		return true
	}
	user := s.currentUsername(c)
	if !s.auth.CanAccess(user, sourceID, targetPath, act) {
		s.auditAction(c, "acl.denied", gin.H{"source": sourceID, "path": targetPath, "action": act})
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "权限不足：无权访问该路径或执行此操作"})
		return false
	}
	return true
}

// —— 用户管理与自省接口 ——

type viewUser struct {
	Name      string        `json:"username"`
	Role      auth.Role     `json:"role"`
	Disabled  bool          `json:"disabled"`
	Remark    string        `json:"remark,omitempty"`
	Policies  []auth.Policy `json:"policies"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at,omitempty"`
}

func (s *Server) me(c *gin.Context) {
	username := s.currentUsername(c)
	role := s.currentRole(c)
	var policies []auth.Policy
	canShare := true
	if s.auth != nil {
		if u, ok := s.auth.Get(username); ok {
			policies = u.Policies
		}
		canShare = s.auth.CanShareAny(username)
	}
	if policies == nil {
		policies = []auth.Policy{}
	}
	c.JSON(http.StatusOK, gin.H{
		"user":      username,
		"role":      role,
		"can_share": canShare,
		"policies":  policies,
	})
}

func (s *Server) listUsers(c *gin.Context) {
	users := s.auth.List()
	out := make([]viewUser, 0, len(users))
	for _, u := range users {
		pols := u.Policies
		if pols == nil {
			pols = []auth.Policy{}
		}
		out = append(out, viewUser{
			Name:      u.Name,
			Role:      u.Role,
			Disabled:  u.Disabled,
			Remark:    u.Remark,
			Policies:  pols,
			CreatedAt: u.CreatedAt,
			UpdatedAt: u.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) createUser(c *gin.Context) {
	var req struct {
		Username    string        `json:"username"`
		Password    string        `json:"password"`
		PasswordEnc string        `json:"password_enc"`
		Role        auth.Role     `json:"role"`
		Disabled    bool          `json:"disabled"`
		Remark      string        `json:"remark"`
		Policies    []auth.Policy `json:"policies"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	password, ok := s.passwordOf(c, &bootstrapReq{
		Username:    req.Username,
		Password:    req.Password,
		PasswordEnc: req.PasswordEnc,
	})
	if !ok {
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleGuest
	}
	if err := s.auth.CreateWithOptions(auth.CreateUserOptions{
		Name:     req.Username,
		Password: password,
		Role:     req.Role,
		Disabled: req.Disabled,
		Remark:   req.Remark,
		Policies: req.Policies,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.create", gin.H{"target": req.Username, "role": req.Role, "policies": len(req.Policies)})
	u, _ := s.auth.Get(req.Username)
	pols := u.Policies
	if pols == nil {
		pols = []auth.Policy{}
	}
	c.JSON(http.StatusCreated, gin.H{
		"user":     u.Name,
		"username": u.Name,
		"role":     u.Role,
		"disabled": u.Disabled,
		"remark":   u.Remark,
		"policies": pols,
	})
}

// updateUser 更新用户基本信息（角色、禁用状态、备注、策略）。
func (s *Server) updateUser(c *gin.Context) {
	target := c.Param("name")
	var req struct {
		Role     *auth.Role     `json:"role"`
		Disabled *bool          `json:"disabled"`
		Remark   *string        `json:"remark"`
		Policies *[]auth.Policy `json:"policies"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	me := s.currentUsername(c)
	if target == me {
		if req.Disabled != nil && *req.Disabled {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "不能禁用当前登录账号"})
			return
		}
		if req.Role != nil && *req.Role != auth.RoleAdmin {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "不能降级当前登录账号的管理员角色"})
			return
		}
	}
	if err := s.auth.UpdateUser(target, auth.UpdateUserOptions{
		Role:     req.Role,
		Disabled: req.Disabled,
		Remark:   req.Remark,
		Policies: req.Policies,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.update", gin.H{"target": target, "role": req.Role, "disabled": req.Disabled})
	u, _ := s.auth.Get(target)
	pols := u.Policies
	if pols == nil {
		pols = []auth.Policy{}
	}
	c.JSON(http.StatusOK, viewUser{
		Name:      u.Name,
		Role:      u.Role,
		Disabled:  u.Disabled,
		Remark:    u.Remark,
		Policies:  pols,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	})
}

// updateUserPolicies 单独更新指定用户的 ACL 策略集合。
func (s *Server) updateUserPolicies(c *gin.Context) {
	target := c.Param("name")
	var req struct {
		Policies []auth.Policy `json:"policies"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	if err := s.auth.SetPolicies(target, req.Policies); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	s.auditAction(c, "auth.user.policies", gin.H{"target": target, "count": len(req.Policies)})
	u, _ := s.auth.Get(target)
	pols := u.Policies
	if pols == nil {
		pols = []auth.Policy{}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "policies": pols})
}

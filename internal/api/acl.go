package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/auth"
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

// checkSourceAccess 检查当前登录用户是否对 sourceID 拥有 act 权限（不限子路径）。
// 未通过时直接写入 403 响应并返回 false。
func (s *Server) checkSourceAccess(c *gin.Context, sourceID string, act auth.Action) bool {
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

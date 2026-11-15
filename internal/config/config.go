// Package config 加载 M0（PoC）阶段的 YAML 源配置。
// 生产阶段源配置将迁移到元数据库并加密存储（见 docs/04-总体架构设计），
// 本实现仅为 PoC 联调用，凭据以明文存放于本地配置文件。
package config

import (
	"fmt"
	"os"

	"ufile/internal/source"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Port int `yaml:"port"`
	// TrustedProxies 受信反向代理 IP/CIDR；为空时不采信 X-Forwarded-For（防伪造 IP 绕过限速）。
	TrustedProxies []string `yaml:"trusted_proxies"`
	// SecureCookie 强制 Cookie 带 Secure（HTTPS 在反向代理终止时开启）。
	SecureCookie bool `yaml:"secure_cookie"`
	// GatewaySecret AgentGuard 网关 HMAC-SHA256 签名密钥（亦可通过环境变量 UFILE_GATEWAY_SECRET 注入）。
	GatewaySecret string `yaml:"gateway_secret"`
	// RequireGatewayForToken 强制所有 uft_ API Token 请求必须通过已认证网关进入。
	RequireGatewayForToken bool `yaml:"require_gateway_for_token"`
	// GatewayFailClosedAudit 控制网关来源的高风险写/删操作在审计不可写时是否阻断（默认开启）。
	GatewayFailClosedAudit *bool `yaml:"gateway_fail_closed_audit"`
	// AgentGuardAdminURL 可选，a-guard HTTP Gateway 管理地址（如 http://127.0.0.1:8765）；配置后在源或权限变更时主动通知其同步策略。
	AgentGuardAdminURL string `yaml:"agentguard_admin_url"`
	// AgentGuardAdminSecret 可选，a-guard /_admin/policy/sync 的签名密钥（未填时复用 GatewaySecret，亦可通过 UFILE_AGENTGUARD_ADMIN_SECRET 注入）。
	AgentGuardAdminSecret string `yaml:"agentguard_admin_secret"`
	// AgentGuardAppID 可选，通知 a-guard 同步时携带的 app_id（默认 "ufile"）。
	AgentGuardAppID string `yaml:"agentguard_app_id"`
}

// Webhook 事件投递安全配置。
type Webhook struct {
	// AllowPrivateTargets 允许投递到内网/回环地址（默认 false，防 SSRF）。
	AllowPrivateTargets bool `yaml:"allow_private_targets"`
}

type Source struct {
	ID     string         `yaml:"id"`
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"` // local / s3 / ...
	Config map[string]any `yaml:"config"`
}

// Seeds 导出为 source.Manager 的播种定义。
func (c *Config) Seeds() []Seed {
	out := make([]Seed, 0, len(c.Sources))
	for _, s := range c.Sources {
		out = append(out, Seed{ID: s.ID, Name: s.Name, Type: s.Type, Config: s.Config})
	}
	return out
}

type Seed = source.Seed

type Config struct {
	Server  Server   `yaml:"server"`
	Webhook Webhook  `yaml:"webhook"`
	Sources []Source `yaml:"sources"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.ID == "" || s.Type == "" {
			return nil, fmt.Errorf("config: source #%d 缺少 id 或 type", i+1)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("config: source id 重复: %s", s.ID)
		}
		seen[s.ID] = true
	}
	return &c, nil
}

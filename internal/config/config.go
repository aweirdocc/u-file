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

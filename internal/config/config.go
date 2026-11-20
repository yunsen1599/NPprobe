// Package config 测试端本地配置（/etc/npprobe/config.yml）。
// 只放连接面板所需的两项；测试参数（间隔、并发、超时、测试地址）都由面板下发。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Panel 面板接口地址，例如 https://panel.example.com/api
	Panel string `yaml:"panel"`
	// Token 面板「测试端」里新建或重置后显示的 token
	Token string `yaml:"token"`
	// LogLevel debug / info（默认）/ warn
	LogLevel string `yaml:"log_level"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Panel = strings.TrimRight(strings.TrimSpace(c.Panel), "/")
	c.Token = strings.TrimSpace(c.Token)
	u, err := url.Parse(c.Panel)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("panel must be an http(s) URL, got %q", c.Panel)
	}
	if c.Token == "" {
		return nil, fmt.Errorf("token is required")
	}
	return &c, nil
}

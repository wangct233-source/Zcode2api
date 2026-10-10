// Package config 定义 Zcode2api 的配置模型与加载逻辑。
//
// 优先级：环境变量 > config.yaml > 内置默认值。
// 面板修改配置后会写回 config.yaml，重启不丢。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PoolConfig 账号池并发与冷却参数。
type PoolConfig struct {
	// MaxConcurrentPerAccount 单账号总并发闸。上游实测上限 3，默认 2 留余量。
	MaxConcurrentPerAccount int `yaml:"maxConcurrentPerAccount"`
	// MaxConcurrentPerModel 单模型并发闸（map[模型名]上限，"default" 为兜底）。
	MaxConcurrentPerModel map[string]int `yaml:"maxConcurrentPerModel"`
	// MinSpacingMs 同账号两次派发的最小间隔（毫秒）。
	MinSpacingMs int64 `yaml:"minSpacingMs"`
	// CooldownMs 账号级冷却（毫秒），429/3008 触发。
	CooldownMs int64 `yaml:"cooldownMs"`
	// ModelCooldownMs 模型级冷却（毫秒），3009 触发。
	ModelCooldownMs int64 `yaml:"modelCooldownMs"`
	// QuotaHoldMs 配额耗尽持有（毫秒），402/1113 触发。
	QuotaHoldMs int64 `yaml:"quotaHoldMs"`
}

// UpdaterConfig 热更新配置。
type UpdaterConfig struct {
	Enabled       bool   `yaml:"enabled"`       // 是否开启自动检查
	Repo          string `yaml:"repo"`          // GitHub 仓库（owner/name）
	IntervalHours int    `yaml:"intervalHours"` // 检查周期（小时），0 表示仅手动检查
	Mirrors       string `yaml:"mirrors"`       // 备用镜像（逗号分隔），GitHub 不可达时回退
}

// ClaimConfig 免费额度自动领取（Rod 无头浏览器方案）。
type ClaimConfig struct {
	Enabled        bool `yaml:"enabled"`          // 总开关（实验性，默认关）
	PollIntervalMs int64 `yaml:"pollIntervalMs"`  // 轮询周期
	Headless       bool `yaml:"headless"`        // 无头模式
}

// Config 顶层配置。
type Config struct {
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"server"`

	Auth struct {
		ProxyAPIKey   string `yaml:"proxyApiKey"`   // 客户端调用密钥；未设置则不校验
		PanelPassword string `yaml:"panelPassword"` // 面板密码；未设置则面板锁定（无兜底键）
	} `yaml:"auth"`

	Provider struct {
		Name          string `yaml:"name"`          // zai | bigmodel
		OpenAIBase    string `yaml:"openaiBase"`    // 上游 OpenAI 兼容端点
		AnthropicBase string `yaml:"anthropicBase"` // 备用：Anthropic 兼容端点（预留）
	} `yaml:"provider"`

	DefaultModel string       `yaml:"defaultModel"`
	Models       []string     `yaml:"models"`
	Pool         PoolConfig   `yaml:"pool"`
	Updater      UpdaterConfig `yaml:"updater"`
	Claim        ClaimConfig  `yaml:"claim"`

	// DeviceMid 全局设备指纹（首次启动生成并写回配置）。
	DeviceMid string `yaml:"deviceMid"`
}

// Default 返回内置默认配置。
func Default() *Config {
	c := &Config{}
	// 默认 0.0.0.0：服务自带鉴权（面板密码 + API Key），
	// 绑 127.0.0.1 会让 Docker 端口映射和服务器部署"看起来起了但连不上"，
	// 是新手最高频的部署故障。确实只想本机用的用户可改配置或设 ZG_HOST=127.0.0.1。
	c.Server.Host = "0.0.0.0"
	c.Server.Port = 17800
	c.Provider.Name = "zai"
	c.Provider.OpenAIBase = "https://api.z.ai/api/coding/paas/v4"
	c.Provider.AnthropicBase = "https://api.z.ai/api/anthropic"
	c.DefaultModel = "glm-4.6"
	c.Models = []string{"glm-4.6", "glm-4.5", "glm-4.5-air", "glm-4.5-flash"}
	c.Pool = PoolConfig{
		MaxConcurrentPerAccount: 2,
		MaxConcurrentPerModel:   map[string]int{"default": 2, "glm-4.6": 2},
		MinSpacingMs:            1500,
		CooldownMs:              60000,
		ModelCooldownMs:         5000,
		QuotaHoldMs:             30 * 60000,
	}
	c.Updater = UpdaterConfig{
		Enabled:       true,
		Repo:          "wangct233-source/Zcode2api",
		IntervalHours: 24,
	}
	c.Claim = ClaimConfig{Enabled: false, PollIntervalMs: 5 * 3600 * 1000, Headless: true}
	return c
}

// ProviderPresets 内置 provider 端点预设。
var ProviderPresets = map[string][2]string{
	"zai":      {"https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/anthropic"},
	"bigmodel": {"https://open.bigmodel.cn/api/coding/paas/v4", "https://open.bigmodel.cn/api/anthropic"},
}

// Load 按优先级加载配置：env > YAML > 默认值。
func Load(path string) (*Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	applyEnv(c)
	normalize(c)
	return c, nil
}

// applyEnv 应用环境变量覆盖（前缀 ZG_）。
func applyEnv(c *Config) {
	if v := os.Getenv("ZG_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("ZG_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.Port = n
		}
	}
	if v := os.Getenv("ZG_PROXY_API_KEY"); v != "" {
		c.Auth.ProxyAPIKey = v
	}
	if v := os.Getenv("ZG_PANEL_PASSWORD"); v != "" {
		c.Auth.PanelPassword = v
	}
	if v := os.Getenv("ZG_PROVIDER"); v != "" {
		c.Provider.Name = v
	}
	if v := os.Getenv("ZG_UPSTREAM_BASE"); v != "" {
		c.Provider.OpenAIBase = v
	}
	if v := os.Getenv("ZG_UPDATE_REPO"); v != "" {
		c.Updater.Repo = v
	}
	if v := os.Getenv("ZG_STORE_DIR"); v != "" {
		storeDirOverride = v
	}
}

var storeDirOverride string

// StoreDirOverride 返回状态目录覆盖值（供 main 使用）。
func StoreDirOverride() string { return storeDirOverride }

// normalize 做一致性修正：provider 预设、模型白名单补全等。
func normalize(c *Config) {
	if p, ok := ProviderPresets[strings.ToLower(c.Provider.Name)]; ok && c.Provider.OpenAIBase == "" {
		c.Provider.OpenAIBase, c.Provider.AnthropicBase = p[0], p[1]
	}
	c.Provider.OpenAIBase = strings.TrimRight(c.Provider.OpenAIBase, "/")
	found := false
	for _, m := range c.Models {
		if m == c.DefaultModel {
			found = true
			break
		}
	}
	if !found {
		c.Models = append([]string{c.DefaultModel}, c.Models...)
	}
	if _, ok := c.Pool.MaxConcurrentPerModel["default"]; !ok {
		if c.Pool.MaxConcurrentPerModel == nil {
			c.Pool.MaxConcurrentPerModel = map[string]int{}
		}
		c.Pool.MaxConcurrentPerModel["default"] = 2
	}
}

// Save 将配置写回 YAML 文件（面板回写用）。
func Save(path string, c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

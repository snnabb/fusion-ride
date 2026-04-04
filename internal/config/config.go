package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config 是 FusionRide 的顶层配置。
type Config struct {
	mu sync.RWMutex

	Server   ServerConfig   `yaml:"server"`
	Admin    AdminConfig    `yaml:"admin"`
	Playback PlaybackConfig `yaml:"playback"`
	Timeouts TimeoutConfig  `yaml:"timeouts"`
	Bitrate  BitrateConfig  `yaml:"bitrate"`
	Proxies  []ProxyConfig  `yaml:"proxies"`
	Upstream []UpstreamDef  `yaml:"upstream"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Name string `yaml:"name"`
	ID   string `yaml:"id,omitempty"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type PlaybackConfig struct {
	Mode string `yaml:"mode"` // "proxy" | "redirect"
}

type TimeoutConfig struct {
	API            int `yaml:"api"`
	Aggregate      int `yaml:"aggregate"`
	Login          int `yaml:"login"`
	HealthCheck    int `yaml:"healthCheck"`
	HealthInterval int `yaml:"healthInterval"`
}

type BitrateConfig struct {
	PreferHighest bool     `yaml:"preferHighest"`
	CodecPriority []string `yaml:"codecPriority"`
}

type ProxyConfig struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Enabled bool   `yaml:"enabled"`
}

type UpstreamDef struct {
	Name             string `yaml:"name"`
	URL              string `yaml:"url"`
	Username         string `yaml:"username,omitempty"`
	Password         string `yaml:"password,omitempty"`
	APIKey           string `yaml:"apiKey,omitempty"`
	PlaybackMode     string `yaml:"playbackMode,omitempty"`
	StreamingURL     string `yaml:"streamingUrl,omitempty"`
	SpoofClient      string `yaml:"spoofClient,omitempty"`
	CustomUA         string `yaml:"customUserAgent,omitempty"`
	CustomClient     string `yaml:"customClient,omitempty"`
	CustomVersion    string `yaml:"customClientVersion,omitempty"`
	CustomDevice     string `yaml:"customDeviceName,omitempty"`
	CustomDeviceID   string `yaml:"customDeviceId,omitempty"`
	ProxyID          string `yaml:"proxyId,omitempty"`
	PriorityMetadata bool   `yaml:"priorityMetadata,omitempty"`
	FollowRedirects  bool   `yaml:"followRedirects"`
}

// Default 返回一份合理的默认配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 8096,
			Name: "FusionRide",
		},
		Admin: AdminConfig{
			Username: "admin",
		},
		Playback: PlaybackConfig{
			Mode: "proxy",
		},
		Timeouts: TimeoutConfig{
			API:            30000,
			Aggregate:      15000,
			Login:          10000,
			HealthCheck:    10000,
			HealthInterval: 60000,
		},
		Bitrate: BitrateConfig{
			PreferHighest: true,
			CodecPriority: []string{"hevc", "av1", "h264"},
		},
	}
}

// Load 从 YAML 文件加载配置，缺失字段使用默认值。
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.validate()
	return cfg, nil
}

// Save 以原子写入方式保存配置到文件。
func (c *Config) Save(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	return os.Rename(tmp, path)
}

// UpdateFunc 以线程安全的方式更新配置。
func (c *Config) UpdateFunc(fn func(cfg *Config)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// Snapshot 返回配置的只读副本。
func (c *Config) Snapshot() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := *c
	cp.Upstream = make([]UpstreamDef, len(c.Upstream))
	copy(cp.Upstream, c.Upstream)
	cp.Proxies = make([]ProxyConfig, len(c.Proxies))
	copy(cp.Proxies, c.Proxies)
	return cp
}

func (c *Config) validate() {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		c.Server.Port = 8096
	}
	if c.Server.Name == "" {
		c.Server.Name = "FusionRide"
	}
	if c.Playback.Mode == "" {
		c.Playback.Mode = "proxy"
	}
	if c.Timeouts.API <= 0 {
		c.Timeouts.API = 30000
	}
	if c.Timeouts.Aggregate <= 0 {
		c.Timeouts.Aggregate = 15000
	}
	if c.Timeouts.HealthInterval <= 0 {
		c.Timeouts.HealthInterval = 60000
	}
	if len(c.Bitrate.CodecPriority) == 0 {
		c.Bitrate.CodecPriority = []string{"hevc", "av1", "h264"}
	}
}

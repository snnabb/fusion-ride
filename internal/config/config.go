package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

var defaultCodecPriority = []string{"hevc", "av1", "h264"}

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
	Mode string `yaml:"mode"`
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

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 8096,
			Name: "FusionRide",
			ID:   "fusionride",
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
			CodecPriority: append([]string(nil), defaultCodecPriority...),
		},
		Proxies:  []ProxyConfig{},
		Upstream: []UpstreamDef{},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.Validate()
	return cfg, nil
}

func (c *Config) Save(path string) error {
	c.mu.RLock()
	data, err := yaml.Marshal(c.snapshotLocked())
	c.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}

	return nil
}

func (c *Config) UpdateFunc(fn func(cfg *Config)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fn(c)
	c.validateLocked()
}

func (c *Config) Validate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.validateLocked()
}

func (c *Config) Snapshot() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked()
}

func (c *Config) snapshotLocked() *Config {
	snapshot := &Config{
		Server:   c.Server,
		Admin:    c.Admin,
		Playback: c.Playback,
		Timeouts: c.Timeouts,
		Bitrate: BitrateConfig{
			PreferHighest: c.Bitrate.PreferHighest,
			CodecPriority: append([]string(nil), c.Bitrate.CodecPriority...),
		},
		Proxies:  append([]ProxyConfig(nil), c.Proxies...),
		Upstream: append([]UpstreamDef(nil), c.Upstream...),
	}
	return snapshot
}

func (c *Config) validateLocked() {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		c.Server.Port = 8096
	}
	if c.Server.Name == "" {
		c.Server.Name = "FusionRide"
	}
	if c.Server.ID == "" {
		c.Server.ID = "fusionride"
	}
	if c.Admin.Username == "" {
		c.Admin.Username = "admin"
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
	if c.Timeouts.Login <= 0 {
		c.Timeouts.Login = 10000
	}
	if c.Timeouts.HealthCheck <= 0 {
		c.Timeouts.HealthCheck = 10000
	}
	if c.Timeouts.HealthInterval <= 0 {
		c.Timeouts.HealthInterval = 60000
	}

	if len(c.Bitrate.CodecPriority) == 0 {
		c.Bitrate.CodecPriority = append([]string(nil), defaultCodecPriority...)
	}
	if c.Proxies == nil {
		c.Proxies = []ProxyConfig{}
	}
	if c.Upstream == nil {
		c.Upstream = []UpstreamDef{}
	}
}

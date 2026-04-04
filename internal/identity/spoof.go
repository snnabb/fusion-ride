package identity

import (
	"strings"
)

// Spoofer 生成伪装的 Emby 客户端标识头。
type Spoofer struct {
	mode string

	// custom 模式字段
	userAgent   string
	client      string
	version     string
	deviceName  string
	deviceID    string
}

// Infuse 默认值
var infuseDefaults = map[string]string{
	"User-Agent":        "Infuse/7.8.2 (iPhone; iOS 18.1; Scale/3.00)",
	"X-Emby-Client":    "Infuse",
	"X-Emby-Device-Name": "iPhone",
	"X-Emby-Device-Id":   "F53801B1-261C-4C52-8A40-DCBD9E3E6E5C",
	"X-Emby-Client-Version": "7.8.2",
}

// NewSpoofer 创建 UA 伪装器。
// mode: "none" | "passthrough" | "infuse" | "custom"
func NewSpoofer(mode, ua, client, version, device, deviceID string) *Spoofer {
	if mode == "" {
		mode = "infuse"
	}
	return &Spoofer{
		mode:       mode,
		userAgent:  ua,
		client:     client,
		version:    version,
		deviceName: device,
		deviceID:   deviceID,
	}
}

// Headers 返回应该设置的请求头。
func (s *Spoofer) Headers() map[string]string {
	switch s.mode {
	case "none":
		return map[string]string{
			"User-Agent":             "FusionRide/1.0",
			"X-Emby-Client":         "FusionRide",
			"X-Emby-Device-Name":    "Server",
			"X-Emby-Device-Id":      "fusionride-default",
			"X-Emby-Client-Version": "1.0.0",
		}

	case "infuse":
		h := make(map[string]string, len(infuseDefaults))
		for k, v := range infuseDefaults {
			h[k] = v
		}
		return h

	case "custom":
		h := make(map[string]string, 5)
		if s.userAgent != "" {
			h["User-Agent"] = s.userAgent
		} else {
			h["User-Agent"] = infuseDefaults["User-Agent"]
		}
		if s.client != "" {
			h["X-Emby-Client"] = s.client
		} else {
			h["X-Emby-Client"] = "FusionRide"
		}
		if s.deviceName != "" {
			h["X-Emby-Device-Name"] = s.deviceName
		} else {
			h["X-Emby-Device-Name"] = "Server"
		}
		if s.deviceID != "" {
			h["X-Emby-Device-Id"] = s.deviceID
		} else {
			h["X-Emby-Device-Id"] = "fusionride-custom"
		}
		if s.version != "" {
			h["X-Emby-Client-Version"] = s.version
		} else {
			h["X-Emby-Client-Version"] = "1.0.0"
		}
		return h

	case "passthrough":
		// passthrough 模式在请求时动态处理，这里返回 Infuse 兜底
		h := make(map[string]string, len(infuseDefaults))
		for k, v := range infuseDefaults {
			h[k] = v
		}
		return h

	default:
		return map[string]string{
			"User-Agent": "FusionRide/1.0",
		}
	}
}

// Mode 返回当前伪装模式。
func (s *Spoofer) Mode() string {
	return s.mode
}

// ApplyPassthrough 用客户端的真实头覆盖（passthrough 模式）。
func (s *Spoofer) ApplyPassthrough(reqHeaders map[string]string) map[string]string {
	if s.mode != "passthrough" {
		return s.Headers()
	}

	h := s.Headers() // Infuse 兜底

	// 五级解析：优先使用请求中的真实头
	passthroughKeys := []string{
		"User-Agent",
		"X-Emby-Client",
		"X-Emby-Device-Name",
		"X-Emby-Device-Id",
		"X-Emby-Client-Version",
	}

	for _, key := range passthroughKeys {
		if v, ok := reqHeaders[key]; ok && v != "" {
			h[key] = v
		}
	}

	return h
}

// BuildAuthorizationHeader 构建 X-Emby-Authorization 头。
func (s *Spoofer) BuildAuthorizationHeader() string {
	h := s.Headers()
	parts := []string{
		`MediaBrowser Client="` + getOrDefault(h, "X-Emby-Client", "FusionRide") + `"`,
		`Device="` + getOrDefault(h, "X-Emby-Device-Name", "Server") + `"`,
		`DeviceId="` + getOrDefault(h, "X-Emby-Device-Id", "fusionride") + `"`,
		`Version="` + getOrDefault(h, "X-Emby-Client-Version", "1.0.0") + `"`,
	}
	return strings.Join(parts, ", ")
}

func getOrDefault(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

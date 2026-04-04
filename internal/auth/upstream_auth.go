package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UpstreamSession 保存上游服务器的认证회话。
type UpstreamSession struct {
	ServerID    int
	Token       string // 上游 Emby Token
	UserID      string // 上游 User ID
	AuthMethod  string // "password" | "apikey"
	AuthedAt    time.Time
	DisplayName string
}

// AuthenticateUpstream 向上游 Emby 服务器认证。
// 优先使用 apiKey，否则用 username/password 登录。
func AuthenticateUpstream(baseURL, username, password, apiKey string, headers map[string]string, timeout time.Duration) (*UpstreamSession, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	if apiKey != "" {
		return authenticateByAPIKey(baseURL, apiKey, timeout)
	}
	if username != "" && password != "" {
		return authenticateByName(baseURL, username, password, headers, timeout)
	}
	return nil, fmt.Errorf("无认证凭据：需要 apiKey 或 username+password")
}

func authenticateByAPIKey(baseURL, apiKey string, timeout time.Duration) (*UpstreamSession, error) {
	client := &http.Client{Timeout: timeout}

	// 验证 API Key 有效性
	req, err := http.NewRequest("GET", baseURL+"/System/Info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接上游失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API Key 验证失败: HTTP %d", resp.StatusCode)
	}

	return &UpstreamSession{
		Token:      apiKey,
		AuthMethod: "apikey",
		AuthedAt:   time.Now(),
	}, nil
}

func authenticateByName(baseURL, username, password string, headers map[string]string, timeout time.Duration) (*UpstreamSession, error) {
	client := &http.Client{Timeout: timeout}

	payload := map[string]string{
		"Username": username,
		"Pw":       password,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURL+"/Users/AuthenticateByName", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	// 设置 Emby 客户端标识头
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 必须有 Authorization 头
	if req.Header.Get("X-Emby-Authorization") == "" {
		authParts := []string{
			`MediaBrowser Client="FusionRide"`,
			`Device="Server"`,
			`DeviceId="fusionride-server"`,
			`Version="1.0.0"`,
		}
		req.Header.Set("X-Emby-Authorization", strings.Join(authParts, ", "))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接上游失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("登录失败: HTTP %d - %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"User"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析登录响应失败: %w", err)
	}

	if result.AccessToken == "" {
		return nil, fmt.Errorf("登录成功但未返回 Token")
	}

	return &UpstreamSession{
		Token:       result.AccessToken,
		UserID:      result.User.ID,
		AuthMethod:  "password",
		AuthedAt:    time.Now(),
		DisplayName: result.User.Name,
	}, nil
}

// CheckUpstreamHealth 检查上游是否在线。
func CheckUpstreamHealth(baseURL string, headers map[string]string, timeout time.Duration) (bool, string) {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: timeout}

	req, err := http.NewRequest("GET", baseURL+"/System/Info/Public", nil)
	if err != nil {
		return false, err.Error()
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var info struct {
			ServerName string `json:"ServerName"`
			Version    string `json:"Version"`
		}
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &info)
		return true, fmt.Sprintf("%s (v%s)", info.ServerName, info.Version)
	}

	return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

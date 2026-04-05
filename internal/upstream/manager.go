package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/snnabb/fusion-ride/internal/auth"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/identity"
	"github.com/snnabb/fusion-ride/internal/logger"
)

// Upstream represents a single Emby server.
type Upstream struct {
	Mu sync.RWMutex

	ID           int
	Name         string
	URL          string
	PlaybackMode string // "proxy" | "direct" | "redirect"
	StreamingURL string
	StreamHosts  []string
	SpoofMode    string
	Enabled      bool
	Priority     int
	PriorityMeta bool

	// Runtime
	Session       *auth.UpstreamSession
	HealthStatus  string // "online" | "offline" | "unknown"
	HealthMessage string
	LastCheck     time.Time

	// Credentials
	Username string
	Password string
	APIKey   string

	// UA Spoofer
	Spoofer *identity.Spoofer

	// HTTP Client
	Client *http.Client
}

// Manager manages all instances.
type Manager struct {
	mu        sync.RWMutex
	upstreams []*Upstream
	db        *db.DB
	log       *logger.Logger
	stopCh    chan struct{}
	globalPBM string
}

// NewManager creates a manager and loads from db.
func NewManager(database *db.DB, log *logger.Logger, globalPlaybackMode string) *Manager {
	m := &Manager{
		db:        database,
		log:       log,
		stopCh:    make(chan struct{}),
		globalPBM: normalizePlaybackMode(globalPlaybackMode),
	}
	m.loadFromDB()
	return m
}

func (m *Manager) All() []*Upstream {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Upstream, len(m.upstreams))
	copy(out, m.upstreams)
	return out
}

func (m *Manager) Online() []*Upstream {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []*Upstream
	for _, u := range m.upstreams {
		u.Mu.RLock()
		isOnline := u.Enabled && u.HealthStatus == "online" && u.Session != nil
		u.Mu.RUnlock()
		if isOnline {
			out = append(out, u)
		}
	}
	return out
}

func (m *Manager) ByID(id int) *Upstream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.upstreams {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (m *Manager) Add(name, urlValue, username, password, apiKey, playbackMode, spoofMode, streamingURL string, streamHosts []string) (int, error) {
	urlValue = strings.TrimRight(urlValue, "/")
	streamingURL = strings.TrimRight(streamingURL, "/")
	playbackMode = normalizePlaybackMode(playbackMode)
	spoofMode = identity.NormalizeMode(spoofMode)
	streamHosts = normalizeStreamHosts(streamHosts)

	streamHostsJSON, err := marshalStreamHosts(streamHosts)
	if err != nil {
		return 0, fmt.Errorf("序列化流媒体主机列表失败: %w", err)
	}

	result, err := m.db.Exec(
		`INSERT INTO upstreams(name, url, username, password, api_key, playback_mode, streaming_url, stream_hosts, spoof_mode)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, urlValue, username, password, apiKey, playbackMode, streamingURL, streamHostsJSON, spoofMode,
	)
	if err != nil {
		return 0, fmt.Errorf("保存上游失败: %w", err)
	}

	id, _ := result.LastInsertId()
	u := &Upstream{
		ID:           int(id),
		Name:         name,
		URL:          urlValue,
		Username:     username,
		Password:     password,
		APIKey:       apiKey,
		PlaybackMode: playbackMode,
		StreamingURL: streamingURL,
		StreamHosts:  append([]string(nil), streamHosts...),
		SpoofMode:    spoofMode,
		Enabled:      true,
		HealthStatus: "unknown",
		Spoofer:      identity.NewSpoofer(spoofMode, "", "", "", "", ""),
		Client:       &http.Client{Timeout: 30 * time.Second},
	}

	m.mu.Lock()
	m.upstreams = append(m.upstreams, u)
	m.mu.Unlock()

	m.log.Info("已添加上游 [%s] %s", name, urlValue)
	go m.authenticate(u)

	return int(id), nil
}

func (m *Manager) Remove(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i, u := range m.upstreams {
		if u.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("上游 %d 不存在", id)
	}

	name := m.upstreams[idx].Name
	if _, err := m.db.Exec(`DELETE FROM upstreams WHERE id = ?`, id); err != nil {
		return err
	}

	m.upstreams = append(m.upstreams[:idx], m.upstreams[idx+1:]...)
	m.log.Info("已删除上游 [%s]", name)
	return nil
}

func (m *Manager) Update(id int, fields map[string]any) error {
	u := m.ByID(id)
	if u == nil {
		return fmt.Errorf("上游 %d 不存在", id)
	}

	setClauses := make([]string, 0, len(fields))
	values := make([]any, 0, len(fields))
	normalized := make(map[string]any, len(fields))

	for key, value := range fields {
		switch key {
		case "name", "username", "password", "api_key", "streaming_url":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的字段 %s 类型错误", id, key)
			}
			if key == "streaming_url" {
				text = strings.TrimRight(text, "/")
			}
			normalized[key] = text
		case "url":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的 URL 类型错误", id)
			}
			normalized[key] = strings.TrimRight(text, "/")
		case "playback_mode":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的播放模式类型错误", id)
			}
			normalized[key] = normalizePlaybackMode(text)
		case "spoof_mode":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的 UA 伪装模式类型错误", id)
			}
			normalized[key] = identity.NormalizeMode(text)
		case "enabled", "priority_meta", "follow_redirects":
			flag, ok := value.(bool)
			if !ok {
				return fmt.Errorf("上游 %d 的字段 %s 类型错误", id, key)
			}
			normalized[key] = flag
		case "priority":
			number, ok := value.(int)
			if !ok {
				return fmt.Errorf("上游 %d 的优先级类型错误", id)
			}
			normalized[key] = number
		case "stream_hosts":
			hosts, err := normalizeStreamHostsValue(value)
			if err != nil {
				return fmt.Errorf("上游 %d 的字段 %s 类型错误", id, key)
			}
			streamHostsJSON, err := marshalStreamHosts(hosts)
			if err != nil {
				return fmt.Errorf("序列化上游 %d 的流媒体主机列表失败: %w", id, err)
			}
			normalized[key] = streamHostsJSON
			normalized["stream_hosts_runtime"] = hosts
		default:
			return fmt.Errorf("不支持的字段 %s", key)
		}
	}

	for key, value := range normalized {
		if key == "stream_hosts_runtime" {
			continue
		}
		setClauses = append(setClauses, key+" = ?")
		values = append(values, value)
	}

	if len(setClauses) == 0 {
		return nil
	}

	values = append(values, id)
	query := fmt.Sprintf("UPDATE upstreams SET %s, updated_at = unixepoch() WHERE id = ?", strings.Join(setClauses, ", "))
	if _, err := m.db.Exec(query, values...); err != nil {
		return err
	}

	u.Mu.Lock()
	shouldReconnect := false
	if value, ok := normalized["name"]; ok {
		u.Name = value.(string)
	}
	if value, ok := normalized["url"]; ok {
		u.URL = value.(string)
		shouldReconnect = true
	}
	if value, ok := normalized["playback_mode"]; ok {
		u.PlaybackMode = value.(string)
	}
	if value, ok := normalized["username"]; ok {
		u.Username = value.(string)
		shouldReconnect = true
	}
	if value, ok := normalized["password"]; ok {
		u.Password = value.(string)
		shouldReconnect = true
	}
	if value, ok := normalized["api_key"]; ok {
		u.APIKey = value.(string)
		shouldReconnect = true
	}
	if value, ok := normalized["spoof_mode"]; ok {
		u.SpoofMode = value.(string)
		u.Spoofer = identity.NewSpoofer(u.SpoofMode, "", "", "", "", "")
		shouldReconnect = true
	}
	if value, ok := normalized["streaming_url"]; ok {
		u.StreamingURL = value.(string)
	}
	if value, ok := normalized["stream_hosts_runtime"]; ok {
		u.StreamHosts = append([]string(nil), value.([]string)...)
	}
	if value, ok := normalized["priority"]; ok {
		u.Priority = value.(int)
	}
	if value, ok := normalized["priority_meta"]; ok {
		u.PriorityMeta = value.(bool)
	}
	if value, ok := normalized["enabled"]; ok {
		u.Enabled = value.(bool)
		if u.Enabled {
			shouldReconnect = true
		}
	}
	if shouldReconnect {
		u.Session = nil
		u.HealthStatus = "unknown"
		u.HealthMessage = "等待重新连接"
	}
	enabled := u.Enabled
	u.Mu.Unlock()

	if shouldReconnect && enabled {
		go m.authenticate(u)
	}

	return nil
}

func (m *Manager) Reconnect(id int) error {
	u := m.ByID(id)
	if u == nil {
		return fmt.Errorf("上游 %d 不存在", id)
	}
	go m.authenticate(u)
	return nil
}

func (m *Manager) Reorder(ids []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE upstreams SET priority = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	ordered := make([]*Upstream, 0, len(m.upstreams))
	for _, id := range ids {
		for _, u := range m.upstreams {
			if u.ID == id {
				u.Priority = len(ordered)
				ordered = append(ordered, u)
				break
			}
		}
	}
	m.upstreams = ordered
	return nil
}

func (m *Manager) StartHealthChecks(interval time.Duration, checkTimeout time.Duration) {
	m.checkAllHealth(checkTimeout)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.checkAllHealth(checkTimeout)
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}
}

func (m *Manager) loadFromDB() {
	rows, err := m.db.Query(
		`SELECT id, name, url, username, password, api_key, playback_mode, streaming_url,
		        stream_hosts, spoof_mode, custom_ua, custom_client, custom_version, custom_device,
		        custom_device_id, proxy_id, priority, priority_meta, follow_redirects,
		        enabled, health_status, session_token
		 FROM upstreams ORDER BY priority ASC`,
	)
	if err != nil {
		m.log.Error("加载上游列表失败: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var u Upstream
		var (
			streamHostsJSON                                     string
			customUA, customClient, customVersion, customDevice string
			customDeviceID, proxyID, sessionToken               string
			followRedirects                                     bool
		)

		err := rows.Scan(
			&u.ID, &u.Name, &u.URL, &u.Username, &u.Password, &u.APIKey,
			&u.PlaybackMode, &u.StreamingURL, &streamHostsJSON, &u.SpoofMode,
			&customUA, &customClient, &customVersion, &customDevice, &customDeviceID,
			&proxyID, &u.Priority, &u.PriorityMeta, &followRedirects,
			&u.Enabled, &u.HealthStatus, &sessionToken,
		)
		if err != nil {
			m.log.Error("读取上游记录失败: %v", err)
			continue
		}

		u.URL = strings.TrimRight(u.URL, "/")
		u.StreamingURL = strings.TrimRight(u.StreamingURL, "/")
		u.PlaybackMode = normalizePlaybackMode(u.PlaybackMode)
		u.StreamHosts = unmarshalStreamHosts(streamHostsJSON)
		u.SpoofMode = identity.NormalizeMode(u.SpoofMode)
		u.Spoofer = identity.NewSpoofer(u.SpoofMode, customUA, customClient, customVersion, customDevice, customDeviceID)
		u.Client = &http.Client{Timeout: 30 * time.Second}

		if sessionToken != "" {
			u.Session = &auth.UpstreamSession{
				ServerID:   u.ID,
				Token:      sessionToken,
				AuthMethod: "restored",
				AuthedAt:   time.Now(),
			}
		}

		m.upstreams = append(m.upstreams, &u)
		m.log.Info("已加载上游 [%s] %s，状态：%s", u.Name, u.URL, u.HealthStatus)
	}
}

func (m *Manager) authenticate(u *Upstream) {
	u.Mu.Lock()
	baseURL := u.URL
	username := u.Username
	password := u.Password
	apiKey := u.APIKey
	headers := u.Spoofer.Headers()
	name := u.Name
	u.Mu.Unlock()

	m.log.Info("正在认证上游 [%s]", name)

	session, err := auth.AuthenticateUpstream(baseURL, username, password, apiKey, headers, 10*time.Second)
	if err != nil {
		m.log.Error("上游 [%s] 认证失败: %v", name, err)
		u.Mu.Lock()
		u.HealthStatus = "offline"
		u.HealthMessage = err.Error()
		u.Mu.Unlock()
		return
	}

	session.ServerID = u.ID
	u.Mu.Lock()
	u.Session = session
	u.HealthStatus = "online"
	u.HealthMessage = "认证成功"
	u.Mu.Unlock()

	_, _ = m.db.Exec(`UPDATE upstreams SET session_token = ?, health_status = 'online' WHERE id = ?`, session.Token, u.ID)
	m.log.Info("上游 [%s] 认证成功", name)
}

func (m *Manager) checkAllHealth(timeout time.Duration) {
	upstreams := m.All()
	var wg sync.WaitGroup

	for _, u := range upstreams {
		if !u.Enabled {
			continue
		}

		wg.Add(1)
		go func(u *Upstream) {
			defer wg.Done()

			u.Mu.RLock()
			headers := u.Spoofer.Headers()
			prevStatus := u.HealthStatus
			u.Mu.RUnlock()

			online, msg := auth.CheckUpstreamHealth(u.URL, headers, timeout)

			u.Mu.Lock()
			u.LastCheck = time.Now()
			if online {
				u.HealthStatus = "online"
				u.HealthMessage = msg
			} else {
				u.HealthStatus = "offline"
				u.HealthMessage = msg
			}
			newStatus := u.HealthStatus
			u.Mu.Unlock()

			if prevStatus != newStatus {
				if newStatus == "online" {
					m.log.Info("健康检查 [%s] 状态变更：在线，%s", u.Name, msg)
					if u.Session == nil {
						go m.authenticate(u)
					}
				} else {
					m.log.Warn("健康检查 [%s] 状态变更：离线，%s", u.Name, msg)
				}
			}

			_, _ = m.db.Exec(`UPDATE upstreams SET health_status = ?, last_check = ? WHERE id = ?`, newStatus, u.LastCheck.Unix(), u.ID)
		}(u)
	}

	wg.Wait()
}

func (u *Upstream) DoAPI(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u.Mu.RLock()
	baseURL := u.URL
	u.Mu.RUnlock()

	return u.doWithHeaders(ctx, baseURL, method, path, body, nil, nil)
}

func (u *Upstream) DoAPIWithHeaders(ctx context.Context, method, path string, body io.Reader, incoming http.Header) (*http.Response, error) {
	u.Mu.RLock()
	baseURL := u.URL
	u.Mu.RUnlock()

	return u.doWithHeaders(ctx, baseURL, method, path, body, incoming, nil)
}

func (u *Upstream) DoPlaybackWithHeaders(ctx context.Context, method, path string, body io.Reader, incoming http.Header, clientOverride *http.Client) (*http.Response, error) {
	return u.doWithHeaders(ctx, u.PlaybackBaseURL(), method, path, body, incoming, clientOverride)
}

func (u *Upstream) doWithHeaders(ctx context.Context, baseURL, method, path string, body io.Reader, incoming http.Header, clientOverride *http.Client) (*http.Response, error) {
	u.Mu.RLock()
	name := u.Name
	session := u.Session
	spoofer := u.Spoofer
	client := u.Client
	u.Mu.RUnlock()

	if session == nil {
		return nil, fmt.Errorf("上游 [%s] 尚未认证", name)
	}
	if spoofer == nil {
		spoofer = identity.NewSpoofer("infuse", "", "", "", "", "")
	}
	if clientOverride != nil {
		client = clientOverride
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}

	copyForwardHeaders(req.Header, incoming)
	req.Header.Set("X-Emby-Token", session.Token)
	if incoming != nil {
		if authHeader := incoming.Get("Authorization"); authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		if embyAuth := incoming.Get("X-Emby-Authorization"); embyAuth != "" {
			req.Header.Set("X-Emby-Authorization", embyAuth)
		}
	}
	spoofer.ApplyToHeader(req.Header)

	return client.Do(req)
}

func (u *Upstream) DoAPIJSON(ctx context.Context, method, path string, body io.Reader, result any) error {
	resp, err := u.DoAPI(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, string(payload))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func normalizePlaybackMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "inherit":
		return ""
	case "proxy":
		return "proxy"
	case "direct", "redirect":
		return "direct"
	case "redirect-follow":
		return "redirect"
	default:
		return "proxy"
	}
}

func copyForwardHeaders(dst http.Header, src http.Header) {
	if src == nil {
		return
	}

	skipped := map[string]struct{}{
		"Authorization":         {},
		"Connection":            {},
		"Content-Length":        {},
		"Host":                  {},
		"Keep-Alive":            {},
		"Proxy-Connection":      {},
		"Te":                    {},
		"Trailer":               {},
		"Transfer-Encoding":     {},
		"Upgrade":               {},
		"User-Agent":            {},
		"X-Emby-Authorization":  {},
		"X-Emby-Client":         {},
		"X-Emby-Client-Version": {},
		"X-Emby-Device-Id":      {},
		"X-Emby-Device-Name":    {},
		"X-Emby-Token":          {},
	}

	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, skip := skipped[canonical]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (u *Upstream) EffectivePlaybackMode(globalMode string) string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()
	if u.PlaybackMode != "" {
		return normalizePlaybackMode(u.PlaybackMode)
	}
	return normalizePlaybackMode(globalMode)
}

func (u *Upstream) GetUserID() string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()
	if u.Session == nil {
		return ""
	}
	return u.Session.UserID
}

func (u *Upstream) SetUserID(userID string) {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if u.Session == nil {
		return
	}
	u.Session.UserID = strings.TrimSpace(userID)
}

func (u *Upstream) PlaybackBaseURL() string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()
	return u.playbackBaseURLLocked()
}

func (u *Upstream) playbackBaseURLLocked() string {
	base := strings.TrimRight(u.URL, "/")
	if u.StreamingURL != "" {
		base = strings.TrimRight(u.StreamingURL, "/")
	}
	return base
}

func (u *Upstream) BuildStreamURL(originalID string, query string) string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()

	streamURL := fmt.Sprintf("%s/Videos/%s/stream", u.playbackBaseURLLocked(), originalID)
	if query != "" {
		streamURL += "?" + query
	}
	if u.Session != nil {
		if query != "" {
			streamURL += "&"
		} else {
			streamURL += "?"
		}
		streamURL += "api_key=" + u.Session.Token
	}
	return streamURL
}

func (u *Upstream) AllowedStreamHosts() map[string]bool {
	u.Mu.RLock()
	defer u.Mu.RUnlock()

	allowed := make(map[string]bool)
	addAllowedHost(allowed, u.URL)
	addAllowedHost(allowed, u.StreamingURL)
	for _, host := range u.StreamHosts {
		addAllowedHost(allowed, host)
	}
	return allowed
}

func normalizeStreamHostsValue(value any) ([]string, error) {
	switch typed := value.(type) {
	case []string:
		return normalizeStreamHosts(typed), nil
	case []any:
		hosts := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("stream_hosts contains non-string value")
			}
			hosts = append(hosts, text)
		}
		return normalizeStreamHosts(hosts), nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, nil
		}
		return normalizeStreamHosts([]string{typed}), nil
	default:
		return nil, fmt.Errorf("unsupported stream_hosts type %T", value)
	}
}

func normalizeStreamHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		host = strings.TrimPrefix(host, "http://")
		host = strings.TrimPrefix(host, "https://")
		host = strings.Trim(host, "/")
		if host == "" {
			continue
		}
		if parsed, err := url.Parse("https://" + host); err == nil && parsed.Host != "" {
			host = strings.ToLower(parsed.Host)
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

func marshalStreamHosts(hosts []string) (string, error) {
	normalized := normalizeStreamHosts(hosts)
	if normalized == nil {
		normalized = []string{}
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalStreamHosts(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return nil
	}
	return normalizeStreamHosts(hosts)
}

func addAllowedHost(allowed map[string]bool, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}

	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return
	}

	host := strings.ToLower(parsed.Host)
	if host != "" {
		allowed[host] = true
	}
	if hostname := strings.ToLower(parsed.Hostname()); hostname != "" {
		allowed[hostname] = true
	}
}

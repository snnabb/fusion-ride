package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	PlaybackMode string // "proxy" | "redirect"
	StreamingURL string
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
		globalPBM: globalPlaybackMode,
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

func (m *Manager) Add(name, url, username, password, apiKey, playbackMode, spoofMode string) (int, error) {
	url = strings.TrimRight(url, "/")
	playbackMode = normalizePlaybackMode(playbackMode)
	spoofMode = identity.NormalizeMode(spoofMode)

	result, err := m.db.Exec(
		`INSERT INTO upstreams(name, url, username, password, api_key, playback_mode, spoof_mode)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		name, url, username, password, apiKey, playbackMode, spoofMode,
	)
	if err != nil {
		return 0, fmt.Errorf("保存上游失败: %w", err)
	}

	id, _ := result.LastInsertId()
	u := &Upstream{
		ID:           int(id),
		Name:         name,
		URL:          url,
		Username:     username,
		Password:     password,
		APIKey:       apiKey,
		PlaybackMode: playbackMode,
		SpoofMode:    spoofMode,
		Enabled:      true,
		HealthStatus: "unknown",
		Spoofer:      identity.NewSpoofer(spoofMode, "", "", "", "", ""),
		Client:       &http.Client{Timeout: 30 * time.Second},
	}

	m.mu.Lock()
	m.upstreams = append(m.upstreams, u)
	m.mu.Unlock()

	m.log.Info("已添加上游 [%s] %s", name, url)

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
	for k, v := range fields {
		switch k {
		case "name", "username", "password", "api_key", "streaming_url":
			value, ok := v.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的字段 %s 类型错误", id, k)
			}
			normalized[k] = value
		case "url":
			value, ok := v.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的 URL 类型错误", id)
			}
			normalized[k] = strings.TrimRight(value, "/")
		case "playback_mode":
			value, ok := v.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的播放模式类型错误", id)
			}
			normalized[k] = normalizePlaybackMode(value)
		case "spoof_mode":
			value, ok := v.(string)
			if !ok {
				return fmt.Errorf("上游 %d 的 UA 伪装模式类型错误", id)
			}
			normalized[k] = identity.NormalizeMode(value)
		case "enabled", "priority_meta", "follow_redirects":
			value, ok := v.(bool)
			if !ok {
				return fmt.Errorf("上游 %d 的字段 %s 类型错误", id, k)
			}
			normalized[k] = value
		case "priority":
			value, ok := v.(int)
			if !ok {
				return fmt.Errorf("上游 %d 的优先级类型错误", id)
			}
			normalized[k] = value
		default:
			return fmt.Errorf("不支持的字段 %s", k)
		}
	}
	for k, v := range normalized {
		setClauses = append(setClauses, k+" = ?")
		values = append(values, v)
	}
	if len(setClauses) == 0 {
		return nil
	}
	values = append(values, id)

	query := fmt.Sprintf("UPDATE upstreams SET %s, updated_at = unixepoch() WHERE id = ?",
		strings.Join(setClauses, ", "))

	if _, err := m.db.Exec(query, values...); err != nil {
		return err
	}

	u.Mu.Lock()
	shouldReconnect := false
	if v, ok := normalized["name"]; ok {
		u.Name = v.(string)
	}
	if v, ok := normalized["url"]; ok {
		u.URL = strings.TrimRight(v.(string), "/")
		shouldReconnect = true
	}
	if v, ok := normalized["playback_mode"]; ok {
		u.PlaybackMode = v.(string)
	}
	if v, ok := normalized["username"]; ok {
		u.Username = v.(string)
		shouldReconnect = true
	}
	if v, ok := normalized["password"]; ok {
		u.Password = v.(string)
		shouldReconnect = true
	}
	if v, ok := normalized["api_key"]; ok {
		u.APIKey = v.(string)
		shouldReconnect = true
	}
	if v, ok := normalized["spoof_mode"]; ok {
		u.SpoofMode = v.(string)
		u.Spoofer = identity.NewSpoofer(u.SpoofMode, "", "", "", "", "")
		shouldReconnect = true
	}
	if v, ok := normalized["streaming_url"]; ok {
		u.StreamingURL = strings.TrimRight(v.(string), "/")
	}
	if v, ok := normalized["priority"]; ok {
		u.Priority = v.(int)
	}
	if v, ok := normalized["priority_meta"]; ok {
		u.PriorityMeta = v.(bool)
	}
	if v, ok := normalized["enabled"]; ok {
		u.Enabled = v.(bool)
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
		        spoof_mode, custom_ua, custom_client, custom_version, custom_device,
		        custom_device_id, proxy_id, priority, priority_meta, follow_redirects,
		        enabled, health_status, session_token
		 FROM upstreams ORDER BY priority ASC`)
	if err != nil {
		m.log.Error("加载上游列表失败: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var u Upstream
		var customUA, customClient, customVer, customDev, customDevID, proxyID, sessionToken string
		var followRedirects bool

		err := rows.Scan(
			&u.ID, &u.Name, &u.URL, &u.Username, &u.Password, &u.APIKey,
			&u.PlaybackMode, &u.StreamingURL, &u.SpoofMode,
			&customUA, &customClient, &customVer, &customDev, &customDevID,
			&proxyID, &u.Priority, &u.PriorityMeta, &followRedirects,
			&u.Enabled, &u.HealthStatus, &sessionToken,
		)
		if err != nil {
			m.log.Error("读取上游记录失败: %v", err)
			continue
		}

		u.URL = strings.TrimRight(u.URL, "/")
		u.PlaybackMode = normalizePlaybackMode(u.PlaybackMode)
		u.SpoofMode = identity.NormalizeMode(u.SpoofMode)
		u.Spoofer = identity.NewSpoofer(u.SpoofMode, customUA, customClient, customVer, customDev, customDevID)
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
	u.Mu.Unlock()

	m.log.Info("正在认证上游 [%s]", u.Name)

	session, err := auth.AuthenticateUpstream(
		baseURL, username, password, apiKey, headers,
		10*time.Second,
	)
	if err != nil {
		m.log.Error("上游 [%s] 认证失败: %v", u.Name, err)
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

	m.db.Exec(`UPDATE upstreams SET session_token = ?, health_status = 'online' WHERE id = ?`,
		session.Token, u.ID)

	m.log.Info("上游 [%s] 认证成功", u.Name)
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

			m.db.Exec(`UPDATE upstreams SET health_status = ?, last_check = ? WHERE id = ?`,
				newStatus, u.LastCheck.Unix(), u.ID)
		}(u)
	}

	wg.Wait()
}

func (u *Upstream) DoAPI(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u.Mu.RLock()
	baseURL := u.URL
	session := u.Session
	headers := u.Spoofer.Headers()
	u.Mu.RUnlock()

	if session == nil {
		return nil, fmt.Errorf("上游 [%s] 尚未认证", u.Name)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	url := baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Emby-Token", session.Token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return u.Client.Do(req)
}

func (u *Upstream) DoAPIWithHeaders(ctx context.Context, method, path string, body io.Reader, incoming http.Header) (*http.Response, error) {
	u.Mu.RLock()
	baseURL := u.URL
	session := u.Session
	spoofer := u.Spoofer
	client := u.Client
	u.Mu.RUnlock()

	if session == nil {
		return nil, fmt.Errorf("上游 [%s] 尚未认证", u.Name)
	}
	if spoofer == nil {
		spoofer = identity.NewSpoofer("infuse", "", "", "", "", "")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	url := baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	copyForwardHeaders(req.Header, incoming)
	if incoming != nil {
		if token := strings.TrimSpace(incoming.Get("X-Emby-Token")); token != "" {
			req.Header.Set("X-Emby-Token", token)
		} else {
			req.Header.Set("X-Emby-Token", session.Token)
		}
	} else {
		req.Header.Set("X-Emby-Token", session.Token)
	}
	if incoming != nil {
		if auth := incoming.Get("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if auth := incoming.Get("X-Emby-Authorization"); auth != "" {
			req.Header.Set("X-Emby-Authorization", auth)
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

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, string(b))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func normalizePlaybackMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "inherit":
		return ""
	case "redirect":
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
		return u.PlaybackMode
	}
	return globalMode
}

func (u *Upstream) GetUserID() string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()
	if u.Session == nil {
		return ""
	}
	return u.Session.UserID
}

func (u *Upstream) BuildStreamURL(originalID string, query string) string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()

	base := u.URL
	if u.StreamingURL != "" {
		base = strings.TrimRight(u.StreamingURL, "/")
	}

	streamURL := fmt.Sprintf("%s/Videos/%s/stream?%s", base, originalID, query)

	if u.Session != nil {
		if query != "" {
			streamURL += "&"
		}
		streamURL += "api_key=" + u.Session.Token
	}

	return streamURL
}

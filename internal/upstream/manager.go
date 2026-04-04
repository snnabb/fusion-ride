package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fusionride/fusion-ride/internal/auth"
	"github.com/fusionride/fusion-ride/internal/db"
	"github.com/fusionride/fusion-ride/internal/identity"
	"github.com/fusionride/fusion-ride/internal/logger"
)

// Upstream represents a single Emby server.
type Upstream struct {
	Mu sync.RWMutex

	ID             int
	Name           string
	URL            string
	PlaybackMode   string // "proxy" | "redirect"
	StreamingURL   string
	SpoofMode      string
	Enabled        bool
	Priority       int
	PriorityMeta   bool

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
	if playbackMode == "" {
		playbackMode = m.globalPBM
	}
	if spoofMode == "" {
		spoofMode = "infuse"
	}

	result, err := m.db.Exec(
		`INSERT INTO upstreams(name, url, username, password, api_key, playback_mode, spoof_mode)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		name, url, username, password, apiKey, playbackMode, spoofMode,
	)
	if err != nil {
		return 0, fmt.Errorf("Save failed: %w", err)
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

	m.log.Info("Add upstream [%s] %s", name, url)

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
		return fmt.Errorf("Upstream %d does not exist", id)
	}

	name := m.upstreams[idx].Name

	if _, err := m.db.Exec(`DELETE FROM upstreams WHERE id = ?`, id); err != nil {
		return err
	}

	m.upstreams = append(m.upstreams[:idx], m.upstreams[idx+1:]...)
	m.log.Info("Removed upstream [%s]", name)
	return nil
}

func (m *Manager) Update(id int, fields map[string]any) error {
	u := m.ByID(id)
	if u == nil {
		return fmt.Errorf("Upstream %d does not exist", id)
	}

	setClauses := make([]string, 0, len(fields))
	values := make([]any, 0, len(fields))
	for k, v := range fields {
		setClauses = append(setClauses, k+" = ?")
		values = append(values, v)
	}
	values = append(values, id)

	query := fmt.Sprintf("UPDATE upstreams SET %s, updated_at = unixepoch() WHERE id = ?",
		strings.Join(setClauses, ", "))

	if _, err := m.db.Exec(query, values...); err != nil {
		return err
	}

	u.Mu.Lock()
	if v, ok := fields["name"]; ok {
		u.Name = v.(string)
	}
	if v, ok := fields["url"]; ok {
		u.URL = strings.TrimRight(v.(string), "/")
	}
	if v, ok := fields["playback_mode"]; ok {
		u.PlaybackMode = v.(string)
	}
	if v, ok := fields["enabled"]; ok {
		u.Enabled = v.(bool)
	}
	u.Mu.Unlock()

	return nil
}

func (m *Manager) Reconnect(id int) error {
	u := m.ByID(id)
	if u == nil {
		return fmt.Errorf("Upstream %d does not exist", id)
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
	close(m.stopCh)
}

func (m *Manager) loadFromDB() {
	rows, err := m.db.Query(
		`SELECT id, name, url, username, password, api_key, playback_mode, streaming_url,
		        spoof_mode, custom_ua, custom_client, custom_version, custom_device,
		        custom_device_id, proxy_id, priority, priority_meta, follow_redirects,
		        enabled, health_status, session_token
		 FROM upstreams ORDER BY priority ASC`)
	if err != nil {
		m.log.Error("Load upstreams failed: %v", err)
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
			m.log.Error("Read row failed: %v", err)
			continue
		}

		u.URL = strings.TrimRight(u.URL, "/")
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
		m.log.Info("Loaded upstream [%s] %s (Status: %s)", u.Name, u.URL, u.HealthStatus)
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

	m.log.Info("Authenticating upstream [%s] ...", u.Name)

	session, err := auth.AuthenticateUpstream(
		baseURL, username, password, apiKey, headers,
		10*time.Second,
	)
	if err != nil {
		m.log.Error("Upstream [%s] auth failed: %v", u.Name, err)
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
	u.HealthMessage = "Authenticated"
	u.Mu.Unlock()

	m.db.Exec(`UPDATE upstreams SET session_token = ?, health_status = 'online' WHERE id = ?`,
		session.Token, u.ID)

	m.log.Info("Upstream [%s] auth success", u.Name)
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
					m.log.Info("Upstream [%s] OFFLINE -> ONLINE: %s", u.Name, msg)
					if u.Session == nil {
						go m.authenticate(u)
					}
				} else {
					m.log.Warn("Upstream [%s] ONLINE -> OFFLINE: %s", u.Name, msg)
				}
			}

			m.db.Exec(`UPDATE upstreams SET health_status = ?, last_check = ? WHERE id = ?`,
				newStatus, u.LastCheck.Unix(), u.ID)
		}(u)
	}

	wg.Wait()
}

func (u *Upstream) DoAPI(method, path string, body io.Reader) (*http.Response, error) {
	u.Mu.RLock()
	baseURL := u.URL
	session := u.Session
	headers := u.Spoofer.Headers()
	u.Mu.RUnlock()

	if session == nil {
		return nil, fmt.Errorf("Upstream [%s] not authenticated", u.Name)
	}

	url := baseURL + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Emby-Token", session.Token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return u.Client.Do(req)
}

func (u *Upstream) DoAPIJSON(method, path string, body io.Reader, result any) error {
	resp, err := u.DoAPI(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func (u *Upstream) EffectivePlaybackMode(globalMode string) string {
	u.Mu.RLock()
	defer u.Mu.RUnlock()
	if u.PlaybackMode != "" {
		return u.PlaybackMode
	}
	return globalMode
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

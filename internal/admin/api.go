package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fusionride/fusion-ride/internal/auth"
	"github.com/fusionride/fusion-ride/internal/config"
	"github.com/fusionride/fusion-ride/internal/idmap"
	"github.com/fusionride/fusion-ride/internal/logger"
	"github.com/fusionride/fusion-ride/internal/traffic"
	"github.com/fusionride/fusion-ride/internal/upstream"
)

// API is the admin panel REST API handler.
type API struct {
	cfg       *config.Config
	cfgPath   string
	adminAuth *auth.AdminAuth
	upMgr     *upstream.Manager
	ids       *idmap.Store
	log       *logger.Logger
	meter     *traffic.Meter
	sseHub    *SSEHub
	startTime time.Time
}

// NewAPI creates an admin API instance.
func NewAPI(cfg *config.Config, cfgPath string, adminAuth *auth.AdminAuth,
	upMgr *upstream.Manager, ids *idmap.Store, log *logger.Logger,
	meter *traffic.Meter) *API {

	return &API{
		cfg:       cfg,
		cfgPath:   cfgPath,
		adminAuth: adminAuth,
		upMgr:     upMgr,
		ids:       ids,
		log:       log,
		meter:     meter,
		sseHub:    NewSSEHub(),
		startTime: time.Now(),
	}
}

// Handler returns the admin API HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/admin/api/setup", a.handleSetup)
	mux.HandleFunc("/admin/api/login", a.handleLogin)
	mux.HandleFunc("/admin/api/needs-setup", a.handleNeedsSetup)

	mux.HandleFunc("/admin/api/logout", a.requireAuth(a.handleLogout))
	mux.HandleFunc("/admin/api/status", a.requireAuth(a.handleStatus))
	mux.HandleFunc("/admin/api/upstreams", a.requireAuth(a.handleUpstreams))
	mux.HandleFunc("/admin/api/upstreams/", a.requireAuth(a.handleUpstreamByID))
	mux.HandleFunc("/admin/api/upstreams/reorder", a.requireAuth(a.handleReorder))
	mux.HandleFunc("/admin/api/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("/admin/api/traffic", a.requireAuth(a.handleTraffic))
	mux.HandleFunc("/admin/api/traffic/stream", a.requireAuth(a.handleTrafficStream))
	mux.HandleFunc("/admin/api/logs", a.requireAuth(a.handleLogs))
	mux.HandleFunc("/admin/api/logs/download", a.requireAuth(a.handleLogDownload))
	mux.HandleFunc("/admin/api/diagnostics", a.requireAuth(a.handleDiagnostics))
	mux.HandleFunc("/admin/api/password", a.requireAuth(a.handleChangePassword))

	return mux
}

func (a *API) requireAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			jsonError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := a.adminAuth.VerifyToken(token); err != nil {
			jsonError(w, "Token invalid or expired", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

func (a *API) handleNeedsSetup(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, map[string]bool{"needsSetup": a.adminAuth.NeedsSetup()})
}

func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.adminAuth.NeedsSetup() {
		jsonError(w, "Already set up", http.StatusConflict)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Bad request", http.StatusBadRequest)
		return
	}

	if err := a.adminAuth.Setup(req.Username, req.Password); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	token, err := a.adminAuth.Login(req.Username, req.Password)
	if err != nil {
		jsonError(w, "Setup ok but login failed", http.StatusInternalServerError)
		return
	}

	a.log.Info("Admin initial setup complete: %s", req.Username)
	jsonResp(w, map[string]string{"token": token})
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Bad request", http.StatusBadRequest)
		return
	}

	token, err := a.adminAuth.Login(req.Username, req.Password)
	if err != nil {
		a.log.Warn("Admin login failed: %s", req.Username)
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	a.log.Info("Admin login: %s", req.Username)
	jsonResp(w, map[string]string{"token": token})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, map[string]string{"status": "ok"})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	upstreams := a.upMgr.All()
	upstreamStats := make([]map[string]any, 0, len(upstreams))

	totalOnline := 0
	for _, u := range upstreams {
		u.Mu.RLock()
		status := map[string]any{
			"id":            u.ID,
			"name":          u.Name,
			"url":           u.URL,
			"enabled":       u.Enabled,
			"healthStatus":  u.HealthStatus,
			"healthMessage": u.HealthMessage,
			"playbackMode":  u.PlaybackMode,
			"spoofMode":     u.SpoofMode,
			"lastCheck":     u.LastCheck.Unix(),
		}
		if u.HealthStatus == "online" {
			totalOnline++
		}
		u.Mu.RUnlock()
		upstreamStats = append(upstreamStats, status)
	}

	idTotal, idByServer := a.ids.Stats()

	jsonResp(w, map[string]any{
		"serverName":   a.cfg.Server.Name,
		"version":      "1.0.0",
		"port":         a.cfg.Server.Port,
		"uptime":       int(time.Since(a.startTime).Seconds()),
		"upstreams":    upstreamStats,
		"totalOnline":  totalOnline,
		"totalServers": len(upstreams),
		"idMappings":   idTotal,
		"idByServer":   idByServer,
		"playbackMode": a.cfg.Playback.Mode,
	})
}

func (a *API) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		upstreams := a.upMgr.All()
		result := make([]map[string]any, 0, len(upstreams))
		for _, u := range upstreams {
			u.Mu.RLock()
			result = append(result, map[string]any{
				"id":           u.ID,
				"name":         u.Name,
				"url":          u.URL,
				"enabled":      u.Enabled,
				"healthStatus": u.HealthStatus,
				"playbackMode": u.PlaybackMode,
				"spoofMode":    u.SpoofMode,
				"priority":     u.Priority,
				"priorityMeta": u.PriorityMeta,
			})
			u.Mu.RUnlock()
		}
		jsonResp(w, result)

	case "POST":
		var req struct {
			Name         string `json:"name"`
			URL          string `json:"url"`
			Username     string `json:"username"`
			Password     string `json:"password"`
			APIKey       string `json:"apiKey"`
			PlaybackMode string `json:"playbackMode"`
			SpoofMode    string `json:"spoofMode"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "Bad request", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.URL == "" {
			jsonError(w, "Name and URL are required", http.StatusBadRequest)
			return
		}

		id, err := a.upMgr.Add(req.Name, req.URL, req.Username, req.Password, req.APIKey, req.PlaybackMode, req.SpoofMode)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		a.sseHub.Broadcast("upstream_added", map[string]any{"id": id, "name": req.Name})
		jsonResp(w, map[string]any{"id": id, "status": "ok"})

	default:
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) handleUpstreamByID(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/api/upstreams/"), "/")
	if len(pathParts) == 0 {
		jsonError(w, "Missing ID", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(pathParts[0])
	if err != nil {
		jsonError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	action := ""
	if len(pathParts) > 1 {
		action = pathParts[1]
	}

	switch action {
	case "reconnect":
		if r.Method != "POST" {
			jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := a.upMgr.Reconnect(id); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonResp(w, map[string]string{"status": "reconnecting"})

	case "test":
		if r.Method != "POST" {
			jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		u := a.upMgr.ByID(id)
		if u == nil {
			jsonError(w, "Upstream not found", http.StatusNotFound)
			return
		}
		online, msg := auth.CheckUpstreamHealth(u.URL, u.Spoofer.Headers(), 10*time.Second)
		jsonResp(w, map[string]any{"online": online, "message": msg})

	default:
		switch r.Method {
		case "PUT":
			var req map[string]any
			if err := readJSON(r, &req); err != nil {
				jsonError(w, "Bad request", http.StatusBadRequest)
				return
			}
			if err := a.upMgr.Update(id, req); err != nil {
				jsonError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			a.sseHub.Broadcast("upstream_updated", map[string]any{"id": id})
			jsonResp(w, map[string]string{"status": "ok"})

		case "DELETE":
			if err := a.upMgr.Remove(id); err != nil {
				jsonError(w, err.Error(), http.StatusNotFound)
				return
			}
			a.ids.CleanupServer(id)
			a.sseHub.Broadcast("upstream_removed", map[string]any{"id": id})
			jsonResp(w, map[string]string{"status": "ok"})

		default:
			jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (a *API) handleReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Bad request", http.StatusBadRequest)
		return
	}
	if err := a.upMgr.Reorder(req.IDs); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, map[string]string{"status": "ok"})
}

func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		snap := a.cfg.Snapshot()
		jsonResp(w, map[string]any{
			"serverName":   snap.Server.Name,
			"port":         snap.Server.Port,
			"playbackMode": snap.Playback.Mode,
			"timeouts":     snap.Timeouts,
			"bitrate":      snap.Bitrate,
		})

	case "PUT":
		var req map[string]any
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "Bad request", http.StatusBadRequest)
			return
		}

		a.cfg.UpdateFunc(func(cfg *config.Config) {
			if v, ok := req["serverName"].(string); ok {
				cfg.Server.Name = v
			}
			if v, ok := req["playbackMode"].(string); ok {
				cfg.Playback.Mode = v
			}
		})

		if err := a.cfg.Save(a.cfgPath); err != nil {
			a.log.Error("Save config failed: %v", err)
		}

		jsonResp(w, map[string]string{"status": "ok"})

	default:
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) handleTraffic(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if m := r.URL.Query().Get("minutes"); m != "" {
		if v, err := strconv.Atoi(m); err == nil {
			minutes = v
		}
	}

	recent, _ := a.meter.RecentStats(minutes)
	total, _ := a.meter.TotalStats()

	jsonResp(w, map[string]any{
		"recent":  recent,
		"total":   total,
		"current": a.meter.Snapshots(),
	})
}

func (a *API) handleTrafficStream(w http.ResponseWriter, r *http.Request) {
	a.sseHub.ServeHTTP(w, r)
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		limit := 500
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil {
				limit = v
			}
		}
		entries := a.log.Recent(limit)
		jsonResp(w, entries)

	case "DELETE":
		if err := a.log.Clear(); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResp(w, map[string]string{"status": "ok"})

	default:
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	data, err := a.log.Content()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", "attachment; filename=fusionride.log")
	w.Write(data)
}

func (a *API) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	upstreams := a.upMgr.All()
	results := make([]map[string]any, 0, len(upstreams))

	for _, u := range upstreams {
		start := time.Now()
		online, msg := auth.CheckUpstreamHealth(u.URL, u.Spoofer.Headers(), 10*time.Second)
		latency := time.Since(start).Milliseconds()

		results = append(results, map[string]any{
			"id":        u.ID,
			"name":      u.Name,
			"url":       u.URL,
			"online":    online,
			"message":   msg,
			"latency":   latency,
			"spoofMode": u.SpoofMode,
		})
	}

	jsonResp(w, map[string]any{
		"upstreams": results,
		"timestamp": time.Now().Unix(),
	})
}

func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Bad request", http.StatusBadRequest)
		return
	}

	if err := a.adminAuth.ChangePassword(req.OldPassword, req.NewPassword); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	a.log.Info("Admin password changed")
	jsonResp(w, map[string]string{"status": "ok"})
}

// SSEHub manages Server-Sent Events connections.
type SSEHub struct {
	mu      sync.RWMutex
	clients map[chan string]struct{}
}

// NewSSEHub creates a new SSE hub.
func NewSSEHub() *SSEHub {
	return &SSEHub{
		clients: make(map[chan string]struct{}),
	}
}

func (h *SSEHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// Broadcast sends an event to all connected SSE clients.
func (h *SSEHub) Broadcast(event string, data any) {
	payload, _ := json.Marshal(map[string]any{
		"event": event,
		"data":  data,
		"time":  time.Now().Unix(),
	})

	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- string(payload):
		default:
		}
	}
}

// StartTrafficBroadcast periodically broadcasts traffic data via SSE.
func (a *API) StartTrafficBroadcast(meter *traffic.Meter, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			snapshots := meter.Snapshots()
			a.sseHub.Broadcast("traffic", snapshots)
		}
	}()
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.Unmarshal(body, v)
}

func jsonResp(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func extractBearerToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if strings.HasPrefix(a, "Bearer ") {
		return a[7:]
	}
	return r.URL.Query().Get("token")
}

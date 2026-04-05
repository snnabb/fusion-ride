package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/snnabb/fusion-ride/internal/aggregator"
	"github.com/snnabb/fusion-ride/internal/config"
	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/traffic"
	"github.com/snnabb/fusion-ride/internal/upstream"
)

type Handler struct {
	cfg   *config.Config
	upMgr *upstream.Manager
	agg   *aggregator.Aggregator
	ids   *idmap.Store
	log   *logger.Logger
	meter *traffic.Meter

	sessionMu sync.RWMutex
	sessions  map[string]loginSession
}

func NewHandler(cfg *config.Config, upMgr *upstream.Manager, agg *aggregator.Aggregator, ids *idmap.Store, log *logger.Logger, meter *traffic.Meter) *Handler {
	return &Handler{
		cfg:      cfg,
		upMgr:    upMgr,
		agg:      agg,
		ids:      ids,
		log:      log,
		meter:    meter,
		sessions: make(map[string]loginSession),
	}
}

type loginSession struct {
	UpstreamID     int
	UpstreamUserID string
	VirtualUserID  string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if isWebSocket(r) {
		h.handleWebSocket(w, r)
		return
	}
	if strings.HasSuffix(path, "/AuthenticateByName") {
		h.handleLogin(w, r)
		return
	}
	if path == "/System/Info/Public" || path == "/emby/System/Info/Public" {
		h.handleSystemInfo(w, r)
		return
	}
	if path == "/Users/Me" || path == "/emby/Users/Me" {
		h.handleCurrentUser(w, r)
		return
	}
	if isStreamPath(path) {
		h.handleStream(w, r)
		return
	}
	if isImagePath(path) {
		h.handleImage(w, r)
		return
	}
	if isAggregatablePath(path) {
		h.handleAggregate(w, r)
		return
	}
	if virtualID := extractVirtualID(path); virtualID != "" {
		h.handleSingleItem(w, r, virtualID)
		return
	}

	h.handleFallback(w, r)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取登录请求失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var loginReq struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
	}
	if err := json.Unmarshal(body, &loginReq); err != nil {
		http.Error(w, "登录请求格式错误", http.StatusBadRequest)
		return
	}

	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "当前没有可用上游服务", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.Login)
	defer cancel()

	selected := onlineUpstreams[0]
	resp, err := selected.DoAPIWithHeaders(ctx, http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(body), r.Header)
	if err != nil {
		http.Error(w, "登录失败："+err.Error(), http.StatusUnauthorized)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	var result map[string]any
	_ = json.Unmarshal(respBody, &result)
	var upstreamUserID string
	var virtualUserID string
	if serverID, ok := result["ServerId"].(string); ok && serverID != "" {
		result["ServerId"] = h.cfg.Server.ID
	}
	if user, ok := result["User"].(map[string]any); ok {
		if userID, ok := user["Id"].(string); ok && userID != "" {
			upstreamUserID = userID
			virtualUserID = h.ids.GetOrCreate(userID, selected.ID, "User")
			user["Id"] = virtualUserID
		}
	}
	if accessToken, ok := result["AccessToken"].(string); ok && accessToken != "" && upstreamUserID != "" && virtualUserID != "" {
		h.rememberSession(accessToken, loginSession{
			UpstreamID:     selected.ID,
			UpstreamUserID: upstreamUserID,
			VirtualUserID:  virtualUserID,
		})
	}

	modified, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(modified)

	h.log.Info("用户 [%s] 通过上游 [%s] 登录成功", loginReq.Username, selected.Name)
}

func (h *Handler) handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get("X-Emby-Token"))
	if token == "" {
		h.handleFallback(w, r)
		return
	}

	session, ok := h.lookupSession(token)
	if !ok {
		h.handleFallback(w, r)
		return
	}

	selected := h.upMgr.ByID(session.UpstreamID)
	if selected == nil {
		http.Error(w, "上游服务不可用", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	upstreamPath := "/Users/" + session.UpstreamUserID
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, nil, r.Header)
	if err != nil {
		http.Error(w, "获取当前用户信息失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && len(body) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			payload["Id"] = session.VirtualUserID
			if _, ok := payload["ServerId"].(string); ok {
				payload["ServerId"] = h.cfg.Server.ID
			}
			if rewritten, err := json.Marshal(payload); err == nil {
				body = rewritten
			}
		}
	}

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (h *Handler) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	info := map[string]any{
		"ServerName":             h.cfg.Server.Name,
		"Version":                "4.8.0.0",
		"Id":                     h.cfg.Server.ID,
		"LocalAddress":           fmt.Sprintf("http://localhost:%d", h.cfg.Server.Port),
		"OperatingSystem":        "Linux",
		"HasUpdateAvailable":     false,
		"SupportsLibraryMonitor": true,
		"ProductName":            "FusionRide",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (h *Handler) handleAggregate(w http.ResponseWriter, r *http.Request) {
	fullPath := r.URL.Path
	if r.URL.RawQuery != "" {
		fullPath += "?" + r.URL.RawQuery
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.Aggregate)
	defer cancel()

	var (
		result []byte
		err    error
	)
	if strings.Contains(r.URL.Path, "/Search/Hints") {
		result, err = h.agg.AggregateSearch(ctx, fullPath)
	} else {
		result, err = h.agg.AggregateItems(ctx, fullPath)
	}
	if err != nil {
		h.log.Error("聚合请求失败: %v", err)
		http.Error(w, "聚合请求失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	virtualID := extractStreamID(r.URL.Path)
	if virtualID == "" {
		http.Error(w, "无效的流媒体 ID", http.StatusBadRequest)
		return
	}

	originalID, serverID, ok := h.ids.Resolve(virtualID)
	if !ok {
		http.Error(w, "未找到对应媒体条目", http.StatusNotFound)
		return
	}

	selected := h.upMgr.ByID(serverID)
	if selected == nil {
		http.Error(w, "上游服务不可用", http.StatusServiceUnavailable)
		return
	}

	if selected.EffectivePlaybackMode(h.cfg.Playback.Mode) == "redirect" {
		streamURL := selected.BuildStreamURL(originalID, r.URL.RawQuery)
		h.log.Debug("流媒体重定向 %s -> %s", virtualID, streamURL)
		http.Redirect(w, r, streamURL, http.StatusFound)
		return
	}

	h.proxyStream(w, r, selected, originalID)
}

func (h *Handler) proxyStream(w http.ResponseWriter, r *http.Request, selected *upstream.Upstream, originalID string) {
	upstreamPath := strings.Replace(r.URL.Path, extractStreamID(r.URL.Path), originalID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoAPIWithHeaders(r.Context(), r.Method, upstreamPath, r.Body, r.Header)
	if err != nil {
		h.log.Error("流媒体代理失败: %v", err)
		http.Error(w, "流媒体代理失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	buf := make([]byte, 64*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			h.meter.Add(selected.ID, 0, int64(written))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request) {
	virtualID := extractImageItemID(r.URL.Path)
	if virtualID == "" {
		h.handleFallback(w, r)
		return
	}

	originalID, serverID, ok := h.ids.Resolve(virtualID)
	if !ok {
		h.handleFallback(w, r)
		return
	}

	selected := h.upMgr.ByID(serverID)
	if selected == nil {
		http.Error(w, "上游服务不可用", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	imagePath := strings.Replace(r.URL.Path, virtualID, originalID, 1)
	if r.URL.RawQuery != "" {
		imagePath += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoAPIWithHeaders(ctx, http.MethodGet, imagePath, nil, r.Header)
	if err != nil {
		http.Error(w, "获取图片失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Cache-Control", "public, max-age=86400")
	for key, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(key), "content-") {
			for _, value := range values {
				w.Header().Set(key, value)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) handleSingleItem(w http.ResponseWriter, r *http.Request, virtualID string) {
	originalID, serverID, ok := h.ids.Resolve(virtualID)
	if !ok {
		h.handleFallback(w, r)
		return
	}

	selected := h.upMgr.ByID(serverID)
	if selected == nil {
		http.Error(w, "上游服务不可用", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	upstreamPath := strings.Replace(r.URL.Path, virtualID, originalID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, r.Body, r.Header)
	if err != nil {
		http.Error(w, "请求上游失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	body = h.ids.RewriteIDsInJSON(body, serverID, extractAllIDs(body))

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "当前没有可用上游服务", http.StatusServiceUnavailable)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "当前连接不支持 WebSocket 升级", http.StatusInternalServerError)
		return
	}

	clientConn, clientRW, err := hijacker.Hijack()
	if err != nil {
		h.log.Error("劫持客户端连接失败: %v", err)
		return
	}
	defer clientConn.Close()

	selected := onlineUpstreams[0]
	upstreamAddr := strings.TrimPrefix(selected.URL, "http://")
	upstreamAddr = strings.TrimPrefix(upstreamAddr, "https://")
	upstreamConn, err := net.DialTimeout("tcp", upstreamAddr, 10*time.Second)
	if err != nil {
		h.log.Error("连接上游 WebSocket 失败: %v", err)
		return
	}
	defer upstreamConn.Close()

	req := r.Clone(r.Context())
	req.RequestURI = ""
	req.URL.Scheme = "http"
	req.URL.Host = upstreamAddr
	if err := req.Write(upstreamConn); err != nil {
		h.log.Error("转发 WebSocket 升级请求失败: %v", err)
		return
	}

	if clientRW != nil && clientRW.Reader.Buffered() > 0 {
		buffered, _ := io.ReadAll(clientRW.Reader)
		if len(buffered) > 0 {
			if _, err := upstreamConn.Write(buffered); err != nil {
				h.log.Error("转发 WebSocket 握手失败: %v", err)
				return
			}
		}
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstreamConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upstreamConn)
		done <- struct{}{}
	}()
	<-done
}

func (h *Handler) handleFallback(w http.ResponseWriter, r *http.Request) {
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "当前没有可用上游服务", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	selected := onlineUpstreams[0]
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, path, r.Body, r.Header)
	if err != nil {
		http.Error(w, "代理请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) rememberSession(token string, session loginSession) {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	h.sessions[token] = session
}

func (h *Handler) lookupSession(token string) (loginSession, bool) {
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	session, ok := h.sessions[token]
	return session, ok
}

func (h *Handler) withTimeout(parent context.Context, timeoutMS int) (context.Context, context.CancelFunc) {
	if timeoutMS <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func isAggregatablePath(path string) bool {
	cleanPath := strings.TrimSpace(path)
	cleanPath = strings.TrimPrefix(cleanPath, "/emby")
	if cleanPath == "" {
		cleanPath = "/"
	}

	switch {
	case cleanPath == "/Items":
		return true
	case strings.HasPrefix(cleanPath, "/Search/Hints"):
		return true
	case strings.HasPrefix(cleanPath, "/Shows/"):
		return true
	case cleanPath == "/Genres" || strings.HasPrefix(cleanPath, "/Genres/"):
		return true
	case cleanPath == "/Persons" || strings.HasPrefix(cleanPath, "/Persons/"):
		return true
	case cleanPath == "/Studios" || strings.HasPrefix(cleanPath, "/Studios/"):
		return true
	case cleanPath == "/Years" || strings.HasPrefix(cleanPath, "/Years/"):
		return true
	}

	if !strings.HasPrefix(cleanPath, "/Users/") {
		return false
	}

	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	if len(parts) < 3 {
		return false
	}

	userID := strings.TrimSpace(parts[1])
	if userID == "" || strings.EqualFold(userID, "me") || strings.EqualFold(userID, "public") {
		return false
	}

	switch parts[2] {
	case "Items", "Views":
		return true
	default:
		return false
	}
}

func isStreamPath(path string) bool {
	return strings.Contains(path, "/Videos/") && (strings.Contains(path, "/stream") || strings.Contains(path, "/master.m3u8") || strings.Contains(path, "/main.m3u8")) ||
		(strings.Contains(path, "/Audio/") && strings.Contains(path, "/stream"))
}

func isImagePath(path string) bool {
	return strings.Contains(path, "/Images/")
}

func extractVirtualID(path string) string {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if len(part) == 32 && isHex(part) {
			return part
		}
	}
	return ""
}

func extractStreamID(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if (part == "Videos" || part == "Audio") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractImageItemID(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "Items" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, char := range s {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func extractAllIDs(data []byte) []string {
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	keys := []string{"Id", "SeriesId", "SeasonId", "ParentId", "AlbumId", "ChannelId"}
	var ids []string
	for _, key := range keys {
		if value, ok := parsed[key].(string); ok && value != "" {
			ids = append(ids, value)
		}
	}
	return ids
}

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

	sessionMu              sync.RWMutex
	sessions               map[string]loginSession
	sessionMaxAge          time.Duration
	sessionCleanupInterval time.Duration
	cleanupOnce            sync.Once

	playbackMu       sync.RWMutex
	playbackSessions map[string]playbackSession

	imageCache *imageCache
}

type loginSession struct {
	UpstreamID     int
	UpstreamUserID string
	VirtualUserID  string
	lastAccess     time.Time
}

type playbackSession struct {
	UpstreamID     int
	OriginalItemID string
	VirtualItemID  string
	lastAccess     time.Time
}

func NewHandler(cfg *config.Config, upMgr *upstream.Manager, agg *aggregator.Aggregator, ids *idmap.Store, log *logger.Logger, meter *traffic.Meter) *Handler {
	return &Handler{
		cfg:                    cfg,
		upMgr:                  upMgr,
		agg:                    agg,
		ids:                    ids,
		log:                    log,
		meter:                  meter,
		sessions:               make(map[string]loginSession),
		sessionMaxAge:          24 * time.Hour,
		sessionCleanupInterval: 10 * time.Minute,
		playbackSessions:       make(map[string]playbackSession),
		imageCache:             newImageCache(200<<20, 500<<10),
	}
}

func (h *Handler) StartSessionCleanup(ctx context.Context) {
	if h.sessionCleanupInterval <= 0 || h.sessionMaxAge <= 0 {
		return
	}

	h.cleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(h.sessionCleanupInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					removedLogins := h.cleanupExpiredSessions(time.Now())
					removedPlayback := h.cleanupExpiredPlaybackSessions(time.Now())
					if removedLogins > 0 || removedPlayback > 0 {
						h.log.Debug("已清理 %d 个登录会话和 %d 个播放会话", removedLogins, removedPlayback)
					}
				}
			}
		}()
	})
}

func (h *Handler) cleanupExpiredSessions(now time.Time) int {
	if h.sessionMaxAge <= 0 {
		return 0
	}

	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()

	removed := 0
	for token, session := range h.sessions {
		if session.lastAccess.IsZero() || now.Sub(session.lastAccess) > h.sessionMaxAge {
			delete(h.sessions, token)
			removed++
		}
	}
	return removed
}

func (h *Handler) cleanupExpiredPlaybackSessions(now time.Time) int {
	if h.sessionMaxAge <= 0 {
		return 0
	}

	h.playbackMu.Lock()
	defer h.playbackMu.Unlock()

	removed := 0
	for playSessionID, session := range h.playbackSessions {
		if session.lastAccess.IsZero() || now.Sub(session.lastAccess) > h.sessionMaxAge {
			delete(h.playbackSessions, playSessionID)
			removed++
		}
	}
	return removed
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case isWebSocket(r):
		h.handleWebSocket(w, r)
	case strings.HasSuffix(path, "/AuthenticateByName"):
		h.handleLogin(w, r)
	case path == "/System/Info/Public" || path == "/emby/System/Info/Public":
		h.handleSystemInfo(w, r)
	case path == "/System/Info" || path == "/emby/System/Info":
		h.handleSystemInfoAuth(w, r)
	case path == "/Users/Me" || path == "/emby/Users/Me":
		h.handleCurrentUser(w, r)
	case isPlaybackInfoPath(path):
		h.handlePlaybackInfo(w, r)
	case isPlaybackReportPath(path):
		h.handlePlaybackReport(w, r)
	case isStreamPath(path):
		h.handleStream(w, r)
	case isImagePath(path):
		h.handleImage(w, r)
	case isAggregatablePath(path):
		h.handleAggregate(w, r)
	case extractVirtualID(path) != "":
		h.handleSingleItem(w, r, extractVirtualID(path))
	default:
		h.handleFallback(w, r)
	}
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

	var (
		selected *upstream.Upstream
		respBody []byte
		lastErr  error
	)

	for _, candidate := range onlineUpstreams {
		resp, err := candidate.DoAPIWithHeaders(ctx, http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(body), r.Header)
		if err != nil {
			lastErr = err
			continue
		}

		candidateBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("读取上游 [%s] 登录响应失败: %w", candidate.Name, readErr)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			selected = candidate
			respBody = candidateBody
			break
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			lastErr = fmt.Errorf("上游 [%s] 用户名或密码错误", candidate.Name)
			continue
		}

		lastErr = fmt.Errorf("上游 [%s] 返回 HTTP %d", candidate.Name, resp.StatusCode)
	}

	if selected == nil {
		if lastErr != nil {
			h.log.Warn("用户 [%s] 登录失败: %v", loginReq.Username, lastErr)
		}
		http.Error(w, "所有上游均登录失败", http.StatusUnauthorized)
		return
	}

	var result map[string]any
	_ = json.Unmarshal(respBody, &result)
	var upstreamUserID string
	var virtualUserID string
	if _, ok := result["ServerId"].(string); ok {
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

	modified, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "重写登录响应失败", http.StatusInternalServerError)
		return
	}

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

	copyResponseHeaders(w.Header(), resp.Header)
	if resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (h *Handler) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info, ok := h.fetchSystemInfo(r.Context(), "/System/Info/Public", nil, false)
	if !ok {
		info = map[string]any{
			"StartupWizardCompleted": true,
			"Version":                "4.8.0.0",
			"OperatingSystem":        "Linux",
			"ProductName":            "Emby Server",
		}
	}

	h.writeSystemInfo(w, info)
}

func (h *Handler) handleSystemInfoAuth(w http.ResponseWriter, r *http.Request) {
	info, ok := h.fetchSystemInfo(r.Context(), "/System/Info", r.Header, true)
	if !ok {
		info = map[string]any{
			"StartupWizardCompleted": true,
			"Version":                "4.8.0.0",
			"OperatingSystem":        "Linux",
			"ProductName":            "Emby Server",
		}
	}

	h.writeSystemInfo(w, info)
}

func (h *Handler) fetchSystemInfo(parent context.Context, path string, incoming http.Header, authenticated bool) (map[string]any, bool) {
	for _, candidate := range h.systemInfoCandidates(incoming) {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)

		var (
			resp *http.Response
			err  error
		)
		if authenticated {
			resp, err = candidate.DoAPIWithHeaders(ctx, http.MethodGet, path, nil, incoming)
		} else {
			resp, err = candidate.DoAPI(ctx, http.MethodGet, path, nil)
		}
		cancel()
		if err != nil {
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}

		var info map[string]any
		if json.Unmarshal(body, &info) == nil {
			return info, true
		}
	}

	return nil, false
}

func (h *Handler) systemInfoCandidates(incoming http.Header) []*upstream.Upstream {
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return nil
	}

	var candidates []*upstream.Upstream
	seen := make(map[int]struct{}, len(onlineUpstreams))
	if incoming != nil {
		if token := strings.TrimSpace(incoming.Get("X-Emby-Token")); token != "" {
			if session, ok := h.lookupSession(token); ok {
				if preferred := h.upMgr.ByID(session.UpstreamID); preferred != nil {
					candidates = append(candidates, preferred)
					seen[preferred.ID] = struct{}{}
				}
			}
		}
	}

	for _, candidate := range onlineUpstreams {
		if _, ok := seen[candidate.ID]; ok {
			continue
		}
		candidates = append(candidates, candidate)
	}

	return candidates
}

func (h *Handler) writeSystemInfo(w http.ResponseWriter, info map[string]any) {
	if info == nil {
		info = make(map[string]any)
	}

	info["StartupWizardCompleted"] = true
	info["ServerName"] = h.cfg.Server.Name
	info["Id"] = h.cfg.Server.ID
	delete(info, "LocalAddress")
	delete(info, "LocalAddresses")
	delete(info, "RemoteAddress")
	delete(info, "RemoteAddresses")
	delete(info, "WanAddress")
	delete(info, "WanAddresses")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (h *Handler) handleAggregate(w http.ResponseWriter, r *http.Request) {
	if h.agg == nil {
		http.Error(w, "聚合器未初始化", http.StatusInternalServerError)
		return
	}

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

	copyResponseHeaders(w.Header(), resp.Header)
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

	imagePath := strings.Replace(r.URL.Path, virtualID, originalID, 1)
	if r.URL.RawQuery != "" {
		imagePath += "?" + r.URL.RawQuery
	}

	cacheKey := fmt.Sprintf("%d:%s", selected.ID, imagePath)
	if cached, ok := h.imageCache.Get(cacheKey); ok {
		copyResponseHeaders(w.Header(), cached.headers)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(cached.status)
		_, _ = w.Write(cached.body)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	resp, err := selected.DoAPIWithHeaders(ctx, http.MethodGet, imagePath, nil, r.Header)
	if err != nil {
		http.Error(w, "获取图片失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取图片失败", http.StatusBadGateway)
		return
	}

	contentHeaders := make(http.Header)
	for key, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(key), "content-") || strings.EqualFold(key, "ETag") || strings.EqualFold(key, "Last-Modified") {
			for _, value := range values {
				contentHeaders.Add(key, value)
			}
		}
	}

	if resp.StatusCode == http.StatusOK {
		h.imageCache.Put(cacheKey, resp.StatusCode, contentHeaders, body)
	}

	copyResponseHeaders(w.Header(), contentHeaders)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (h *Handler) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	virtualID := extractVirtualID(r.URL.Path)
	if virtualID == "" {
		h.handleFallback(w, r)
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

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	requestBody, err := readRequestBody(r)
	if err != nil {
		http.Error(w, "读取播放信息请求失败", http.StatusBadRequest)
		return
	}

	upstreamPath := h.rewritePathForUpstream(r.URL.Path, r.URL.RawQuery, selected)
	requestBody = h.devirtualizeRequestBody(requestBody, selected.ID)

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, readerFromBytes(requestBody), r.Header)
	if err != nil {
		http.Error(w, "获取播放信息失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取播放信息响应失败", http.StatusBadGateway)
		return
	}

	if isJSONContentType(resp.Header) {
		h.rememberPlaybackSessionFromResponse(responseBody, selected.ID, originalID, virtualID)
		responseBody = h.virtualizeJSONBytes(responseBody, selected.ID)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if isJSONContentType(resp.Header) {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (h *Handler) handleSingleItem(w http.ResponseWriter, r *http.Request, virtualID string) {
	_, serverID, ok := h.ids.Resolve(virtualID)
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

	requestBody, err := readRequestBody(r)
	if err != nil {
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}

	upstreamPath := h.rewritePathForUpstream(r.URL.Path, r.URL.RawQuery, selected)
	requestBody = h.devirtualizeRequestBody(requestBody, selected.ID)

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, readerFromBytes(requestBody), r.Header)
	if err != nil {
		http.Error(w, "请求上游失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取上游响应失败", http.StatusBadGateway)
		return
	}

	if isJSONContentType(resp.Header) {
		body = h.virtualizeJSONBytes(body, selected.ID)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if isJSONContentType(resp.Header) {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (h *Handler) handlePlaybackReport(w http.ResponseWriter, r *http.Request) {
	requestBody, err := readRequestBody(r)
	if err != nil {
		http.Error(w, "读取播放状态请求失败", http.StatusBadRequest)
		return
	}

	selected := h.selectUpstreamForPlaybackReport(r, requestBody)
	if selected == nil {
		http.Error(w, "当前没有可用上游服务", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	upstreamPath := h.rewritePathForUpstream(r.URL.Path, r.URL.RawQuery, selected)
	requestBody = h.devirtualizeRequestBody(requestBody, selected.ID)

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, readerFromBytes(requestBody), r.Header)
	if err != nil {
		http.Error(w, "转发播放状态失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody, _ := io.ReadAll(resp.Body)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)

	if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/Stopped") {
		if playSessionID := extractPlaySessionID(requestBody); playSessionID != "" {
			h.removePlaybackSession(playSessionID)
		}
	}
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
	requestBody, err := readRequestBody(r)
	if err != nil {
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}

	selected := h.selectUpstreamForFallback(r, requestBody)
	if selected == nil {
		http.Error(w, "当前没有可用上游服务", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := h.withTimeout(r.Context(), h.cfg.Timeouts.API)
	defer cancel()

	upstreamPath := h.rewritePathForUpstream(r.URL.Path, r.URL.RawQuery, selected)
	requestBody = h.devirtualizeRequestBody(requestBody, selected.ID)

	resp, err := selected.DoAPIWithHeaders(ctx, r.Method, upstreamPath, readerFromBytes(requestBody), r.Header)
	if err != nil {
		http.Error(w, "代理请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取上游响应失败", http.StatusBadGateway)
		return
	}
	if isJSONContentType(resp.Header) {
		responseBody = h.virtualizeJSONBytes(responseBody, selected.ID)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if isJSONContentType(resp.Header) {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (h *Handler) rememberSession(token string, session loginSession) {
	if token == "" {
		return
	}

	session.lastAccess = time.Now()
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	h.sessions[token] = session
}

func (h *Handler) lookupSession(token string) (loginSession, bool) {
	if token == "" {
		return loginSession{}, false
	}

	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()

	session, ok := h.sessions[token]
	if !ok {
		return loginSession{}, false
	}
	session.lastAccess = time.Now()
	h.sessions[token] = session
	return session, true
}

func (h *Handler) rememberPlaybackSession(playSessionID string, session playbackSession) {
	if playSessionID == "" {
		return
	}

	session.lastAccess = time.Now()
	h.playbackMu.Lock()
	defer h.playbackMu.Unlock()
	h.playbackSessions[playSessionID] = session
}

func (h *Handler) lookupPlaybackSession(playSessionID string) (playbackSession, bool) {
	if playSessionID == "" {
		return playbackSession{}, false
	}

	h.playbackMu.Lock()
	defer h.playbackMu.Unlock()

	session, ok := h.playbackSessions[playSessionID]
	if !ok {
		return playbackSession{}, false
	}
	session.lastAccess = time.Now()
	h.playbackSessions[playSessionID] = session
	return session, true
}

func (h *Handler) removePlaybackSession(playSessionID string) {
	if playSessionID == "" {
		return
	}
	h.playbackMu.Lock()
	defer h.playbackMu.Unlock()
	delete(h.playbackSessions, playSessionID)
}

func (h *Handler) rememberPlaybackSessionFromResponse(body []byte, upstreamID int, originalItemID, virtualItemID string) {
	playSessionID := extractPlaySessionID(body)
	if playSessionID == "" {
		return
	}

	h.rememberPlaybackSession(playSessionID, playbackSession{
		UpstreamID:     upstreamID,
		OriginalItemID: originalItemID,
		VirtualItemID:  virtualItemID,
	})
}

func (h *Handler) selectUpstreamForPlaybackReport(r *http.Request, body []byte) *upstream.Upstream {
	if playSessionID := firstNonEmpty(extractPlaySessionID(body), strings.TrimSpace(r.URL.Query().Get("PlaySessionId"))); playSessionID != "" {
		if session, ok := h.lookupPlaybackSession(playSessionID); ok {
			if selected := h.upMgr.ByID(session.UpstreamID); selected != nil {
				return selected
			}
		}
	}

	if virtualID := extractVirtualItemID(body); virtualID != "" {
		if _, serverID, ok := h.ids.Resolve(virtualID); ok {
			if selected := h.upMgr.ByID(serverID); selected != nil {
				return selected
			}
		}
	}

	return h.selectUpstreamForFallback(r, body)
}

func (h *Handler) selectUpstreamForFallback(r *http.Request, body []byte) *upstream.Upstream {
	if token := strings.TrimSpace(r.Header.Get("X-Emby-Token")); token != "" {
		if session, ok := h.lookupSession(token); ok {
			if selected := h.upMgr.ByID(session.UpstreamID); selected != nil {
				return selected
			}
		}
	}

	if playSessionID := firstNonEmpty(extractPlaySessionID(body), strings.TrimSpace(r.URL.Query().Get("PlaySessionId"))); playSessionID != "" {
		if session, ok := h.lookupPlaybackSession(playSessionID); ok {
			if selected := h.upMgr.ByID(session.UpstreamID); selected != nil {
				return selected
			}
		}
	}

	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return nil
	}
	return onlineUpstreams[0]
}

func (h *Handler) rewritePathForUpstream(path string, rawQuery string, selected *upstream.Upstream) string {
	parsed := &url.URL{Path: path, RawQuery: rawQuery}
	segments := strings.Split(parsed.Path, "/")

	if len(segments) > 2 && segments[1] == "Users" {
		if userID := selected.GetUserID(); userID != "" {
			segments[2] = userID
		}
	}

	for i, segment := range segments {
		if !isLikelyVirtualID(segment) {
			continue
		}
		if originalID, ok := h.originalIDForUpstream(segment, selected.ID); ok {
			segments[i] = originalID
		}
	}

	parsed.Path = strings.Join(segments, "/")
	if parsed.RawQuery != "" {
		query := parsed.Query()
		for key, values := range query {
			if !shouldRewriteScalarKey(key) && !shouldRewriteArrayKey(key) {
				continue
			}
			for i, value := range values {
				if originalID, ok := h.originalIDForUpstream(value, selected.ID); ok {
					values[i] = originalID
				}
			}
			query[key] = values
		}
		parsed.RawQuery = query.Encode()
	}

	return parsed.RequestURI()
}

func (h *Handler) devirtualizeRequestBody(body []byte, upstreamID int) []byte {
	if len(body) == 0 {
		return body
	}
	return h.devirtualizeJSONBytes(body, upstreamID)
}

func (h *Handler) originalIDForUpstream(virtualID string, upstreamID int) (string, bool) {
	originalID, serverID, ok := h.ids.Resolve(virtualID)
	if !ok {
		return "", false
	}
	if serverID == upstreamID {
		return originalID, true
	}

	for _, instance := range h.ids.GetInstances(virtualID) {
		if instance.ServerID == upstreamID {
			return instance.OriginalID, true
		}
	}
	return "", false
}

func (h *Handler) withTimeout(parent context.Context, timeoutMS int) (context.Context, context.CancelFunc) {
	if timeoutMS <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
}

func copyResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func readerFromBytes(body []byte) io.Reader {
	if len(body) == 0 {
		return nil
	}
	return bytes.NewReader(body)
}

func isJSONContentType(headers http.Header) bool {
	return strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/json")
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

func isPlaybackInfoPath(path string) bool {
	return strings.HasSuffix(strings.TrimRight(path, "/"), "/PlaybackInfo")
}

func isPlaybackReportPath(path string) bool {
	cleanPath := strings.TrimPrefix(strings.TrimRight(path, "/"), "/emby")
	switch cleanPath {
	case "/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped":
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
		if isLikelyVirtualID(part) {
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

func extractPlaySessionID(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}

	playSessionID, _ := parsed["PlaySessionId"].(string)
	return strings.TrimSpace(playSessionID)
}

func extractVirtualItemID(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}

	itemID, _ := parsed["ItemId"].(string)
	itemID = strings.TrimSpace(itemID)
	if !isLikelyVirtualID(itemID) {
		return ""
	}
	return itemID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isLikelyVirtualID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

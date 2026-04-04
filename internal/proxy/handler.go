package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fusionride/fusion-ride/internal/aggregator"
	"github.com/fusionride/fusion-ride/internal/config"
	"github.com/fusionride/fusion-ride/internal/idmap"
	"github.com/fusionride/fusion-ride/internal/logger"
	"github.com/fusionride/fusion-ride/internal/traffic"
	"github.com/fusionride/fusion-ride/internal/upstream"
)

// Handler 处理所有 Emby API 代理请求。
type Handler struct {
	cfg    *config.Config
	upMgr  *upstream.Manager
	agg    *aggregator.Aggregator
	ids    *idmap.Store
	log    *logger.Logger
	meter  *traffic.Meter
}

// NewHandler 创建代理处理器。
func NewHandler(cfg *config.Config, upMgr *upstream.Manager, agg *aggregator.Aggregator,
	ids *idmap.Store, log *logger.Logger, meter *traffic.Meter) *Handler {
	return &Handler{
		cfg:   cfg,
		upMgr: upMgr,
		agg:   agg,
		ids:   ids,
		log:   log,
		meter: meter,
	}
}

// ServeHTTP 是主请求处理入口。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// WebSocket 升级
	if isWebSocket(r) {
		h.handleWebSocket(w, r)
		return
	}

	// 登录（聚合认证）
	if strings.HasSuffix(path, "/AuthenticateByName") {
		h.handleLogin(w, r)
		return
	}

	// 系统信息（返回 FusionRide 自身信息）
	if path == "/System/Info/Public" || path == "/emby/System/Info/Public" {
		h.handleSystemInfo(w, r)
		return
	}

	// 流媒体播放
	if isStreamPath(path) {
		h.handleStream(w, r)
		return
	}

	// 图片代理
	if isImagePath(path) {
		h.handleImage(w, r)
		return
	}

	// 可聚合 API
	if isAggregatablePath(path) {
		h.handleAggregate(w, r)
		return
	}

	// 单容器路径（包含虚拟 ID 的请求）
	if vid := extractVirtualID(path); vid != "" {
		h.handleSingleItem(w, r, vid)
		return
	}

	// 兜底：代理到第一个在线上游
	h.handleFallback(w, r)
}

// ── 登录处理 ──

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var loginReq struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
	}
	if err := json.Unmarshal(body, &loginReq); err != nil {
		http.Error(w, "请求体格式错误", http.StatusBadRequest)
		return
	}

	// 尝试在所有上游登录
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "无可用上游服务器", http.StatusServiceUnavailable)
		return
	}

	// 使用第一个上游的登录结果
	u := onlineUpstreams[0]
	resp, err := u.DoAPI("POST", "/Users/AuthenticateByName", strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, "登录失败: "+err.Error(), http.StatusUnauthorized)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// 替换响应中的服务器信息
	var result map[string]any
	json.Unmarshal(respBody, &result)

	if srvInfo, ok := result["ServerId"].(string); ok && srvInfo != "" {
		result["ServerId"] = h.cfg.Server.ID
	}

	// 虚拟化 User ID
	if user, ok := result["User"].(map[string]any); ok {
		if uid, ok := user["Id"].(string); ok {
			user["Id"] = h.ids.GetOrCreate(uid, u.ID, "User")
		}
	}

	modified, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	w.Write(modified)

	h.log.Info("用户 [%s] 通过上游 [%s] 登录成功", loginReq.Username, u.Name)
}

// ── 系统信息 ──

func (h *Handler) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"ServerName":            h.cfg.Server.Name,
		"Version":              "4.8.0.0",
		"Id":                   h.cfg.Server.ID,
		"LocalAddress":          fmt.Sprintf("http://localhost:%d", h.cfg.Server.Port),
		"OperatingSystem":       "Linux",
		"HasUpdateAvailable":    false,
		"SupportsLibraryMonitor": true,
		"ProductName":           "FusionRide",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// ── 聚合请求 ──

func (h *Handler) handleAggregate(w http.ResponseWriter, r *http.Request) {
	fullPath := r.URL.Path
	if r.URL.RawQuery != "" {
		fullPath += "?" + r.URL.RawQuery
	}

	var result []byte
	var err error

	if strings.Contains(r.URL.Path, "/Search/Hints") {
		result, err = h.agg.AggregateSearch(fullPath)
	} else {
		result, err = h.agg.AggregateItems(fullPath)
	}

	if err != nil {
		h.log.Error("聚合请求失败: %v", err)
		http.Error(w, "聚合失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// ── 流媒体处理 ──

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	vid := extractStreamID(r.URL.Path)
	if vid == "" {
		http.Error(w, "无效的流 ID", http.StatusBadRequest)
		return
	}

	origID, serverID, ok := h.ids.Resolve(vid)
	if !ok {
		http.Error(w, "未知的媒体 ID", http.StatusNotFound)
		return
	}

	u := h.upMgr.ByID(serverID)
	if u == nil {
		http.Error(w, "上游服务器不可用", http.StatusServiceUnavailable)
		return
	}

	mode := u.EffectivePlaybackMode(h.cfg.Playback.Mode)

	if mode == "redirect" {
		// 302 重定向模式
		streamURL := u.BuildStreamURL(origID, r.URL.RawQuery)
		h.log.Debug("302 重定向: %s → %s", vid, streamURL)
		http.Redirect(w, r, streamURL, http.StatusFound)
		return
	}

	// proxy 中转模式
	h.proxyStream(w, r, u, origID)
}

func (h *Handler) proxyStream(w http.ResponseWriter, r *http.Request, u *upstream.Upstream, originalID string) {
	// 构建上游请求路径
	upstreamPath := strings.Replace(r.URL.Path, extractStreamID(r.URL.Path), originalID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := u.DoAPI(r.Method, upstreamPath, r.Body)
	if err != nil {
		h.log.Error("流代理失败: %v", err)
		http.Error(w, "流代理失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 转发响应头
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 背压式流式转发 + 流量计量
	buf := make([]byte, 64*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			h.meter.Add(u.ID, 0, int64(written))

			// 刷新缓冲
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ── 图片代理 ──

func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request) {
	vid := extractImageItemID(r.URL.Path)
	if vid == "" {
		h.handleFallback(w, r)
		return
	}

	origID, serverID, ok := h.ids.Resolve(vid)
	if !ok {
		h.handleFallback(w, r)
		return
	}

	u := h.upMgr.ByID(serverID)
	if u == nil {
		http.Error(w, "上游不可用", http.StatusServiceUnavailable)
		return
	}

	imgPath := strings.Replace(r.URL.Path, vid, origID, 1)
	if r.URL.RawQuery != "" {
		imgPath += "?" + r.URL.RawQuery
	}

	resp, err := u.DoAPI("GET", imgPath, nil)
	if err != nil {
		http.Error(w, "图片获取失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 缓存友好
	w.Header().Set("Cache-Control", "public, max-age=86400")
	for k, vv := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "content-") {
			for _, v := range vv {
				w.Header().Set(k, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ── 单条目请求 ──

func (h *Handler) handleSingleItem(w http.ResponseWriter, r *http.Request, virtualID string) {
	origID, serverID, ok := h.ids.Resolve(virtualID)
	if !ok {
		h.handleFallback(w, r)
		return
	}

	u := h.upMgr.ByID(serverID)
	if u == nil {
		http.Error(w, "上游不可用", http.StatusServiceUnavailable)
		return
	}

	// 替换路径中的虚拟 ID
	upstreamPath := strings.Replace(r.URL.Path, virtualID, origID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := u.DoAPI(r.Method, upstreamPath, r.Body)
	if err != nil {
		http.Error(w, "请求上游失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// 虚拟化响应中的 ID
	body = h.ids.RewriteIDsInJSON(body, serverID, extractAllIDs(body))

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// ── WebSocket ──

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "无可用上游", http.StatusServiceUnavailable)
		return
	}

	// WebSocket 连接到第一个在线上游
	u := onlineUpstreams[0]
	targetURL := strings.Replace(u.URL, "http", "ws", 1) + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Hijack 客户端连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket 不支持", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		h.log.Error("WebSocket hijack 失败: %v", err)
		return
	}
	defer clientConn.Close()

	// 连接上游
	targetURL = strings.Replace(targetURL, "wss://", "ws://", 1) // 简化处理
	upstreamAddr := strings.TrimPrefix(u.URL, "http://")
	upstreamAddr = strings.TrimPrefix(upstreamAddr, "https://")

	upstreamConn, err := net.DialTimeout("tcp", upstreamAddr, 10*time.Second)
	if err != nil {
		h.log.Error("WebSocket 连接上游失败: %v", err)
		return
	}
	defer upstreamConn.Close()

	// 简化 WebSocket 代理：双向转发原始 TCP
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(upstreamConn, clientConn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(clientConn, upstreamConn)
		done <- struct{}{}
	}()

	<-done
}

// ── 兜底代理 ──

func (h *Handler) handleFallback(w http.ResponseWriter, r *http.Request) {
	onlineUpstreams := h.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		http.Error(w, "无可用上游服务器", http.StatusServiceUnavailable)
		return
	}

	u := onlineUpstreams[0]
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}

	resp, err := u.DoAPI(r.Method, path, r.Body)
	if err != nil {
		http.Error(w, "代理请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ── 路径判断 ──

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func isAggregatablePath(path string) bool {
	aggregatablePatterns := []string{
		"/Items",
		"/Users/", // + uid + /Items
		"/Search/Hints",
		"/Shows/",
		"/Genres",
		"/Persons",
		"/Studios",
		"/Years",
	}

	for _, p := range aggregatablePatterns {
		if strings.Contains(path, p) {
			// 排除单条目详情
			if strings.Count(path, "/") <= 4 {
				return true
			}
		}
	}

	return false
}

func isStreamPath(path string) bool {
	return strings.Contains(path, "/Videos/") && (strings.Contains(path, "/stream") ||
		strings.Contains(path, "/master.m3u8") ||
		strings.Contains(path, "/main.m3u8")) ||
		strings.Contains(path, "/Audio/") && strings.Contains(path, "/stream")
}

func isImagePath(path string) bool {
	return strings.Contains(path, "/Images/")
}

func extractVirtualID(path string) string {
	// 从路径中提取可能的虚拟 ID（UUID 格式，32个十六进制字符无连字符）
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if len(part) == 32 && isHex(part) {
			return part
		}
	}
	return ""
}

func extractStreamID(path string) string {
	// /Videos/{id}/stream → 提取 {id}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if (part == "Videos" || part == "Audio") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractImageItemID(path string) string {
	// /Items/{id}/Images/Primary → 提取 {id}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "Items" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
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
	return aggregator_extractIDs(parsed)
}

func aggregator_extractIDs(data map[string]any) []string {
	idKeys := []string{"Id", "SeriesId", "SeasonId", "ParentId", "AlbumId", "ChannelId"}
	var ids []string
	for _, key := range idKeys {
		if v, ok := data[key].(string); ok && v != "" {
			ids = append(ids, v)
		}
	}
	return ids
}

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/snnabb/fusion-ride/internal/upstream"
)

type redirectFollowTransport struct {
	base         http.RoundTripper
	allowedHosts map[string]bool
	spoofer      interface{ ApplyToHeader(http.Header) }
}

func (t *redirectFollowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	for i := 0; i < 3; i++ {
		if resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode > http.StatusPermanentRedirect {
			break
		}
		location := resp.Header.Get("Location")
		if location == "" {
			break
		}

		locationURL, err := url.Parse(location)
		if err != nil {
			break
		}
		if locationURL.Host == "" {
			locationURL = req.URL.ResolveReference(locationURL)
		}

		if !isAllowedRedirectHost(t.allowedHosts, locationURL) {
			break
		}

		resp.Body.Close()
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, locationURL.String(), nil)
		if err != nil {
			break
		}
		for key, values := range req.Header {
			newReq.Header[key] = append([]string(nil), values...)
		}
		newReq.Host = locationURL.Host
		if t.spoofer != nil {
			t.spoofer.ApplyToHeader(newReq.Header)
		}

		resp, err = base.RoundTrip(newReq)
		if err != nil {
			return nil, err
		}
		req = newReq
	}

	return resp, nil
}

func (h *Handler) handlePlaybackInfoCompat(w http.ResponseWriter, r *http.Request) {
	virtualID := extractVirtualID(normalizeRoutePath(r.URL.Path))
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
		responseBody = h.rewritePlaybackInfoResponse(responseBody, selected, originalID, virtualID)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if isJSONContentType(resp.Header) {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (h *Handler) handleStreamCompat(w http.ResponseWriter, r *http.Request) {
	virtualID := extractStreamID(normalizeRoutePath(r.URL.Path))
	if virtualID == "" {
		http.Error(w, "无效的流媒体 ID", http.StatusBadRequest)
		return
	}

	h.log.Debug("流媒体请求: 路径=%s 提取ID=%s 查询=%s", r.URL.Path, virtualID, r.URL.RawQuery)

	originalID, selected := h.resolveStreamTarget(r, virtualID)
	if selected == nil || originalID == "" {
		h.log.Warn("流媒体请求无法定位上游: 路径=%s ID=%s 查询=%s", r.URL.Path, virtualID, r.URL.RawQuery)
		http.Error(w, "未找到对应媒体条目", http.StatusNotFound)
		return
	}

	mode := selected.EffectivePlaybackMode(h.cfg.Playback.Mode)
	streamQuery := r.URL.Query()
	for _, param := range []string{"MediaSourceId", "mediaSourceId", "ItemId", "itemId"} {
		if virtualQueryID := strings.TrimSpace(streamQuery.Get(param)); virtualQueryID != "" {
			if originalQueryID, _, ok := h.ids.Resolve(virtualQueryID); ok {
				streamQuery.Set(param, originalQueryID)
			}
		}
	}
	rawQuery := streamQuery.Encode()
	switch mode {
	case "direct":
		streamURL := selected.BuildStreamURL(originalID, rawQuery)
		h.log.Debug("流媒体直连重定向: %s -> %s", virtualID, streamURL)
		http.Redirect(w, r, streamURL, http.StatusFound)
	case "redirect":
		rewritten := r.Clone(r.Context())
		rewritten.URL.RawQuery = rawQuery
		h.proxyStreamWithRedirectFollow(w, rewritten, selected, originalID)
	default:
		rewritten := r.Clone(r.Context())
		rewritten.URL.RawQuery = rawQuery
		h.proxyStreamCompat(w, rewritten, selected, originalID)
	}
}

func (h *Handler) resolveStreamTarget(r *http.Request, requestedID string) (string, *upstream.Upstream) {
	if originalID, serverID, ok := h.ids.Resolve(requestedID); ok {
		return originalID, h.upMgr.ByID(serverID)
	}

	query := r.URL.Query()
	if mediaSourceID := strings.TrimSpace(query.Get("MediaSourceId")); mediaSourceID != "" {
		if originalID, serverID, ok := h.ids.Resolve(mediaSourceID); ok {
			h.log.Debug("流媒体请求通过 MediaSourceId 回退命中: 请求ID=%s 媒体源ID=%s", requestedID, mediaSourceID)
			return originalID, h.upMgr.ByID(serverID)
		}
	}
	if itemID := strings.TrimSpace(query.Get("ItemId")); itemID != "" {
		if originalID, serverID, ok := h.ids.Resolve(itemID); ok {
			h.log.Debug("流媒体请求通过 ItemId 回退命中: 请求ID=%s ItemId=%s", requestedID, itemID)
			return originalID, h.upMgr.ByID(serverID)
		}
	}
	if playSessionID := strings.TrimSpace(query.Get("PlaySessionId")); playSessionID != "" {
		if session, ok := h.lookupPlaybackSession(playSessionID); ok {
			h.log.Debug("流媒体请求通过播放会话回退命中: 请求ID=%s PlaySessionId=%s", requestedID, playSessionID)
			return session.OriginalItemID, h.upMgr.ByID(session.UpstreamID)
		}
	}
	if selected := h.selectUpstreamForFallback(r, nil); selected != nil {
		if originalID, ok := h.originalIDForUpstream(requestedID, selected.ID); ok {
			h.log.Debug("流媒体请求通过实例回退命中: 请求ID=%s 上游=%d", requestedID, selected.ID)
			return originalID, selected
		}
	}

	return "", nil
}

func (h *Handler) proxyStreamCompat(w http.ResponseWriter, r *http.Request, selected *upstream.Upstream, originalID string) {
	upstreamPath := strings.Replace(r.URL.Path, extractStreamID(r.URL.Path), originalID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	resp, err := selected.DoPlaybackWithHeaders(r.Context(), r.Method, upstreamPath, r.Body, r.Header, nil)
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

func (h *Handler) proxyStreamWithRedirectFollow(w http.ResponseWriter, r *http.Request, selected *upstream.Upstream, originalID string) {
	upstreamPath := strings.Replace(r.URL.Path, extractStreamID(r.URL.Path), originalID, 1)
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	baseTransport := http.DefaultTransport
	clientTimeout := time.Duration(0)
	selected.Mu.RLock()
	if selected.Client != nil {
		clientTimeout = selected.Client.Timeout
		if selected.Client.Transport != nil {
			baseTransport = selected.Client.Transport
		}
	}
	spoofer := selected.Spoofer
	selected.Mu.RUnlock()

	client := &http.Client{
		Timeout: clientTimeout,
		Transport: &redirectFollowTransport{
			base:         baseTransport,
			allowedHosts: selected.AllowedStreamHosts(),
			spoofer:      spoofer,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := selected.DoPlaybackWithHeaders(r.Context(), r.Method, upstreamPath, r.Body, r.Header, client)
	if err != nil {
		h.log.Error("流媒体重定向跟随失败: %v", err)
		http.Error(w, "流媒体重定向跟随失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) rewritePlaybackInfoResponse(body []byte, selected *upstream.Upstream, originalItemID, virtualItemID string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	rawMediaSources, ok := payload["MediaSources"].([]any)
	if !ok {
		return body
	}

	for _, entry := range rawMediaSources {
		mediaSource, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		h.rewriteMediaSource(mediaSource, selected, originalItemID, virtualItemID)
	}

	result, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return result
}

func (h *Handler) rewriteMediaSource(mediaSource map[string]any, selected *upstream.Upstream, originalItemID, virtualItemID string) {
	virtualMediaSourceID, _ := mediaSource["Id"].(string)
	if virtualMediaSourceID != "" && !isLikelyVirtualID(virtualMediaSourceID) {
		virtualMediaSourceID = h.ids.GetOrCreate(virtualMediaSourceID, selected.ID, "MediaSource")
		mediaSource["Id"] = virtualMediaSourceID
	}

	sourceURL := firstNonEmpty(stringValue(mediaSource["TranscodingUrl"]), stringValue(mediaSource["DirectStreamUrl"]))
	delete(mediaSource, "DirectStreamUrl")
	delete(mediaSource, "Path")
	mediaSource["SupportsDirectPlay"] = false
	mediaSource["SupportsDirectStream"] = true

	if sourceURL != "" {
		if rewritten := h.buildProxyPlaybackURL(sourceURL, originalItemID, virtualItemID, virtualMediaSourceID); rewritten != "" {
			mediaSource["TranscodingUrl"] = rewritten
		} else {
			delete(mediaSource, "TranscodingUrl")
		}
	}
}

func (h *Handler) buildProxyPlaybackURL(rawURL, originalItemID, virtualItemID, virtualMediaSourceID string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	replacements := map[string]string{}
	if originalItemID != "" && virtualItemID != "" {
		replacements[originalItemID] = virtualItemID
	}

	parsed.Scheme = ""
	parsed.Host = ""
	parsed.Path = rewritePlaybackPath(parsed.Path, replacements)

	query := parsed.Query()
	if virtualMediaSourceID != "" && query.Get("MediaSourceId") != "" {
		query.Set("MediaSourceId", virtualMediaSourceID)
	}
	if virtualItemID != "" && query.Get("ItemId") != "" {
		query.Set("ItemId", virtualItemID)
	}
	if query.Get("UserId") != "" {
		query.Set("UserId", h.proxyUserID)
	}
	query.Del("api_key")
	parsed.RawQuery = query.Encode()

	if parsed.Path == "" {
		parsed.Path = "/Videos/" + virtualItemID + "/stream"
	}

	rewritten := parsed.String()
	if strings.HasPrefix(rewritten, "//") {
		rewritten = "/" + strings.TrimPrefix(rewritten, "//")
	}
	if !strings.HasPrefix(rewritten, "/") {
		rewritten = "/" + strings.TrimPrefix(rewritten, "./")
	}
	return rewritten
}

func rewritePlaybackPath(path string, replacements map[string]string) string {
	if len(replacements) == 0 {
		return path
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if replacement, ok := replacements[segment]; ok {
			segments[i] = replacement
		}
	}
	return strings.Join(segments, "/")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func isAllowedRedirectHost(allowed map[string]bool, target *url.URL) bool {
	if target == nil {
		return false
	}
	host := strings.ToLower(target.Host)
	if allowed[host] {
		return true
	}
	hostname := strings.ToLower(target.Hostname())
	return allowed[hostname]
}

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type loginRequest struct {
	Username string
	Password string
}

func normalizeRoutePath(path string) string {
	routePath := strings.TrimSpace(path)
	if strings.HasPrefix(routePath, "/emby/") {
		routePath = strings.TrimPrefix(routePath, "/emby")
	}
	if routePath == "/emby" {
		return "/"
	}
	if routePath == "" {
		return "/"
	}
	return routePath
}

func (h *Handler) handleLoginCompat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取登录请求失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	authFields := parseEmbyAuthorizationHeader(r.Header.Get("X-Emby-Authorization"))
	clientName := firstNonEmpty(r.Header.Get("X-Emby-Client"), authFields["Client"])
	deviceName := firstNonEmpty(r.Header.Get("X-Emby-Device-Name"), authFields["Device"])
	h.log.Debug("登录请求: Method=%s Path=%s ContentType=%s BodyLen=%d", r.Method, r.URL.Path, r.Header.Get("Content-Type"), len(body))
	h.log.Debug("登录头: UA=%s Client=%s Device=%s", r.Header.Get("User-Agent"), clientName, deviceName)

	loginReq, err := parseLoginRequest(body, r.Header.Get("Content-Type"), authFields)
	if err != nil {
		http.Error(w, "登录请求格式错误", http.StatusBadRequest)
		return
	}

	if h.adminAuth == nil || !h.adminAuth.VerifyCredentials(loginReq.Username, loginReq.Password) {
		h.log.Warn("用户 [%s] 登录失败", loginReq.Username)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "用户名或密码错误"})
		return
	}

	loginCtx, loginCancel := h.withTimeout(r.Context(), h.cfg.Timeouts.Login)
	defer loginCancel()

	primary := h.selectPrimaryUpstream()
	session := loginSession{ProxyUserID: h.proxyUserID}
	if primary != nil {
		session.UpstreamID = primary.ID
		session.UpstreamUserID = h.resolveUpstreamUserID(loginCtx, primary)
	}

	token := generateSecureToken()
	h.rememberSession(token, session)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.0000000Z")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"User": map[string]any{
			"Name":                      loginReq.Username,
			"ServerId":                  h.cfg.Server.ID,
			"Id":                        h.proxyUserID,
			"HasPassword":               true,
			"HasConfiguredPassword":     true,
			"HasConfiguredEasyPassword": false,
			"ConnectUserName":           "",
			"ConnectLinkType":           "Guest",
			"DateCreated":               "2024-01-01T00:00:00.0000000Z",
			"LastLoginDate":             now,
			"LastActivityDate":          now,
			"Configuration": map[string]any{
				"PlayDefaultAudioTrack":      true,
				"SubtitleLanguagePreference": "",
				"DisplayMissingEpisodes":     false,
				"SubtitleMode":               "Default",
				"EnableLocalPassword":        false,
			},
			"Policy": map[string]any{
				"IsAdministrator":                true,
				"IsHidden":                       true,
				"IsDisabled":                     false,
				"EnableAllFolders":               true,
				"EnableContentDeletion":          false,
				"EnableRemoteAccess":             true,
				"EnableLiveTvAccess":             true,
				"EnableLiveTvManagement":         false,
				"EnableMediaPlayback":            true,
				"EnableAudioPlaybackTranscoding": true,
				"EnableVideoPlaybackTranscoding": true,
				"EnablePlaybackRemuxing":         true,
				"EnableContentDownloading":       true,
				"EnableSyncTranscoding":          true,
				"EnableSubtitleManagement":       false,
				"InvalidLoginAttemptCount":       0,
			},
		},
		"SessionInfo": map[string]any{
			"UserId":                h.proxyUserID,
			"UserName":              loginReq.Username,
			"ServerId":              h.cfg.Server.ID,
			"DeviceId":              firstNonEmpty(r.Header.Get("X-Emby-Device-Id"), authFields["DeviceId"]),
			"DeviceName":            deviceName,
			"Client":                clientName,
			"ApplicationVersion":    firstNonEmpty(r.Header.Get("X-Emby-Client-Version"), authFields["Version"]),
			"SupportsRemoteControl": true,
			"PlayState": map[string]any{
				"CanSeek":  false,
				"IsPaused": false,
				"IsMuted":  false,
			},
		},
		"AccessToken": token,
		"ServerId":    h.cfg.Server.ID,
	})
	h.log.Info("用户 [%s] 登录成功", loginReq.Username)
}

func parseLoginRequest(body []byte, contentType string, authFields map[string]string) (loginRequest, error) {
	req := loginRequest{}
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	switch contentType {
	case "", "application/json":
		if len(bytes.TrimSpace(body)) > 0 {
			var payload struct {
				Username string `json:"Username"`
				UserName string `json:"UserName"`
				Pw       string `json:"Pw"`
				Password string `json:"Password"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return loginRequest{}, err
			}
			req.Username = firstNonEmpty(payload.Username, payload.UserName)
			req.Password = firstNonEmpty(payload.Pw, payload.Password)
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return loginRequest{}, err
		}
		req.Username = firstNonEmpty(values.Get("Username"), values.Get("username"), values.Get("UserName"))
		req.Password = firstNonEmpty(values.Get("Pw"), values.Get("pw"), values.Get("Password"), values.Get("password"))
	default:
		return loginRequest{}, fmt.Errorf("unsupported login content type")
	}

	req.Username = firstNonEmpty(req.Username, authFields["Username"], authFields["UserName"], authFields["User"])
	req.Password = firstNonEmpty(req.Password, authFields["Pw"], authFields["Password"])
	if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
		return loginRequest{}, fmt.Errorf("missing credentials")
	}

	return loginRequest{
		Username: strings.TrimSpace(req.Username),
		Password: req.Password,
	}, nil
}

func parseEmbyAuthorizationHeader(raw string) map[string]string {
	result := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result
	}

	if idx := strings.IndexByte(raw, ' '); idx >= 0 {
		raw = strings.TrimSpace(raw[idx+1:])
	}

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key != "" {
			result[key] = value
		}
	}

	return result
}

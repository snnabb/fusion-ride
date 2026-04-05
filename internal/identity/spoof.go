package identity

import (
	"net/http"
	"regexp"
	"strings"
)

type Profile struct {
	Name       string
	UserAgent  string
	Client     string
	Version    string
	DeviceName string
	DeviceID   string
}

type Spoofer struct {
	mode    string
	profile Profile
}

var (
	clientFieldPattern   = regexp.MustCompile(`Client="[^"]*"`)
	versionFieldPattern  = regexp.MustCompile(`Version="[^"]*"`)
	deviceFieldPattern   = regexp.MustCompile(`Device="[^"]*"`)
	deviceIDFieldPattern = regexp.MustCompile(`DeviceId="[^"]*"`)
)

var uaProfiles = map[string]Profile{
	"infuse": {
		Name:       "Infuse",
		UserAgent:  "Infuse/7.8.1",
		Client:     "Infuse",
		Version:    "7.8.1",
		DeviceName: "iPhone",
		DeviceID:   "fusionride-infuse",
	},
	"web": {
		Name:       "Web",
		UserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Emby Theater",
		Client:     "Emby Web",
		Version:    "4.9.0.42",
		DeviceName: "Chrome",
		DeviceID:   "fusionride-web",
	},
	"client": {
		Name:       "Client",
		UserAgent:  "Emby-Theater/4.7.0",
		Client:     "Emby Theater",
		Version:    "4.7.0",
		DeviceName: "Windows",
		DeviceID:   "fusionride-client",
	},
}

func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "infuse":
		return "infuse"
	case "web":
		return "web"
	case "client", "passthrough":
		return "client"
	default:
		return "infuse"
	}
}

func NewSpoofer(mode, _, _, _, _, _ string) *Spoofer {
	normalized := NormalizeMode(mode)
	return &Spoofer{
		mode:    normalized,
		profile: uaProfiles[normalized],
	}
}

func (s *Spoofer) Headers() map[string]string {
	return map[string]string{
		"User-Agent":            s.profile.UserAgent,
		"X-Emby-Client":         s.profile.Client,
		"X-Emby-Device-Name":    s.profile.DeviceName,
		"X-Emby-Device-Id":      s.profile.DeviceID,
		"X-Emby-Client-Version": s.profile.Version,
		"X-Emby-Authorization":  s.BuildAuthorizationHeader(),
	}
}

func (s *Spoofer) Mode() string {
	return s.mode
}

func (s *Spoofer) ApplyToHeader(header http.Header) {
	if header == nil {
		return
	}

	incomingXEmbyAuth := header.Get("X-Emby-Authorization")
	incomingAuth := header.Get("Authorization")

	header.Set("User-Agent", s.profile.UserAgent)
	header.Set("X-Emby-Client", s.profile.Client)
	header.Set("X-Emby-Device-Name", s.profile.DeviceName)
	header.Set("X-Emby-Device-Id", s.profile.DeviceID)
	header.Set("X-Emby-Client-Version", s.profile.Version)

	if incomingXEmbyAuth != "" {
		header.Set("X-Emby-Authorization", s.rewriteAuthorization(incomingXEmbyAuth))
	} else {
		header.Set("X-Emby-Authorization", s.BuildAuthorizationHeader())
	}

	if incomingAuth != "" {
		header.Set("Authorization", s.rewriteAuthorization(incomingAuth))
	}
}

func (s *Spoofer) BuildAuthorizationHeader() string {
	parts := []string{
		`MediaBrowser Client="` + s.profile.Client + `"`,
		`Device="` + s.profile.DeviceName + `"`,
		`DeviceId="` + s.profile.DeviceID + `"`,
		`Version="` + s.profile.Version + `"`,
	}
	return strings.Join(parts, ", ")
}

func (s *Spoofer) rewriteAuthorization(value string) string {
	value = replaceOrAppend(value, clientFieldPattern, `Client="`+s.profile.Client+`"`)
	value = replaceOrAppend(value, deviceFieldPattern, `Device="`+s.profile.DeviceName+`"`)
	value = replaceOrAppend(value, deviceIDFieldPattern, `DeviceId="`+s.profile.DeviceID+`"`)
	value = replaceOrAppend(value, versionFieldPattern, `Version="`+s.profile.Version+`"`)
	return value
}

func replaceOrAppend(input string, pattern *regexp.Regexp, replacement string) string {
	if pattern.MatchString(input) {
		return pattern.ReplaceAllString(input, replacement)
	}
	if strings.TrimSpace(input) == "" {
		return replacement
	}
	return input + `, ` + replacement
}

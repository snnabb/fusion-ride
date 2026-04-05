package proxy

import (
	"encoding/json"
	"strings"
)

var idFieldNames = map[string]struct{}{
	"Id":                    {},
	"SeriesId":              {},
	"SeasonId":              {},
	"ParentId":              {},
	"AlbumId":               {},
	"ChannelId":             {},
	"ItemId":                {},
	"MediaSourceId":         {},
	"ProgramId":             {},
	"RecordingId":           {},
	"UserId":                {},
	"ParentBackdropItemId":  {},
	"PrimaryImageItemId":    {},
	"ThumbItemId":           {},
	"LogoItemId":            {},
	"BackdropItemId":        {},
	"ArtistId":              {},
	"SeasonItemId":          {},
	"SeriesPrimaryImageTag": {},
}

var idArrayFieldNames = map[string]struct{}{
	"AncestorIds":       {},
	"AdditionalPartIds": {},
	"ItemIds":           {},
}

func (h *Handler) virtualizeJSONBytes(data []byte, serverID int) []byte {
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return data
	}

	walkJSONIDs(parsed, "", func(value string) (string, bool) {
		if value == "" {
			return "", false
		}
		if value == h.proxyUserID {
			return value, true
		}
		return h.ids.GetOrCreate(value, serverID, ""), true
	})

	result, err := json.Marshal(parsed)
	if err != nil {
		return data
	}
	return result
}

func (h *Handler) devirtualizeJSONBytes(data []byte, upstreamID int) []byte {
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return data
	}

	walkJSONIDs(parsed, "", func(value string) (string, bool) {
		return h.originalIDForUpstream(value, upstreamID)
	})

	result, err := json.Marshal(parsed)
	if err != nil {
		return data
	}
	return result
}

func (h *Handler) rewriteProxyUserIdentityJSON(data []byte, fromID, toID, serverID string) []byte {
	if len(data) == 0 || fromID == "" || toID == "" {
		return data
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return data
	}

	walkProxyUserIdentity(parsed, fromID, toID, serverID)

	result, err := json.Marshal(parsed)
	if err != nil {
		return data
	}
	return result
}

func walkJSONIDs(data any, parentKey string, rewrite func(value string) (string, bool)) {
	switch v := data.(type) {
	case map[string]any:
		for key, value := range v {
			switch typed := value.(type) {
			case string:
				if shouldRewriteScalarKey(key) {
					if rewritten, ok := rewrite(typed); ok {
						v[key] = rewritten
					}
				}
			case []any:
				if shouldRewriteArrayKey(key) {
					for i, element := range typed {
						text, ok := element.(string)
						if !ok {
							walkJSONIDs(element, key, rewrite)
							continue
						}
						if rewritten, ok := rewrite(text); ok {
							typed[i] = rewritten
						}
					}
				} else {
					walkJSONIDs(typed, key, rewrite)
				}
			default:
				walkJSONIDs(typed, key, rewrite)
			}
		}
	case []any:
		for _, value := range v {
			walkJSONIDs(value, parentKey, rewrite)
		}
	}
}

func walkProxyUserIdentity(data any, fromID, toID, serverID string) {
	switch v := data.(type) {
	case map[string]any:
		for key, value := range v {
			switch typed := value.(type) {
			case string:
				if shouldRewriteUserKey(key) && typed == fromID {
					v[key] = toID
					continue
				}
				if key == "ServerId" && serverID != "" {
					v[key] = serverID
					continue
				}
			}
			walkProxyUserIdentity(value, fromID, toID, serverID)
		}
	case []any:
		for _, value := range v {
			walkProxyUserIdentity(value, fromID, toID, serverID)
		}
	}
}

func shouldRewriteScalarKey(key string) bool {
	if _, ok := idFieldNames[key]; ok {
		return true
	}
	return strings.HasSuffix(key, "ItemId") || strings.HasSuffix(key, "SeriesId") || strings.HasSuffix(key, "SeasonId")
}

func shouldRewriteArrayKey(key string) bool {
	if _, ok := idArrayFieldNames[key]; ok {
		return true
	}
	return strings.HasSuffix(key, "Ids")
}

func shouldRewriteUserKey(key string) bool {
	return key == "Id" || key == "UserId" || strings.HasSuffix(key, "UserId")
}

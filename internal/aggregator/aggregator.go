package aggregator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/upstream"
)

type Aggregator struct {
	upMgr   *upstream.Manager
	idStore *idmap.Store
	log     *logger.Logger
	timeout time.Duration

	codecPriority map[string]int
}

type EmbyItemsResponse struct {
	Items            []json.RawMessage `json:"Items"`
	TotalRecordCount int               `json:"TotalRecordCount"`
}

type EmbyItem struct {
	Raw          json.RawMessage
	ID           string
	Name         string
	Type         string
	ProviderIDs  map[string]string
	MediaSources []MediaSource
	ServerID     int
	VirtualID    string
}

type MediaSource struct {
	Raw           json.RawMessage
	ID            string
	Bitrate       int
	Width         int
	Height        int
	VideoCodec    string
	AudioChannels int
	Size          int64
	ServerID      int
	OriginalID    string
}

func New(upMgr *upstream.Manager, idStore *idmap.Store, log *logger.Logger, timeout time.Duration, codecPriority []string) *Aggregator {
	priority := make(map[string]int, len(codecPriority))
	for i, codec := range codecPriority {
		priority[strings.ToLower(codec)] = len(codecPriority) - i
	}

	return &Aggregator{
		upMgr:         upMgr,
		idStore:       idStore,
		log:           log,
		timeout:       timeout,
		codecPriority: priority,
	}
}

func (a *Aggregator) AggregateItems(ctx context.Context, path string) ([]byte, error) {
	onlineUpstreams := a.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return json.Marshal(EmbyItemsResponse{Items: []json.RawMessage{}, TotalRecordCount: 0})
	}

	ctx, cancel := a.aggregateContext(ctx)
	defer cancel()

	type result struct {
		items    []EmbyItem
		serverID int
		err      error
	}

	results := make(chan result, len(onlineUpstreams))
	var wg sync.WaitGroup

	for _, upstreamInstance := range onlineUpstreams {
		upstreamPath, ok := a.rewritePathForUpstream(path, upstreamInstance)
		if !ok {
			continue
		}

		wg.Add(1)
		go func(upstreamInstance *upstream.Upstream, upstreamPath string) {
			defer wg.Done()

			resp, err := upstreamInstance.DoAPI(ctx, http.MethodGet, upstreamPath, nil)
			if err != nil {
				select {
				case results <- result{serverID: upstreamInstance.ID, err: err}:
				case <-ctx.Done():
				}
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				select {
				case results <- result{serverID: upstreamInstance.ID, err: err}:
				case <-ctx.Done():
				}
				return
			}

			select {
			case results <- result{items: a.parseItems(body, upstreamInstance.ID), serverID: upstreamInstance.ID}:
			case <-ctx.Done():
			}
		}(upstreamInstance, upstreamPath)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allItems []EmbyItem
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("聚合请求已取消: %w", ctx.Err())
		case result, ok := <-results:
			if !ok {
				return a.encodeItems(allItems)
			}
			if result.err != nil {
				a.log.Warn("上游 %d 聚合失败: %v", result.serverID, result.err)
				continue
			}
			allItems = append(allItems, result.items...)
		}
	}
}

func (a *Aggregator) AggregateSearch(ctx context.Context, path string) ([]byte, error) {
	onlineUpstreams := a.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return json.Marshal(map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
	}

	ctx, cancel := a.aggregateContext(ctx)
	defer cancel()

	type searchResult struct {
		hints    []json.RawMessage
		serverID int
		err      error
	}

	results := make(chan searchResult, len(onlineUpstreams))
	var wg sync.WaitGroup

	for _, upstreamInstance := range onlineUpstreams {
		upstreamPath, ok := a.rewritePathForUpstream(path, upstreamInstance)
		if !ok {
			continue
		}

		wg.Add(1)
		go func(upstreamInstance *upstream.Upstream, upstreamPath string) {
			defer wg.Done()

			resp, err := upstreamInstance.DoAPI(ctx, http.MethodGet, upstreamPath, nil)
			if err != nil {
				select {
				case results <- searchResult{serverID: upstreamInstance.ID, err: err}:
				case <-ctx.Done():
				}
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				select {
				case results <- searchResult{serverID: upstreamInstance.ID, err: err}:
				case <-ctx.Done():
				}
				return
			}

			var parsed struct {
				SearchHints []json.RawMessage `json:"SearchHints"`
			}
			_ = json.Unmarshal(body, &parsed)
			for i, hint := range parsed.SearchHints {
				parsed.SearchHints[i] = a.virtualizeRawItem(hint, upstreamInstance.ID)
			}

			select {
			case results <- searchResult{hints: parsed.SearchHints, serverID: upstreamInstance.ID}:
			case <-ctx.Done():
			}
		}(upstreamInstance, upstreamPath)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allHints []json.RawMessage
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("搜索聚合已取消: %w", ctx.Err())
		case result, ok := <-results:
			if !ok {
				return json.Marshal(map[string]any{
					"SearchHints":      allHints,
					"TotalRecordCount": len(allHints),
				})
			}
			if result.err != nil {
				a.log.Warn("上游 %d 搜索失败: %v", result.serverID, result.err)
				continue
			}
			allHints = append(allHints, result.hints...)
		}
	}
}

func (a *Aggregator) AggregateSingleItem(ctx context.Context, virtualID string) ([]byte, error) {
	originalID, serverID, ok := a.idStore.Resolve(virtualID)
	if !ok {
		return nil, fmt.Errorf("虚拟 ID %s 不存在", virtualID)
	}

	selected := a.upMgr.ByID(serverID)
	if selected == nil {
		return nil, fmt.Errorf("上游 %d 不存在", serverID)
	}

	ctx, cancel := a.aggregateContext(ctx)
	defer cancel()

	userID := selected.GetUserID()

	var (
		resp *http.Response
		err  error
	)
	if userID == "" {
		resp, err = selected.DoAPI(ctx, http.MethodGet, fmt.Sprintf("/Items/%s?Fields=MediaSources,ProviderIds", originalID), nil)
	} else {
		resp, err = selected.DoAPI(ctx, http.MethodGet, fmt.Sprintf("/Users/%s/Items/%s", userID, originalID), nil)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	instances := a.idStore.GetInstances(virtualID)
	if len(instances) > 1 {
		body = a.mergeMediaSources(ctx, body, instances, serverID)
	}

	return a.virtualizeRawBytes(body, serverID), nil
}

func (a *Aggregator) aggregateContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if a.timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, a.timeout)
}

func (a *Aggregator) encodeItems(items []EmbyItem) ([]byte, error) {
	merged := a.dedup(items)

	finalItems := make([]json.RawMessage, 0, len(merged))
	for _, item := range merged {
		finalItems = append(finalItems, a.virtualizeItem(item))
	}

	return json.Marshal(EmbyItemsResponse{
		Items:            finalItems,
		TotalRecordCount: len(finalItems),
	})
}

func (a *Aggregator) parseItems(data []byte, serverID int) []EmbyItem {
	var response EmbyItemsResponse
	if err := json.Unmarshal(data, &response); err != nil {
		var singleItem json.RawMessage
		if json.Unmarshal(data, &singleItem) == nil {
			return []EmbyItem{a.parseOneItem(singleItem, serverID)}
		}
		return nil
	}

	items := make([]EmbyItem, 0, len(response.Items))
	for _, raw := range response.Items {
		items = append(items, a.parseOneItem(raw, serverID))
	}
	return items
}

func (a *Aggregator) parseOneItem(raw json.RawMessage, serverID int) EmbyItem {
	var parsed struct {
		ID           string            `json:"Id"`
		Name         string            `json:"Name"`
		Type         string            `json:"Type"`
		ProviderIDs  map[string]string `json:"ProviderIds"`
		MediaSources []struct {
			ID           string `json:"Id"`
			Bitrate      int    `json:"Bitrate"`
			Size         int64  `json:"Size"`
			MediaStreams []struct {
				Type     string `json:"Type"`
				Codec    string `json:"Codec"`
				Width    int    `json:"Width"`
				Height   int    `json:"Height"`
				Channels int    `json:"Channels"`
				BitRate  int    `json:"BitRate"`
			} `json:"MediaStreams"`
		} `json:"MediaSources"`
	}
	_ = json.Unmarshal(raw, &parsed)

	item := EmbyItem{
		Raw:         raw,
		ID:          parsed.ID,
		Name:        parsed.Name,
		Type:        parsed.Type,
		ProviderIDs: parsed.ProviderIDs,
		ServerID:    serverID,
	}

	for _, mediaSource := range parsed.MediaSources {
		source := MediaSource{
			ID:         mediaSource.ID,
			Bitrate:    mediaSource.Bitrate,
			Size:       mediaSource.Size,
			ServerID:   serverID,
			OriginalID: parsed.ID,
		}

		for _, stream := range mediaSource.MediaStreams {
			switch stream.Type {
			case "Video":
				source.VideoCodec = strings.ToLower(stream.Codec)
				source.Width = stream.Width
				source.Height = stream.Height
				if source.Bitrate == 0 {
					source.Bitrate = stream.BitRate
				}
			case "Audio":
				source.AudioChannels = stream.Channels
			}
		}

		item.MediaSources = append(item.MediaSources, source)
	}

	return item
}

func (a *Aggregator) dedup(items []EmbyItem) []EmbyItem {
	groups := make(map[string][]EmbyItem)
	for _, item := range items {
		key := a.buildDedupKey(item)
		groups[key] = append(groups[key], item)
	}

	merged := make([]EmbyItem, 0, len(groups))
	for _, group := range groups {
		if len(group) == 1 {
			merged = append(merged, group[0])
			continue
		}

		primary := a.selectPrimary(group)
		primaryVirtualID := a.idStore.GetOrCreate(primary.ID, primary.ServerID, primary.Type)

		for _, item := range group {
			if item.ServerID == primary.ServerID && item.ID == primary.ID {
				continue
			}

			primary.MediaSources = append(primary.MediaSources, item.MediaSources...)
			_ = a.idStore.AddInstance(primaryVirtualID, item.ID, item.ServerID, maxBitrate(item.MediaSources))
		}

		a.sortMediaSources(primary.MediaSources)
		merged = append(merged, primary)
	}

	return merged
}

func (a *Aggregator) buildDedupKey(item EmbyItem) string {
	if item.ProviderIDs != nil {
		if tmdb := item.ProviderIDs["Tmdb"]; tmdb != "" {
			return "tmdb:" + tmdb + ":" + item.Type
		}
		if imdb := item.ProviderIDs["Imdb"]; imdb != "" {
			return "imdb:" + imdb + ":" + item.Type
		}
		if tvdb := item.ProviderIDs["Tvdb"]; tvdb != "" {
			return "tvdb:" + tvdb + ":" + item.Type
		}
	}
	return "name:" + strings.ToLower(item.Name) + ":" + item.Type
}

func (a *Aggregator) selectPrimary(group []EmbyItem) EmbyItem {
	best := group[0]

	for _, item := range group[1:] {
		if a.upMgr != nil {
			if upstreamInstance := a.upMgr.ByID(item.ServerID); upstreamInstance != nil && upstreamInstance.PriorityMeta {
				best = item
				break
			}
		}

		if containsChinese(item.Name) && !containsChinese(best.Name) {
			best = item
		}
	}

	return best
}

func (a *Aggregator) sortMediaSources(sources []MediaSource) {
	sort.Slice(sources, func(i, j int) bool {
		left, right := sources[i], sources[j]
		if left.Bitrate != right.Bitrate {
			return left.Bitrate > right.Bitrate
		}

		leftResolution := left.Width * left.Height
		rightResolution := right.Width * right.Height
		if leftResolution != rightResolution {
			return leftResolution > rightResolution
		}

		leftPriority := a.codecPriority[left.VideoCodec]
		rightPriority := a.codecPriority[right.VideoCodec]
		if leftPriority != rightPriority {
			return leftPriority > rightPriority
		}

		if left.AudioChannels != right.AudioChannels {
			return left.AudioChannels > right.AudioChannels
		}

		return left.Size > right.Size
	})
}

func (a *Aggregator) virtualizeItem(item EmbyItem) json.RawMessage {
	var parsed any
	if err := json.Unmarshal(item.Raw, &parsed); err != nil {
		return item.Raw
	}

	walkAndVirtualize(parsed, []string{"Id", "SeriesId", "SeasonId", "ParentId", "AlbumId", "ChannelId", "ItemId"}, item.ServerID, a.idStore)

	result, err := json.Marshal(parsed)
	if err != nil {
		return item.Raw
	}
	return json.RawMessage(result)
}

func (a *Aggregator) virtualizeRawItem(raw json.RawMessage, serverID int) json.RawMessage {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return raw
	}

	walkAndVirtualize(parsed, []string{"Id", "SeriesId", "SeasonId", "ParentId", "AlbumId", "ChannelId", "ItemId"}, serverID, a.idStore)

	result, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	return json.RawMessage(result)
}

func (a *Aggregator) virtualizeRawBytes(data []byte, serverID int) []byte {
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		return data
	}

	walkAndVirtualize(parsed, []string{"Id", "SeriesId", "SeasonId", "ParentId", "AlbumId", "ChannelId", "ItemId"}, serverID, a.idStore)

	result, err := json.Marshal(parsed)
	if err != nil {
		return data
	}

	return result
}

func (a *Aggregator) mergeMediaSources(ctx context.Context, mainBody []byte, instances []idmap.Instance, mainServerID int) []byte {
	var mainParsed map[string]any
	if json.Unmarshal(mainBody, &mainParsed) != nil {
		return mainBody
	}

	existingSources, _ := mainParsed["MediaSources"].([]any)
	for _, instance := range instances {
		if instance.ServerID == mainServerID {
			continue
		}

		upstreamInstance := a.upMgr.ByID(instance.ServerID)
		if upstreamInstance == nil {
			continue
		}

		resp, err := upstreamInstance.DoAPI(ctx, http.MethodGet, fmt.Sprintf("/Items/%s", instance.OriginalID), nil)
		if err != nil {
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var remoteParsed struct {
			MediaSources []any `json:"MediaSources"`
		}
		if json.Unmarshal(body, &remoteParsed) == nil {
			existingSources = append(existingSources, remoteParsed.MediaSources...)
		}
	}

	mainParsed["MediaSources"] = existingSources
	result, _ := json.Marshal(mainParsed)
	return result
}

func (a *Aggregator) rewritePathForUpstream(rawPath string, upstreamInstance *upstream.Upstream) (string, bool) {
	parsed, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return rawPath, true
	}

	segments := strings.Split(parsed.Path, "/")
	if len(segments) > 2 && segments[1] == "Users" {
		if userID := upstreamInstance.GetUserID(); userID != "" {
			segments[2] = userID
		}
	}

	for i, segment := range segments {
		if !isLikelyVirtualID(segment) {
			continue
		}

		originalID, ok := a.originalIDForUpstream(segment, upstreamInstance.ID)
		if !ok {
			return "", false
		}
		segments[i] = originalID
	}

	parsed.Path = strings.Join(segments, "/")
	return parsed.RequestURI(), true
}

func (a *Aggregator) originalIDForUpstream(virtualID string, upstreamID int) (string, bool) {
	originalID, serverID, ok := a.idStore.Resolve(virtualID)
	if !ok {
		return "", false
	}
	if serverID == upstreamID {
		return originalID, true
	}

	for _, instance := range a.idStore.GetInstances(virtualID) {
		if instance.ServerID == upstreamID {
			return instance.OriginalID, true
		}
	}

	return "", false
}

func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func maxBitrate(sources []MediaSource) int {
	max := 0
	for _, source := range sources {
		if source.Bitrate > max {
			max = source.Bitrate
		}
	}
	return max
}

func walkAndVirtualize(data any, idFields []string, serverID int, store *idmap.Store) {
	switch v := data.(type) {
	case map[string]any:
		for _, field := range idFields {
			if id, ok := v[field].(string); ok && id != "" {
				v[field] = store.GetOrCreate(id, serverID, "")
			}
		}
		for _, value := range v {
			walkAndVirtualize(value, idFields, serverID, store)
		}
	case []any:
		for _, value := range v {
			walkAndVirtualize(value, idFields, serverID, store)
		}
	}
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

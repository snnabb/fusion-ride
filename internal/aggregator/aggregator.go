package aggregator

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fusionride/fusion-ride/internal/idmap"
	"github.com/fusionride/fusion-ride/internal/logger"
	"github.com/fusionride/fusion-ride/internal/upstream"
)

// Aggregator 负责并发请求多个上游并合并响应。
type Aggregator struct {
	upMgr   *upstream.Manager
	idStore *idmap.Store
	log     *logger.Logger
	timeout time.Duration

	// 码率排序
	codecPriority map[string]int
}

// New 创建聚合器实例。
func New(upMgr *upstream.Manager, idStore *idmap.Store, log *logger.Logger,
	timeout time.Duration, codecPriority []string) *Aggregator {

	cp := make(map[string]int)
	for i, c := range codecPriority {
		cp[strings.ToLower(c)] = len(codecPriority) - i
	}

	return &Aggregator{
		upMgr:         upMgr,
		idStore:       idStore,
		log:           log,
		timeout:       timeout,
		codecPriority: cp,
	}
}

// EmbyItemsResponse 是 Emby /Items 等端点的响应格式。
type EmbyItemsResponse struct {
	Items            []json.RawMessage `json:"Items"`
	TotalRecordCount int               `json:"TotalRecordCount"`
}

// EmbyItem 是解析后的 Emby 媒体条目。
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

// MediaSource 是 Emby 媒体源信息（用于码率排序）。
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

// AggregateItems 并发请求所有在线上游的 Items 端点，合并去重后返回。
func (a *Aggregator) AggregateItems(path string) ([]byte, error) {
	onlineUpstreams := a.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return json.Marshal(EmbyItemsResponse{Items: []json.RawMessage{}, TotalRecordCount: 0})
	}

	type result struct {
		items    []EmbyItem
		serverID int
		err      error
	}

	results := make(chan result, len(onlineUpstreams))
	var wg sync.WaitGroup

	// 并发请求所有上游
	for _, u := range onlineUpstreams {
		wg.Add(1)
		go func(u *upstream.Upstream) {
			defer wg.Done()

			resp, err := u.DoAPI("GET", path, nil)
			if err != nil {
				results <- result{err: err, serverID: u.ID}
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- result{err: err, serverID: u.ID}
				return
			}

			items := a.parseItems(body, u.ID)
			results <- result{items: items, serverID: u.ID}
		}(u)
	}

	// 超时等待
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	var allItems []EmbyItem
	timeout := time.After(a.timeout)

	collecting := true
	for collecting {
		select {
		case r := <-results:
			if r.err != nil {
				a.log.Warn("上游 %d 请求失败: %v", r.serverID, r.err)
			} else {
				allItems = append(allItems, r.items...)
			}
		case <-done:
			collecting = false
		case <-timeout:
			a.log.Warn("聚合超时，返回部分结果 (%d 条)", len(allItems))
			collecting = false
		}
	}

	// 排空 channel
	for {
		select {
		case r := <-results:
			if r.err == nil {
				allItems = append(allItems, r.items...)
			}
		default:
			goto DEDUP
		}
	}

DEDUP:
	// 去重 + 合并
	merged := a.dedup(allItems)

	// 虚拟化 ID + 码率排序
	finalItems := make([]json.RawMessage, 0, len(merged))
	for _, item := range merged {
		rewritten := a.virtualizeItem(item)
		finalItems = append(finalItems, rewritten)
	}

	response := EmbyItemsResponse{
		Items:            finalItems,
		TotalRecordCount: len(finalItems),
	}

	return json.Marshal(response)
}

// AggregateSearch 聚合搜索。
func (a *Aggregator) AggregateSearch(path string) ([]byte, error) {
	onlineUpstreams := a.upMgr.Online()
	if len(onlineUpstreams) == 0 {
		return json.Marshal(map[string]any{"SearchHints": []any{}, "TotalRecordCount": 0})
	}

	type searchResult struct {
		hints    []json.RawMessage
		serverID int
		err      error
	}

	results := make(chan searchResult, len(onlineUpstreams))
	var wg sync.WaitGroup

	for _, u := range onlineUpstreams {
		wg.Add(1)
		go func(u *upstream.Upstream) {
			defer wg.Done()

			resp, err := u.DoAPI("GET", path, nil)
			if err != nil {
				results <- searchResult{err: err, serverID: u.ID}
				return
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)

			var parsed struct {
				SearchHints []json.RawMessage `json:"SearchHints"`
			}
			json.Unmarshal(body, &parsed)

			// ID 虚拟化
			for i, hint := range parsed.SearchHints {
				parsed.SearchHints[i] = a.virtualizeRawItem(hint, u.ID)
			}

			results <- searchResult{hints: parsed.SearchHints, serverID: u.ID}
		}(u)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allHints []json.RawMessage
	for r := range results {
		if r.err == nil {
			allHints = append(allHints, r.hints...)
		}
	}

	return json.Marshal(map[string]any{
		"SearchHints":      allHints,
		"TotalRecordCount": len(allHints),
	})
}

// AggregateSingleItem 获取单个条目详情，合并所有上游的 MediaSources。
func (a *Aggregator) AggregateSingleItem(virtualID string) ([]byte, error) {
	origID, serverID, ok := a.idStore.Resolve(virtualID)
	if !ok {
		return nil, fmt.Errorf("虚拟 ID %s 未找到", virtualID)
	}

	// 获取主实例
	u := a.upMgr.ByID(serverID)
	if u == nil {
		return nil, fmt.Errorf("服务器 %d 不存在", serverID)
	}

	resp, err := u.DoAPI("GET", fmt.Sprintf("/Users/{uid}/Items/%s", origID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// 获取其他实例的 MediaSources
	instances := a.idStore.GetInstances(virtualID)
	if len(instances) > 1 {
		body = a.mergeMediaSources(body, instances, serverID)
	}

	// 虚拟化
	body = a.virtualizeRawBytes(body, serverID)

	return body, nil
}

// ── 内部方法 ──

func (a *Aggregator) parseItems(data []byte, serverID int) []EmbyItem {
	var resp EmbyItemsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		// 可能是单个 item
		var singleItem json.RawMessage
		if json.Unmarshal(data, &singleItem) == nil {
			return []EmbyItem{a.parseOneItem(singleItem, serverID)}
		}
		return nil
	}

	items := make([]EmbyItem, 0, len(resp.Items))
	for _, raw := range resp.Items {
		items = append(items, a.parseOneItem(raw, serverID))
	}
	return items
}

func (a *Aggregator) parseOneItem(raw json.RawMessage, serverID int) EmbyItem {
	var parsed struct {
		ID          string            `json:"Id"`
		Name        string            `json:"Name"`
		Type        string            `json:"Type"`
		ProviderIds map[string]string `json:"ProviderIds"`
		MediaSources []struct {
			ID            string `json:"Id"`
			Bitrate       int    `json:"Bitrate"`
			Size          int64  `json:"Size"`
			MediaStreams   []struct {
				Type          string `json:"Type"`
				Codec         string `json:"Codec"`
				Width         int    `json:"Width"`
				Height        int    `json:"Height"`
				Channels      int    `json:"Channels"`
				BitRate       int    `json:"BitRate"`
			} `json:"MediaStreams"`
		} `json:"MediaSources"`
	}
	json.Unmarshal(raw, &parsed)

	item := EmbyItem{
		Raw:         raw,
		ID:          parsed.ID,
		Name:        parsed.Name,
		Type:        parsed.Type,
		ProviderIDs: parsed.ProviderIds,
		ServerID:    serverID,
	}

	// 解析 MediaSources
	for _, ms := range parsed.MediaSources {
		source := MediaSource{
			ID:         ms.ID,
			Bitrate:    ms.Bitrate,
			Size:       ms.Size,
			ServerID:   serverID,
			OriginalID: parsed.ID,
		}

		for _, stream := range ms.MediaStreams {
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

// dedup 对条目进行智能去重。
func (a *Aggregator) dedup(items []EmbyItem) []EmbyItem {
	type dedupKey struct {
		TMDB string
		IMDB string
		TVDB string
		Name string
		Type string
	}

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

		// 合并同一影片的多个版本
		primary := a.selectPrimary(group)

		// 将其他实例的 MediaSources 合并到主实例
		for _, item := range group {
			if item.ServerID == primary.ServerID && item.ID == primary.ID {
				continue
			}
			primary.MediaSources = append(primary.MediaSources, item.MediaSources...)

			// 记录多实例关系
			a.idStore.AddInstance(
				a.idStore.GetOrCreate(primary.ID, primary.ServerID, primary.Type),
				item.ID,
				item.ServerID,
				maxBitrate(item.MediaSources),
			)
		}

		// 按码率排序所有 MediaSources
		a.sortMediaSources(primary.MediaSources)
		merged = append(merged, primary)
	}

	return merged
}

func (a *Aggregator) buildDedupKey(item EmbyItem) string {
	// 优先使用 TMDB/IMDB/TVDB ID 去重
	if item.ProviderIDs != nil {
		if tmdb, ok := item.ProviderIDs["Tmdb"]; ok && tmdb != "" {
			return "tmdb:" + tmdb + ":" + item.Type
		}
		if imdb, ok := item.ProviderIDs["Imdb"]; ok && imdb != "" {
			return "imdb:" + imdb + ":" + item.Type
		}
		if tvdb, ok := item.ProviderIDs["Tvdb"]; ok && tvdb != "" {
			return "tvdb:" + tvdb + ":" + item.Type
		}
	}

	// 降级：使用名称 + 类型
	return "name:" + strings.ToLower(item.Name) + ":" + item.Type
}

// selectPrimary 选择主实例（元数据优先级）。
func (a *Aggregator) selectPrimary(group []EmbyItem) EmbyItem {
	best := group[0]

	for _, item := range group[1:] {
		u := a.upMgr.ByID(item.ServerID)
		if u != nil && u.PriorityMeta {
			best = item
			break
		}

		// 有中文名优先
		if containsChinese(item.Name) && !containsChinese(best.Name) {
			best = item
		}
	}

	return best
}

// sortMediaSources 按码率优先策略排序。
func (a *Aggregator) sortMediaSources(sources []MediaSource) {
	sort.Slice(sources, func(i, j int) bool {
		si, sj := sources[i], sources[j]

		// 1. 码率降序
		if si.Bitrate != sj.Bitrate {
			return si.Bitrate > sj.Bitrate
		}

		// 2. 分辨率降序
		resI := si.Width * si.Height
		resJ := sj.Width * sj.Height
		if resI != resJ {
			return resI > resJ
		}

		// 3. 编码优先级
		cpI := a.codecPriority[si.VideoCodec]
		cpJ := a.codecPriority[sj.VideoCodec]
		if cpI != cpJ {
			return cpI > cpJ
		}

		// 4. 音频声道数降序
		if si.AudioChannels != sj.AudioChannels {
			return si.AudioChannels > sj.AudioChannels
		}

		// 5. 文件大小降序（越大通常质量越好）
		return si.Size > sj.Size
	})
}

func (a *Aggregator) virtualizeItem(item EmbyItem) json.RawMessage {
	vid := a.idStore.GetOrCreate(item.ID, item.ServerID, item.Type)
	item.VirtualID = vid

	// 替换 JSON 中的 ID
	result := strings.ReplaceAll(string(item.Raw), `"`+item.ID+`"`, `"`+vid+`"`)
	return json.RawMessage(result)
}

func (a *Aggregator) virtualizeRawItem(raw json.RawMessage, serverID int) json.RawMessage {
	var parsed struct {
		ID string `json:"Id"`
	}
	json.Unmarshal(raw, &parsed)
	if parsed.ID == "" {
		return raw
	}

	vid := a.idStore.GetOrCreate(parsed.ID, serverID, "")
	result := strings.ReplaceAll(string(raw), `"`+parsed.ID+`"`, `"`+vid+`"`)
	return json.RawMessage(result)
}

func (a *Aggregator) virtualizeRawBytes(data []byte, serverID int) []byte {
	// 提取所有可能的 ID 并替换
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) != nil {
		return data
	}

	ids := extractIDs(parsed)
	result := string(data)
	for _, id := range ids {
		vid := a.idStore.GetOrCreate(id, serverID, "")
		if vid != id {
			result = strings.ReplaceAll(result, `"`+id+`"`, `"`+vid+`"`)
		}
	}
	return []byte(result)
}

func (a *Aggregator) mergeMediaSources(mainBody []byte, instances []idmap.Instance, mainServerID int) []byte {
	// 简化实现：获取其他实例的 MediaSources 并合并到主体 JSON
	var mainParsed map[string]any
	if json.Unmarshal(mainBody, &mainParsed) != nil {
		return mainBody
	}

	existingSources, _ := mainParsed["MediaSources"].([]any)

	for _, inst := range instances {
		if inst.ServerID == mainServerID {
			continue
		}

		u := a.upMgr.ByID(inst.ServerID)
		if u == nil {
			continue
		}

		resp, err := u.DoAPI("GET", fmt.Sprintf("/Items/%s", inst.OriginalID), nil)
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

// ── 辅助函数 ──

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
	for _, s := range sources {
		if s.Bitrate > max {
			max = s.Bitrate
		}
	}
	return max
}

func extractIDs(data map[string]any) []string {
	var ids []string
	if id, ok := data["Id"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	if id, ok := data["SeriesId"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	if id, ok := data["SeasonId"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	if id, ok := data["ParentId"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	if id, ok := data["AlbumId"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	return ids
}

package idmap

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/fusionride/fusion-ride/internal/db"
)

// Store 管理虚拟 ID ↔ 原始 ID 的双向映射。
type Store struct {
	mu    sync.RWMutex
	db    *db.DB
	cache map[string]*Mapping   // virtualID → Mapping
	rmap  map[string]string     // "originalID:serverID" → virtualID
}

// Mapping 一个虚拟 ID 的完整映射关系。
type Mapping struct {
	VirtualID  string
	OriginalID string
	ServerID   int
	ItemType   string
	Instances  []Instance // 同一影片在其他服务器的实例
}

// Instance 同一影片在另一服务器的实例。
type Instance struct {
	OriginalID string
	ServerID   int
	Bitrate    int
}

// NewStore 创建 ID 映射存储。
func NewStore(database *db.DB) *Store {
	s := &Store{
		db:    database,
		cache: make(map[string]*Mapping, 4096),
		rmap:  make(map[string]string, 4096),
	}
	s.loadCache()
	return s
}

// GetOrCreate 获取或创建虚拟 ID 映射。
func (s *Store) GetOrCreate(originalID string, serverID int, itemType string) string {
	key := fmt.Sprintf("%s:%d", originalID, serverID)

	s.mu.RLock()
	if vid, ok := s.rmap[key]; ok {
		s.mu.RUnlock()
		return vid
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 双重检查
	if vid, ok := s.rmap[key]; ok {
		return vid
	}

	vid := generateVirtualID()

	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO id_mapping(virtual_id, original_id, server_id, item_type) VALUES(?, ?, ?, ?)`,
		vid, originalID, serverID, itemType,
	)
	if err != nil {
		// 可能并发插入，查询已有的
		var existing string
		s.db.QueryRow(
			`SELECT virtual_id FROM id_mapping WHERE original_id = ? AND server_id = ?`,
			originalID, serverID,
		).Scan(&existing)
		if existing != "" {
			s.rmap[key] = existing
			return existing
		}
		return originalID // 降级：使用原始 ID
	}

	m := &Mapping{
		VirtualID:  vid,
		OriginalID: originalID,
		ServerID:   serverID,
		ItemType:   itemType,
	}
	s.cache[vid] = m
	s.rmap[key] = vid

	return vid
}

// Resolve 将虚拟 ID 解析为原始 ID 和服务器 ID。
func (s *Store) Resolve(virtualID string) (originalID string, serverID int, ok bool) {
	s.mu.RLock()
	m, exists := s.cache[virtualID]
	s.mu.RUnlock()

	if exists {
		return m.OriginalID, m.ServerID, true
	}

	// 缓存未命中，查数据库
	var origID string
	var srvID int
	err := s.db.QueryRow(
		`SELECT original_id, server_id FROM id_mapping WHERE virtual_id = ?`,
		virtualID,
	).Scan(&origID, &srvID)

	if err != nil {
		return "", 0, false
	}

	// 写入缓存
	s.mu.Lock()
	s.cache[virtualID] = &Mapping{
		VirtualID:  virtualID,
		OriginalID: origID,
		ServerID:   srvID,
	}
	s.rmap[fmt.Sprintf("%s:%d", origID, srvID)] = virtualID
	s.mu.Unlock()

	return origID, srvID, true
}

// AddInstance 为虚拟 ID 添加另一个服务器实例（同一影片的不同来源）。
func (s *Store) AddInstance(virtualID, originalID string, serverID, bitrate int) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO id_instances(virtual_id, original_id, server_id, bitrate) VALUES(?, ?, ?, ?)`,
		virtualID, originalID, serverID, bitrate,
	)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if m, ok := s.cache[virtualID]; ok {
		// 检查是否已存在
		found := false
		for i, inst := range m.Instances {
			if inst.ServerID == serverID {
				m.Instances[i].OriginalID = originalID
				m.Instances[i].Bitrate = bitrate
				found = true
				break
			}
		}
		if !found {
			m.Instances = append(m.Instances, Instance{
				OriginalID: originalID,
				ServerID:   serverID,
				Bitrate:    bitrate,
			})
		}
	}
	s.mu.Unlock()

	return nil
}

// GetInstances 获取虚拟 ID 的所有实例（包含主实例）。
func (s *Store) GetInstances(virtualID string) []Instance {
	s.mu.RLock()
	m, ok := s.cache[virtualID]
	s.mu.RUnlock()

	if ok && len(m.Instances) > 0 {
		instances := []Instance{{
			OriginalID: m.OriginalID,
			ServerID:   m.ServerID,
		}}
		instances = append(instances, m.Instances...)
		return instances
	}

	// 从数据库加载
	rows, err := s.db.Query(
		`SELECT original_id, server_id, bitrate FROM id_instances WHERE virtual_id = ?`,
		virtualID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var instances []Instance

	// 主实例
	origID, srvID, exists := s.Resolve(virtualID)
	if exists {
		instances = append(instances, Instance{
			OriginalID: origID,
			ServerID:   srvID,
		})
	}

	for rows.Next() {
		var inst Instance
		if err := rows.Scan(&inst.OriginalID, &inst.ServerID, &inst.Bitrate); err == nil {
			instances = append(instances, inst)
		}
	}

	return instances
}

// CleanupServer 清理指定服务器的所有映射。
func (s *Store) CleanupServer(serverID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 数据库清理
	_, err := s.db.Exec(`DELETE FROM id_instances WHERE server_id = ?`, serverID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM id_mapping WHERE server_id = ?`, serverID)
	if err != nil {
		return err
	}

	// 缓存清理
	toDelete := make([]string, 0)
	for vid, m := range s.cache {
		if m.ServerID == serverID {
			toDelete = append(toDelete, vid)
		}
	}
	for _, vid := range toDelete {
		if m, ok := s.cache[vid]; ok {
			delete(s.rmap, fmt.Sprintf("%s:%d", m.OriginalID, m.ServerID))
			delete(s.cache, vid)
		}
	}

	return nil
}

// Stats 返回映射统计信息。
func (s *Store) Stats() (total int, byServer map[int]int) {
	byServer = make(map[int]int)

	rows, err := s.db.Query(`SELECT server_id, COUNT(*) FROM id_mapping GROUP BY server_id`)
	if err != nil {
		return 0, byServer
	}
	defer rows.Close()

	for rows.Next() {
		var sid, cnt int
		if rows.Scan(&sid, &cnt) == nil {
			byServer[sid] = cnt
			total += cnt
		}
	}
	return
}

func (s *Store) loadCache() {
	rows, err := s.db.Query(`SELECT virtual_id, original_id, server_id, item_type FROM id_mapping`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var m Mapping
		var itemType sql.NullString
		if err := rows.Scan(&m.VirtualID, &m.OriginalID, &m.ServerID, &itemType); err == nil {
			m.ItemType = itemType.String
			s.cache[m.VirtualID] = &m
			s.rmap[fmt.Sprintf("%s:%d", m.OriginalID, m.ServerID)] = m.VirtualID
		}
	}
}

func generateVirtualID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // v4
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x%x%x%x%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RewriteIDsInJSON 在 JSON 字节中将原始 ID 替换为虚拟 ID。
func (s *Store) RewriteIDsInJSON(data []byte, serverID int, knownIDs []string) []byte {
	result := string(data)
	for _, origID := range knownIDs {
		if origID == "" {
			continue
		}
		vid := s.GetOrCreate(origID, serverID, "")
		result = strings.ReplaceAll(result, `"`+origID+`"`, `"`+vid+`"`)
	}
	return []byte(result)
}

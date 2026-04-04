package traffic

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/fusionride/fusion-ride/internal/db"
)

// Meter 提供 per-upstream 流量计量。
type Meter struct {
	mu       sync.RWMutex
	counters map[int]*Counter // upstreamID → Counter
	db       *db.DB
	stopCh   chan struct{}
}

// Counter 单个上游的流量计数器。
type Counter struct {
	BytesIn  atomic.Int64
	BytesOut atomic.Int64
}

// Snapshot 流量快照。
type Snapshot struct {
	UpstreamID int   `json:"upstreamId"`
	BytesIn    int64 `json:"bytesIn"`
	BytesOut   int64 `json:"bytesOut"`
	Timestamp  int64 `json:"timestamp"`
}

// NewMeter 创建流量计量器。
func NewMeter(database *db.DB) *Meter {
	return &Meter{
		counters: make(map[int]*Counter),
		db:       database,
		stopCh:   make(chan struct{}),
	}
}

// Add 增加流量计数。
func (m *Meter) Add(upstreamID int, bytesIn, bytesOut int64) {
	m.mu.RLock()
	c, ok := m.counters[upstreamID]
	m.mu.RUnlock()

	if !ok {
		m.mu.Lock()
		c, ok = m.counters[upstreamID]
		if !ok {
			c = &Counter{}
			m.counters[upstreamID] = c
		}
		m.mu.Unlock()
	}

	if bytesIn > 0 {
		c.BytesIn.Add(bytesIn)
	}
	if bytesOut > 0 {
		c.BytesOut.Add(bytesOut)
	}
}

// Snapshots 返回当前所有上游的流量快照。
func (m *Meter) Snapshots() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now().Unix()
	snapshots := make([]Snapshot, 0, len(m.counters))
	for id, c := range m.counters {
		snapshots = append(snapshots, Snapshot{
			UpstreamID: id,
			BytesIn:    c.BytesIn.Load(),
			BytesOut:   c.BytesOut.Load(),
			Timestamp:  now,
		})
	}
	return snapshots
}

// StartFlush 启动定期 flush 到数据库。
func (m *Meter) StartFlush(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.flush()
			case <-m.stopCh:
				m.flush() // 退出前最后 flush
				return
			}
		}
	}()
}

// Stop 停止 flush 定时器.
func (m *Meter) Stop() {
	close(m.stopCh)
}

// TotalStats 获取历史总流量统计。
func (m *Meter) TotalStats() (map[int]Snapshot, error) {
	rows, err := m.db.Query(
		`SELECT upstream_id, SUM(bytes_in), SUM(bytes_out)
		 FROM traffic_stats GROUP BY upstream_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int]Snapshot)
	for rows.Next() {
		var s Snapshot
		if rows.Scan(&s.UpstreamID, &s.BytesIn, &s.BytesOut) == nil {
			result[s.UpstreamID] = s
		}
	}

	// 加上当前未 flush 的
	m.mu.RLock()
	for id, c := range m.counters {
		s := result[id]
		s.UpstreamID = id
		s.BytesIn += c.BytesIn.Load()
		s.BytesOut += c.BytesOut.Load()
		result[id] = s
	}
	m.mu.RUnlock()

	return result, nil
}

// RecentStats 获取最近 N 分钟的流量统计。
func (m *Meter) RecentStats(minutes int) ([]Snapshot, error) {
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()

	rows, err := m.db.Query(
		`SELECT upstream_id, bytes_in, bytes_out, timestamp
		 FROM traffic_stats WHERE timestamp > ? ORDER BY timestamp ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []Snapshot
	for rows.Next() {
		var s Snapshot
		if rows.Scan(&s.UpstreamID, &s.BytesIn, &s.BytesOut, &s.Timestamp) == nil {
			stats = append(stats, s)
		}
	}
	return stats, nil
}

func (m *Meter) flush() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now().Unix()

	for id, c := range m.counters {
		in := c.BytesIn.Swap(0)
		out := c.BytesOut.Swap(0)

		if in == 0 && out == 0 {
			continue
		}

		m.db.Exec(
			`INSERT INTO traffic_stats(upstream_id, bytes_in, bytes_out, timestamp) VALUES(?, ?, ?, ?)`,
			id, in, out, now,
		)
	}
}

package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type MonitorBucket struct {
	Time            time.Time
	Checks          int
	Successes       int
	DurationTotalMS int64
	Error           string
	Errors          int
	LegacyErrors    bool
	Output          *MonitorOutput
}
type MonitorMatrixRow struct {
	HostID  string
	Domains []string
	Enabled bool
	Reason  string
	Last    *MonitorSample
	Buckets []MonitorBucket
	Days    []MonitorBucket
	Error   string
}

func (m *Manager) bucketPath(id string) string {
	return filepath.Join(m.config.DataDir, "monitor-hours", filepath.Base(m.historyPath(id)))
}
func (m *Manager) bucketKey(id string) string { return m.historyKey(id) + ":hours" }
func decodeBuckets(raw []byte) ([]MonitorBucket, error) {
	if len(raw) > 256<<10 || !json.Valid(raw) {
		return nil, fmt.Errorf("invalid hourly history")
	}
	values, err := cast.FromJSON[[]MonitorBucket](raw)
	if err != nil || len(values) > 721 {
		return nil, fmt.Errorf("invalid hourly history")
	}
	seen := map[int64]bool{}
	for _, b := range values {
		if b.Time.IsZero() || b.Checks < 1 || b.Successes < 0 || b.Successes > b.Checks || b.DurationTotalMS < 0 || seen[b.Time.Unix()] {
			return nil, fmt.Errorf("invalid hourly history")
		}
		seen[b.Time.Unix()] = true
	}
	return values, nil
}
func (m *Manager) readLegacyBucketsLocked(id string) ([]MonitorBucket, error) {
	var raw []byte
	if m.rd != nil {
		values := m.rd.Do("HVALS", m.bucketKey(id)+":v2")
		if values.Error != nil {
			return nil, values.Error
		}
		entries := values.Strings()
		if len(entries) > 0 {
			if len(entries) > 720 {
				return nil, fmt.Errorf("hourly history exceeds retention")
			}
			return decodeBuckets([]byte("[" + strings.Join(entries, ",") + "]"))
		}
		value := m.rd.Do("GET", m.bucketKey(id))
		if value.Error != nil {
			return nil, value.Error
		}
		raw = value.Bytes()
	} else {
		path := m.bucketPath(id)
		if file.Exists(path) {
			var err error
			raw, err = file.ReadBytes(path)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(raw) == 0 {
		return []MonitorBucket{}, nil
	}
	return decodeBuckets(raw)
}
func addBucket(values []MonitorBucket, sample MonitorSample) []MonitorBucket {
	stamp := sample.Time.UTC().Truncate(time.Hour)
	cutoff := time.Now().UTC().Truncate(time.Hour).Add(-719 * time.Hour)
	kept := values[:0]
	found := -1
	for _, b := range values {
		if !b.Time.Before(cutoff) {
			if b.Time.Equal(stamp) {
				found = len(kept)
			}
			kept = append(kept, b)
		}
	}
	if stamp.Before(cutoff) {
		return kept
	}
	if found < 0 {
		kept = append(kept, MonitorBucket{Time: stamp})
		found = len(kept) - 1
	}
	kept[found].Checks++
	if sample.OK {
		kept[found].Successes++
	}
	kept[found].DurationTotalMS += sample.DurationMS
	sort.Slice(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
	return kept
}

func (m *Manager) readBucketsLocked(id string) ([]MonitorBucket, error) {
	values, err := m.readCompactLocked(id)
	if err != nil {
		return nil, err
	}
	if len(values) > 0 {
		return compactPeriods(values, false)
	}
	return m.readLegacyBucketsLocked(id)
}
func (m *Manager) MonitorMatrix() ([]MonitorMatrixRow, error) {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	records := cloneState(m.state).Hosts
	m.mu.Unlock()
	rows := []MonitorMatrixRow{}
	for id, record := range records {
		row := MonitorMatrixRow{HostID: id, Domains: record.Draft.Domains, Buckets: []MonitorBucket{}}
		if record.Current != nil {
			row.Domains = record.Current.Domains
		}
		if record.Current == nil {
			row.Reason = "未发布"
		} else if !record.Current.Enabled {
			row.Reason = "Host 已停用"
		} else if !monitorEnabled(*record.Current) {
			row.Reason = "监控已关闭或无代理目标"
		} else {
			row.Enabled = true
			row.Reason = "每 " + cast.String(monitorDefaults(record.Current.Monitor).IntervalSeconds) + " 秒检查"
		}
		last, err := m.latestMonitor(id)
		if err != nil {
			row.Error = "检查记录不可用"
		} else if !last.Time.IsZero() {
			row.Last = &last
		}
		m.monitorMu.Lock()
		buckets, err := m.readBucketsLocked(id)
		if err == nil && len(buckets) == 0 {
			history, historyErr := m.readHistoryLocked(id)
			if historyErr != nil {
				err = historyErr
			} else {
				for _, sample := range history {
					buckets = addBucket(buckets, sample)
				}
			}
		}
		values, compactErr := m.readCompactLocked(id)
		if compactErr != nil {
			err = compactErr
		} else if len(values) > 0 {
			row.Days, err = compactPeriods(values, true)
		}
		m.monitorMu.Unlock()
		if err != nil {
			row.Error = "历史统计不可用"
		} else {
			row.Buckets = buckets
		}
		rows = append(rows, row)
	}
	order := map[string]int{}
	for i, record := range m.Hosts() {
		order[record.Draft.ID] = i
	}
	sort.Slice(rows, func(i, j int) bool { return order[rows[i].HostID] < order[rows[j].HostID] })
	return rows, nil
}

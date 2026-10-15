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

type monitorCounter struct {
	Total        int64
	Times        int
	Error        string
	Errors       int
	LegacyErrors bool
	Output       *MonitorOutput
}
type monitorRecent struct {
	At     int64
	Sample MonitorSample
}

func (m *Manager) compactKey(id string) string { return m.config.Prefix + ":monitor:" + id }
func (m *Manager) compactPath(id string) string {
	return filepath.Join(m.config.DataDir, "monitor-compact", filepath.Base(m.historyPath(id)))
}
func (m *Manager) readCompactLocked(id string) (map[string]string, error) {
	if m.rd != nil {
		value := m.rd.Do("HGETALL", m.compactKey(id))
		return value.StringMap(), value.Error
	}
	path := m.compactPath(id)
	if !file.Exists(path) {
		return map[string]string{}, nil
	}
	raw, err := file.ReadBytes(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 4<<20 || !json.Valid(raw) {
		return nil, fmt.Errorf("invalid compact history")
	}
	return cast.FromJSON[map[string]string](raw)
}
func compactCutoffs(now time.Time) (string, string) {
	now = now.In(time.Local)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return day.AddDate(0, 0, -29).Format("20060102"), now.Truncate(time.Hour).Add(-23 * time.Hour).Format("2006010215")
}
func pruneCompact(values map[string]string, now time.Time) {
	day, hour := compactCutoffs(now)
	for field := range values {
		switch {
		case strings.HasPrefix(field, "d:"):
			if len(field) != 10 || field[2:] < day {
				delete(values, field)
			}
		case strings.HasPrefix(field, "h:"):
			if len(field) != 12 || field[2:] < hour {
				delete(values, field)
			}
		case strings.HasPrefix(field, "r:"):
			if len(field) != 4 || cast.Int(field[2:]) < 0 || cast.Int(field[2:]) > 9 {
				delete(values, field)
			}
		default:
			delete(values, field)
		}
	}
}
func mergeCompactCounter(values map[string]string, field string, total int64, times int, reason string) error {
	var counter monitorCounter
	if raw := values[field]; raw != "" {
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("invalid period counter")
		}
		var err error
		counter, err = cast.FromJSON[monitorCounter](raw)
		if err != nil || counter.Total < 0 || counter.Times < 1 {
			return fmt.Errorf("invalid period counter")
		}
	}
	if counter.Error != "" && counter.Errors == 0 {
		counter.LegacyErrors = true
	}
	counter.Total += total
	counter.Times += times
	if reason != "" {
		counter.Error = reason
	}
	raw, err := cast.ToJSON(counter)
	if err == nil {
		values[field] = raw
	}
	return err
}
func compactPeriods(values map[string]string, daily bool) ([]MonitorBucket, error) {
	result := []MonitorBucket{}
	prefix, layout := "h:", "2006010215"
	if daily {
		prefix, layout = "d:", "20060102"
	}
	for field, raw := range values {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		stamp, err := time.ParseInLocation(layout, strings.TrimPrefix(field, prefix), time.Local)
		if err != nil || !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("invalid period")
		}
		counter, err := cast.FromJSON[monitorCounter](raw)
		if err != nil || counter.Times < 1 || counter.Total < 0 {
			return nil, fmt.Errorf("invalid counter")
		}
		successes := counter.Times - counter.Errors
		if counter.Error != "" && counter.Errors == 0 {
			counter.LegacyErrors = true
		}
		if counter.Error == "" {
			successes = counter.Times
		}
		result = append(result, MonitorBucket{Time: stamp, Checks: counter.Times, Successes: successes, DurationTotalMS: counter.Total, Error: counter.Error, Errors: counter.Errors, LegacyErrors: counter.LegacyErrors, Output: counter.Output})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Time.Before(result[j].Time) })
	return result, nil
}
func compactRecent(values map[string]string, id string) ([]MonitorSample, error) {
	entries := []monitorRecent{}
	for field, raw := range values {
		if !strings.HasPrefix(field, "r:") {
			continue
		}
		if len(raw) > 40<<10 || !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("invalid recent check")
		}
		entry, err := cast.FromJSON[monitorRecent](raw)
		if err != nil || entry.At <= 0 {
			return nil, fmt.Errorf("invalid recent check")
		}
		if err := validateMonitorSample(entry.Sample, id); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].At < entries[j].At })
	if len(entries) > 10 {
		return nil, fmt.Errorf("too many recent checks")
	}
	result := make([]MonitorSample, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Sample)
	}
	return result, nil
}
func compactSlot(values map[string]string) int {
	index := -1
	var latest int64
	for field, raw := range values {
		if !strings.HasPrefix(field, "r:") {
			continue
		}
		entry, err := cast.FromJSON[monitorRecent](raw)
		if err == nil && entry.At >= latest {
			latest = entry.At
			index = cast.Int(field[2:])
		}
	}
	return (index + 1) % 10
}
func (m *Manager) seedCompactLocked(id string) (map[string]string, error) {
	// Only legacy readers are used until the new Hash is committed.
	values := map[string]string{}
	hours, err := m.readLegacyBucketsLocked(id)
	if err != nil {
		return nil, err
	}
	for _, hour := range hours {
		stamp := hour.Time.In(time.Local)
		reason := ""
		if hour.Successes < hour.Checks {
			reason = "历史时段出现失败"
		}
		for _, field := range []string{"d:" + stamp.Format("20060102"), "h:" + stamp.Format("2006010215")} {
			if err := mergeCompactCounter(values, field, hour.DurationTotalMS, hour.Checks, reason); err != nil {
				return nil, err
			}
		}
	}
	history, err := m.readLegacyHistoryLocked(id)
	if err != nil {
		return nil, err
	}
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	for i, sample := range history {
		raw, err := cast.ToJSON(monitorRecent{At: sample.Time.UnixMilli(), Sample: sample})
		if err != nil {
			return nil, err
		}
		values[fmt.Sprintf("r:%02d", i)] = raw
	}
	pruneCompact(values, time.Now())
	return values, nil
}
func (m *Manager) saveCompactLocked(sample MonitorSample) error {
	values := map[string]string{}
	var err error
	existing := false
	if m.rd != nil {
		reply := m.rd.Do("EXISTS", m.compactKey(sample.HostID))
		if reply.Error != nil {
			return reply.Error
		}
		existing = reply.Bool()
	} else {
		values, err = m.readCompactLocked(sample.HostID)
		if err != nil {
			return err
		}
		existing = len(values) > 0
	}
	if !existing {
		values, err = m.seedCompactLocked(sample.HostID)
		if err != nil {
			return err
		}
	}
	stamp := sample.Time.In(time.Local)
	reason := ""
	if !sample.OK {
		reason = sample.Reason
		if reason == "" {
			reason = "检查失败"
		}
	}
	if len(reason) > 1024 {
		reason = "检查失败"
	}
	recent, err := cast.ToJSON(monitorRecent{At: sample.Time.UnixMilli(), Sample: sample})
	if err != nil {
		return err
	}
	if len(recent) > 40<<10 {
		return fmt.Errorf("check exceeds storage limit")
	}
	dayField, hourField := "d:"+stamp.Format("20060102"), "h:"+stamp.Format("2006010215")
	if m.rd != nil {
		seed, err := cast.ToJSON(values)
		if err != nil {
			return err
		}
		dayCut, hourCut := compactCutoffs(time.Now())
		reply := m.rd.Do("EVAL", `if redis.call('HLEN',KEYS[1])==0 then for field,value in pairs(cjson.decode(ARGV[8])) do redis.call('HSET',KEYS[1],field,value) end end
 local latest=-1;local slot=-1;local expired={}
 for _,field in ipairs(redis.call('HKEYS',KEYS[1])) do
 if string.sub(field,1,2)=='r:' then local v=cjson.decode(redis.call('HGET',KEYS[1],field));if v.at>=latest then latest=v.at;slot=tonumber(string.sub(field,3)) end
 elseif (string.sub(field,1,2)=='d:' and string.sub(field,3)<ARGV[6]) or (string.sub(field,1,2)=='h:' and string.sub(field,3)<ARGV[7]) then table.insert(expired,field) end end
 local counters={}
 for _,field in ipairs({ARGV[1],ARGV[2]}) do local old=redis.call('HGET',KEYS[1],field);local v=old and cjson.decode(old) or {total=0,times=0,error=''};if v.error~='' and (v.errors or 0)==0 then v.legacyErrors=true end;v.errors=v.errors or 0;v.total=v.total+tonumber(ARGV[3]);v.times=v.times+1;local check=cjson.decode(ARGV[5]).sample;if ARGV[4]~='' then v.error=ARGV[4];v.errors=v.errors+1;v.output=check.output elseif v.error=='' then v.output=check.output end;counters[field]=cjson.encode(v) end
 for _,field in ipairs(expired) do redis.call('HDEL',KEYS[1],field) end
 for field,value in pairs(counters) do redis.call('HSET',KEYS[1],field,value) end
 local recent=cjson.decode(ARGV[5]);recent.at=math.max(recent.at,latest+1);redis.call('HSET',KEYS[1],string.format('r:%02d',(slot+1)%10),cjson.encode(recent));redis.call('EXPIRE',KEYS[1],2592000);return redis.call('HLEN',KEYS[1])`, 1, m.compactKey(sample.HostID), dayField, hourField, sample.DurationMS, reason, recent, dayCut, hourCut, seed)
		if reply.Error != nil {
			return reply.Error
		}
		if reply.Int() > 64 {
			return fmt.Errorf("monitor Hash exceeds 64 fields")
		}
		// Successful migration replaces obsolete detailed Lists/hourly keys; the new Hash owns the complete retained state.
		return m.rd.Do("DEL", m.historyKey(sample.HostID), m.bucketKey(sample.HostID), m.bucketKey(sample.HostID)+":v2").Error
	}
	slot := compactSlot(values)
	if err := mergeCompactCounter(values, dayField, sample.DurationMS, 1, reason); err != nil {
		return err
	}
	if err := mergeCompactCounter(values, hourField, sample.DurationMS, 1, reason); err != nil {
		return err
	}
	for _, field := range []string{dayField, hourField} {
		counter, err := cast.FromJSON[monitorCounter](values[field])
		if err != nil {
			return err
		}
		if !sample.OK {
			counter.Errors++
			counter.Output = sample.Output
		} else if counter.Error == "" {
			counter.Output = sample.Output
		}
		values[field], err = cast.ToJSON(counter)
		if err != nil {
			return err
		}
	}
	values[fmt.Sprintf("r:%02d", slot)] = recent
	pruneCompact(values, time.Now())
	if len(values) > 64 {
		return fmt.Errorf("monitor history exceeds 64 fields")
	}
	raw, err := cast.ToJSONBytes(values)
	if err != nil {
		return err
	}
	if err := privateWrite(m.compactPath(sample.HostID), raw); err != nil {
		return err
	}
	for _, path := range []string{m.historyPath(sample.HostID), m.bucketPath(sample.HostID)} {
		if file.Exists(path) {
			if err := file.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

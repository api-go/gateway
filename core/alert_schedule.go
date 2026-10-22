package gateway

import (
	"fmt"
	"slices"
	"time"
	_ "time/tzdata"
)

func alertThreshold(channel AlertChannel) int {
	if channel.FailureThreshold == 0 {
		return 3
	}
	return channel.FailureThreshold
}
func alertApplies(channel AlertChannel, hostID string) bool {
	return channel.HostMode == "" || channel.HostMode == "all" || channel.HostMode == "selected" && slices.Contains(channel.HostIDs, hostID)
}
func validateAlertSchedule(channel *AlertChannel) error {
	if channel.HostMode == "" {
		channel.HostMode = "all"
	}
	if channel.HostMode != "all" && channel.HostMode != "selected" {
		return fmt.Errorf("Host范围无效")
	}
	if channel.HostMode == "all" {
		channel.HostIDs = nil
	}
	if len(channel.HostIDs) > 10000 {
		return fmt.Errorf("Host范围超出上限")
	}
	seen := map[string]bool{}
	for _, id := range channel.HostIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("Host范围存在空值或重复")
		}
		seen[id] = true
	}
	channel.FailureThreshold = alertThreshold(*channel)
	if channel.FailureThreshold < 1 || channel.FailureThreshold > 20 {
		return fmt.Errorf("连续失败阈值须为1–20次")
	}

	if channel.RepeatSeconds != 0 && (channel.RepeatSeconds < 60 || channel.RepeatSeconds > 604800) {
		return fmt.Errorf("重复提醒须为 0 或 60–604800 秒")
	}
	if channel.Timezone == "" {
		channel.Timezone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(channel.Timezone); err != nil {
		return fmt.Errorf("时区无效")
	}
	if channel.QuietStart == "" && channel.QuietEnd == "" {
		return nil
	}
	for _, s := range []string{channel.QuietStart, channel.QuietEnd} {
		if _, err := time.Parse("15:04", s); err != nil {
			return fmt.Errorf("勿扰时段须完整填写 HH:MM")
		}
	}
	if channel.QuietStart == channel.QuietEnd {
		return fmt.Errorf("勿扰开始和结束时间不能相同")
	}
	return nil
}
func alertQuiet(channel AlertChannel, now time.Time) bool {
	if channel.QuietStart == "" || channel.QuietEnd == "" {
		return false
	}
	zone := channel.Timezone
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false
	}
	clock := now.In(loc).Format("15:04")
	if channel.QuietStart < channel.QuietEnd {
		return clock >= channel.QuietStart && clock < channel.QuietEnd
	}
	return clock >= channel.QuietStart || clock < channel.QuietEnd
}

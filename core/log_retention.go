package gateway

import (
	"apigo.cc/go/file"
	"apigo.cc/go/log"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// PruneGatewayLogs removes only dated files matching this application's configured log name and split format.
// The active file, unrelated files, backups and unparseable suffixes are always preserved.
func PruneGatewayLogs(conf log.Config, days int, now time.Time) error {
	if days < 1 || days > 3650 {
		return fmt.Errorf("log retention requires 1–3650 days")
	}
	if conf.File == "" || conf.File == "console" || strings.Contains(conf.File, "://") || conf.SplitTag == "" {
		return nil
	}
	directory := filepath.Dir(conf.File)
	if !file.Exists(directory) {
		return nil
	}
	entries, err := file.ReadDir(directory)
	if err != nil {
		return err
	}
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days+1)
	prefix := filepath.Base(conf.File) + "."
	for _, entry := range entries {
		if entry.IsDir || !strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		date, err := time.ParseInLocation(conf.SplitTag, strings.TrimPrefix(entry.Name, prefix), now.Location())
		if err != nil || !date.Before(cutoff) || entry.Name == prefix+now.Format(conf.SplitTag) {
			continue
		}
		if err := file.Remove(entry.FullName); err != nil {
			return err
		}
	}
	return nil
}

package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 备份文件名：usage-20261026-041000.db，先写成 .tmp，成功后改名（§12.6）。
const (
	backupPrefix = "usage-"
	backupSuffix = ".db"
	backupLayout = "20060102-150405"
	tmpSuffix    = ".tmp"
)

// Backup 用 VACUUM INTO 备份到 dir，再删除超过 keepDays 天的备份。文件名带时分秒，因为 VACUUM INTO
// 要求目标文件不存在；先写 .tmp 再改名，中途中断不会留下看起来正常的坏文件。now 的时区决定文件名里的时间。
func (d *DB) Backup(ctx context.Context, dir string, now time.Time, keepDays int) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("创建备份目录: %w", err)
	}
	final := filepath.Join(dir, backupPrefix+now.Format(backupLayout)+backupSuffix)
	tmp := final + tmpSuffix
	// VACUUM INTO 只读源库，在读库上执行，不占写库的连接。
	if _, err := d.r.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("备份数据库: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("备份改名: %w", err)
	}
	if err := pruneBackups(dir, now, keepDays); err != nil {
		return final, err
	}
	return final, nil
}

// pruneBackups 删除早于 now 往前 keepDays 天的备份，只处理本程序生成的文件名。
func pruneBackups(dir string, now time.Time, keepDays int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取备份目录: %w", err)
	}
	cutoff := now.AddDate(0, 0, -keepDays)
	for _, e := range entries {
		at, ok := backupTime(e.Name(), now.Location())
		if !ok || e.IsDir() || !at.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除过期备份: %w", err)
		}
	}
	return nil
}

func backupTime(name string, loc *time.Location) (time.Time, bool) {
	s, ok := strings.CutPrefix(name, backupPrefix)
	if !ok {
		return time.Time{}, false
	}
	s, ok = strings.CutSuffix(s, backupSuffix)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation(backupLayout, s, loc)
	return at, err == nil
}

// CleanupTempBackups 删除上次中断留下的 .tmp 备份，服务启动时调用。
func CleanupTempBackups(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取备份目录: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, backupPrefix) && strings.HasSuffix(name, backupSuffix+tmpSuffix) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("删除残留的备份: %w", err)
			}
		}
	}
	return nil
}

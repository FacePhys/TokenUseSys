package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 日志文件名里的日期格式，例如 app-2026-10-14.log。
const dayLayout = "2006-01-02"

// DailyFile 把日志按自然日写入 dir/<name>-<日期>.log，跨天时换到新文件，并删除超过保留天数的旧文件
// （开发文档 §3：日志保留 7 天）。日期按进程的本地时区（容器里由 TZ 指定）。可以被多个 goroutine 同时写。
type DailyFile struct {
	dir      string
	name     string
	keepDays int
	now      func() time.Time

	mu   sync.Mutex
	day  string
	file *os.File
	stop chan struct{}
	done chan struct{}
}

// OpenDaily 打开当天的日志文件并清理过期文件。之后每分钟检查一次日期，没有日志写入时也会按时换文件和清理。
func OpenDaily(dir, name string, keepDays int) (*DailyFile, error) {
	d, err := openDaily(dir, name, keepDays, time.Now)
	if err != nil {
		return nil, err
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	go d.watch(time.Minute)
	return d, nil
}

func openDaily(dir, name string, keepDays int, now func() time.Time) (*DailyFile, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("创建日志目录: %w", err)
	}
	d := &DailyFile{dir: dir, name: name, keepDays: max(keepDays, 1), now: now}
	if err := d.rotate(d.now()); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DailyFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return 0, os.ErrClosed
	}
	if now := d.now(); now.Format(dayLayout) != d.day {
		// 换文件失败时继续写原来的文件，不丢日志。
		_ = d.rotate(now)
	}
	return d.file.Write(p)
}

// Close 停止定时检查并关闭文件。
func (d *DailyFile) Close() error {
	if d.stop != nil {
		close(d.stop)
		<-d.done
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

func (d *DailyFile) watch(every time.Duration) {
	defer close(d.done)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.check()
		}
	}
}

// check 在日期变化时换文件并清理过期文件。
func (d *DailyFile) check() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now := d.now(); d.file != nil && now.Format(dayLayout) != d.day {
		_ = d.rotate(now)
	}
}

// rotate 打开 now 当天的文件，替换当前文件，然后清理过期文件。调用方持有锁。
func (d *DailyFile) rotate(now time.Time) error {
	day := now.Format(dayLayout)
	f, err := os.OpenFile(filepath.Join(d.dir, d.name+"-"+day+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("打开日志文件: %w", err)
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.file, d.day = f, day
	d.prune(now)
	return nil
}

// prune 删除本进程写的、超过保留天数的文件：保留今天在内最近 keepDays 天。
// 删除失败只写到标准错误，不影响写日志。
func (d *DailyFile) prune(now time.Time) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取日志目录失败:", err)
		return
	}
	y, m, day := now.Date()
	cutoff := time.Date(y, m, day-d.keepDays+1, 0, 0, 0, 0, now.Location()).Format(dayLayout)
	prefix := d.name + "-"
	for _, e := range entries {
		name := e.Name()
		date, ok := strings.CutPrefix(name, prefix)
		if !ok || e.IsDir() {
			continue
		}
		date, ok = strings.CutSuffix(date, ".log")
		if !ok || date >= cutoff {
			continue
		}
		if _, err := time.Parse(dayLayout, date); err != nil {
			continue // 不是本进程按天写的文件
		}
		if err := os.Remove(filepath.Join(d.dir, name)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "删除过期日志失败:", err)
		}
	}
}

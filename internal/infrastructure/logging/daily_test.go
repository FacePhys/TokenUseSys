package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
)

func TestDailyFileRotatesAndKeepsRecentDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 14, 23, 59, 0, 0, time.FixedZone("CST", 8*3600))
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	for _, name := range []string{"app-2026-10-07.log", "app-2026-10-08.log", "app-2026-10-13.log",
		"worker-2026-10-01.log", "app-notes.log", "readme.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("旧\n"), 0o600))
	}
	path := func(name string) string { return filepath.Join(dir, name) }

	d, err := openDaily(dir, "app", 7, clock)
	require.NoError(t, err)
	require.NoFileExists(t, path("app-2026-10-07.log"), "超过 7 天的删除")
	for _, name := range []string{"app-2026-10-08.log", "app-2026-10-13.log", "worker-2026-10-01.log", "app-notes.log", "readme.txt"} {
		require.FileExists(t, path(name), "保留期内的、其他进程的和不相关的文件都不动")
	}

	_, err = d.Write([]byte("第一行\n"))
	require.NoError(t, err)
	advance(2 * time.Minute) // 跨天到 10-15
	_, err = d.Write([]byte("第二行\n"))
	require.NoError(t, err)
	first, err := os.ReadFile(path("app-2026-10-14.log"))
	require.NoError(t, err)
	require.Equal(t, "第一行\n", string(first))
	second, err := os.ReadFile(path("app-2026-10-15.log"))
	require.NoError(t, err)
	require.Equal(t, "第二行\n", string(second))
	require.NoFileExists(t, path("app-2026-10-08.log"), "跨天时清理")

	// 没有日志写入时，定时检查也会换文件和清理。
	advance(5 * 24 * time.Hour) // 10-20
	d.check()
	require.FileExists(t, path("app-2026-10-20.log"))
	require.NoFileExists(t, path("app-2026-10-13.log"))
	require.FileExists(t, path("app-2026-10-14.log"), "10-14～10-20 共 7 天")

	require.NoError(t, d.Close())
	_, err = d.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestDailyFileConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDaily(dir, "app", 7)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, err := d.Write([]byte("一行日志\n"))
				require.NoError(t, err)
			}
		})
	}
	wg.Wait()
	require.NoError(t, d.Close())
	data, err := os.ReadFile(filepath.Join(dir, "app-"+time.Now().Format(dayLayout)+".log"))
	require.NoError(t, err)
	require.Equal(t, 800, strings.Count(string(data), "一行日志\n"))
}

func TestSetupWritesOnlyToFilesWhenDirConfigured(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	log, closeLog, err := Setup(config.LogConfig{Level: "info", Format: "json", Dir: dir, RetentionDays: 7}, "worker", &stdout)
	require.NoError(t, err)
	log.Info("入库完成", "version_id", 3)
	closeLog()
	require.Empty(t, stdout.String(), "按天写文件时不再写标准输出")
	data, err := os.ReadFile(filepath.Join(dir, "worker-"+time.Now().Format(dayLayout)+".log"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"msg":"入库完成"`)

	log, closeLog, err = Setup(config.LogConfig{Level: "info", Format: "json"}, "worker", &stdout)
	require.NoError(t, err)
	log.Info("本地开发")
	closeLog()
	require.Contains(t, stdout.String(), "本地开发", "没有配置目录时写标准输出")

	_, _, err = Setup(config.LogConfig{Level: "info", Format: "json", Dir: filepath.Join(dir, "worker-"+time.Now().Format(dayLayout)+".log", "x"), RetentionDays: 7}, "app", &stdout)
	require.Error(t, err, "目录不可用时启动失败")
}

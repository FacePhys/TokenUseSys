// Package cli 是 relayctl 命令行接口：上游自检、生成密码哈希、重拉某一天、立即备份、测试告警（开发文档 §10）。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usageText = `用法: relayctl [-config config.yaml] <子命令> [参数]

  selfcheck [-fixtures 目录]   M1 自检：逐项核对 4router 接口和开发文档的假设，只读，约 3～5 分钟；
                               带 -fixtures 时把脱敏后的响应写进目录，作为单元测试样本
                               （本机：-fixtures testdata/fixtures；服务器上不带，只看连通和鉴权）
  hash-password                输入管理员密码（不回显），输出 bcrypt 哈希，填到 auth.admin_password_hash
  resync -day 2026-10-26 [-force]
                               按 1 小时窗口重拉某一天（只能是昨天及以前）并重算汇总；上游返回 0 条时
                               默认不替换本地数据，-force 强制替换。可以和服务同时运行
  backup                       立即备份一次数据库到 storage.backup_dir
  alert-test                   往 alert.webhook 发一条测试消息（飞书 / 钉钉 / 企业微信机器人）
`

// Run 解析全局参数并执行子命令。
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("relayctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	configPath := fs.String("config", "config.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("缺少子命令")
	}
	e := &env{configPath: *configPath, stdin: stdin, stdout: stdout, stderr: stderr}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	switch cmd {
	case "selfcheck":
		return e.selfcheck(ctx, rest)
	case "hash-password":
		return e.hashPassword(rest)
	case "resync":
		return e.resync(ctx, rest)
	case "backup":
		return e.backup(ctx, rest)
	case "alert-test":
		return e.alertTest(ctx, rest)
	default:
		fs.Usage()
		return fmt.Errorf("未知的子命令 %q", cmd)
	}
}

type env struct {
	configPath string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

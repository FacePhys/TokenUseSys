// Package migrations 内嵌 goose 的 SQL 迁移文件，服务和 relayctl 打开数据库时自动执行。
package migrations

import "embed"

// FS 包含全部迁移文件。已执行过的文件不能改，表结构变化就新建文件（开发文档 §12.5）。
//
//go:embed *.sql
var FS embed.FS

// Package sqlite 是本地存储（开发文档 §5）：SQLite（modernc.org/sqlite，纯 Go），WAL 模式。
// 写库只有 1 个连接，写事务一开始就拿写锁（_txlock=immediate），和 relayctl 的写事务靠 busy_timeout 排队；
// 读库给页面查询用。所有 SQL 只写在这个包里（§3 可移植约定）。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // 注册 database/sql 的 sqlite 驱动

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/migrations"
)

var _ port.Store = (*DB)(nil)

// pragmas 写在 DSN 里，每个连接打开时执行；goose 在事务里执行迁移，WAL 不能写在迁移文件里（§5）。
const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

// readConns 是读库的连接数，页面查询用。
const readConns = 4

type DB struct {
	w *sql.DB
	r *sql.DB
}

// Open 打开数据库（不存在时创建），并执行内嵌的迁移。
func Open(ctx context.Context, path string) (*DB, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("数据库路径不能含 ? 或 #：%s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("创建数据库目录: %w", err)
	}
	w, err := sql.Open("sqlite", "file:"+path+"?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	w.SetMaxOpenConns(1)
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", "file:"+path+"?"+pragmas)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	r.SetMaxOpenConns(readConns)
	return &DB{w: w, r: r}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("加载迁移文件: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("执行迁移: %w", err)
	}
	return nil
}

// Read 返回读库，给看板查询用。
func (d *DB) Read() *sql.DB { return d.r }

func (d *DB) Close() error {
	return errors.Join(d.r.Close(), d.w.Close())
}

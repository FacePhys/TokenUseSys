-- 表结构见开发文档 §5。时间都是 Unix 秒；额度、token 数是 int64（换 PostgreSQL 时用 BIGINT，§5.3）。

-- +goose Up
CREATE TABLE tokens (
  id             INTEGER PRIMARY KEY,   -- 上游令牌 id
  name           TEXT NOT NULL,
  key_hint       TEXT,                  -- sk-AbCd…9xYz，不存完整 key
  status         INTEGER NOT NULL,      -- 1 启用 2 禁用 3 过期 4 用尽；0 是只在日志里见过、令牌列表里没有的
  group_name     TEXT,
  unlimited      INTEGER NOT NULL,
  remain_quota   INTEGER NOT NULL,
  used_quota     INTEGER NOT NULL,
  expired_time   INTEGER,               -- -1 永不过期
  created_time   INTEGER,
  accessed_time  INTEGER,
  model_limits   TEXT,                  -- 未启用模型限制时为空
  first_seen_at  INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  deleted_at     INTEGER                -- 上游已删除
);

-- 只在 used_quota 或 status 变化时写一行；某时刻的值 = 该时刻之前最近的一行（§4.4）
CREATE TABLE token_snapshots (
  token_id      INTEGER NOT NULL,
  ts            INTEGER NOT NULL,       -- 对齐到整 5 分钟的执行时刻
  used_quota    INTEGER NOT NULL,
  remain_quota  INTEGER NOT NULL,
  status        INTEGER NOT NULL,
  PRIMARY KEY (token_id, ts)
);

CREATE TABLE usage_logs (
  pk                  INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at          INTEGER NOT NULL,
  token_id            INTEGER NOT NULL DEFAULT 0,  -- 0 = 未归属
  token_name          TEXT,
  model_name          TEXT NOT NULL DEFAULT '',
  quota               INTEGER NOT NULL,
  prompt_tokens       INTEGER,
  completion_tokens   INTEGER,
  cache_read_tokens   INTEGER,
  cache_write_tokens  INTEGER,
  use_time            INTEGER,                     -- 秒
  frt_ms              INTEGER,
  is_stream           INTEGER,
  group_name          TEXT,
  request_id          TEXT
);
CREATE INDEX idx_logs_time  ON usage_logs(created_at);
CREATE INDEX idx_logs_token ON usage_logs(token_id, created_at);

-- 页面上的统计全部读这张表
CREATE TABLE usage_hourly (
  hour                INTEGER NOT NULL,            -- 整点 Unix 秒
  token_id            INTEGER NOT NULL,
  model_name          TEXT NOT NULL,
  requests            INTEGER NOT NULL,
  quota               INTEGER NOT NULL,
  prompt_tokens       INTEGER NOT NULL,
  completion_tokens   INTEGER NOT NULL,
  cache_read_tokens   INTEGER NOT NULL,
  cache_write_tokens  INTEGER NOT NULL,
  PRIMARY KEY (hour, token_id, model_name)
);

CREATE TABLE reconcile_reports (
  day             TEXT PRIMARY KEY,   -- 2026-10-26（server.timezone）
  upstream_quota  INTEGER,            -- /api/log/self/stat
  local_quota     INTEGER,            -- 本地 usage_logs 合计
  snapshot_quota  INTEGER,            -- 令牌 used_quota 差值合计；当天 0 点前没有快照时为空
  checked_at      INTEGER
);

-- logs_cursor、data_since、backfill_until、account、status、各任务的执行状态
CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT);

-- +goose Down
DROP TABLE kv;
DROP TABLE reconcile_reports;
DROP TABLE usage_hourly;
DROP TABLE usage_logs;
DROP TABLE token_snapshots;
DROP TABLE tokens;

// Package store 实现动态配置持久层：admin 运行期变更（后端增删、策略热更新）
// 写入关系数据库，重启后加载并覆盖 YAML 同名项。支持 PostgreSQL 与 MySQL，
// 方言差异（占位符、upsert 语法）集中在 dialect 内，DDL 与查询共用。
//
// 表结构（启动时自动建表，见 migrate）：
//   - gateway_backends：动态注册的后端及其挂载的模型路由（labels/models 存 JSON 文本，
//     规避两家 JSON 列类型差异）；
//   - gateway_policies：热更新过的调度策略表达式。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql 驱动注册
	_ "github.com/jackc/pgx/v5/stdlib" // postgres 驱动注册（database/sql 名 "pgx"）

	"ai-gateway/internal/config"
)

// dialect 方言差异集中点。
type dialect struct {
	driver string // database/sql 驱动名
	// numbered true 表示占位符用 $1..$n（postgres），否则用 ?（mysql）。
	numbered bool
}

// ph 生成第 n 个占位符（1 起）。
func (d dialect) ph(n int) string {
	if d.numbered {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// phs 生成 1..n 的占位符列表，如 "$1, $2, $3" 或 "?, ?, ?"。
func (d dialect) phs(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = d.ph(i + 1)
	}
	return strings.Join(out, ", ")
}

var dialects = map[string]dialect{
	"postgres": {driver: "pgx", numbered: true},
	"mysql":    {driver: "mysql", numbered: false},
}

// Store 持久层句柄。
type Store struct {
	db      *sql.DB
	d       dialect
	timeout time.Duration
}

// Open 打开数据库连接、验证连通性并自动建表；失败即返回错误（启动期左移暴露）。
func Open(cfg config.StoreConfig) (*Store, error) {
	d, ok := dialects[cfg.Driver]
	if !ok {
		return nil, fmt.Errorf("持久层 driver %q 非法，可选 postgres/mysql", cfg.Driver)
	}
	db, err := sql.Open(d.driver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("打开持久层数据库失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	s := &Store{db: db, d: d, timeout: cfg.OpTimeout.D()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接持久层数据库失败: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewWithDB 用现成的 *sql.DB 构造（测试注入 sqlmock 用），不建表。
func NewWithDB(db *sql.DB, driver string, timeout time.Duration) *Store {
	return &Store{db: db, d: dialects[driver], timeout: timeout}
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// migrate 建表。DDL 刻意只用两家共有的类型（VARCHAR/TEXT/DOUBLE PRECISION/
// INT/TIMESTAMP），JSON 数据存 TEXT，避免方言分叉。
func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gateway_backends (
			id              VARCHAR(128) PRIMARY KEY,
			url             TEXT NOT NULL,
			engine          VARCHAR(32) NOT NULL,
			weight          DOUBLE PRECISION NOT NULL,
			max_concurrency INT NOT NULL,
			metrics_path    VARCHAR(255) NOT NULL,
			health_path     VARCHAR(255) NOT NULL,
			bootstrap_port  INT NOT NULL,
			labels          TEXT NOT NULL,
			models          TEXT NOT NULL,
			updated_at      TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS gateway_policies (
			name       VARCHAR(128) PRIMARY KEY,
			filter     TEXT NOT NULL,
			score      TEXT NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("持久层建表失败: %w", err)
		}
	}
	return nil
}

// opCtx 生成带超时的操作上下文。
func (s *Store) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}

// BackendRow 一条已持久化的动态后端及其挂载的模型路由。
type BackendRow struct {
	Backend config.BackendConfig
	Models  []string
}

// UpsertBackend 写入（或整体覆盖）一条动态后端。
func (s *Store) UpsertBackend(ctx context.Context, bc config.BackendConfig, models []string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	labels, _ := json.Marshal(bc.Labels)
	modelsJSON, _ := json.Marshal(models)
	cols := "id, url, engine, weight, max_concurrency, metrics_path, health_path, bootstrap_port, labels, models, updated_at"
	var q string
	if s.d.numbered {
		q = fmt.Sprintf(`INSERT INTO gateway_backends (%s) VALUES (%s)
			ON CONFLICT (id) DO UPDATE SET
			url=EXCLUDED.url, engine=EXCLUDED.engine, weight=EXCLUDED.weight,
			max_concurrency=EXCLUDED.max_concurrency, metrics_path=EXCLUDED.metrics_path,
			health_path=EXCLUDED.health_path, bootstrap_port=EXCLUDED.bootstrap_port,
			labels=EXCLUDED.labels, models=EXCLUDED.models, updated_at=EXCLUDED.updated_at`,
			cols, s.d.phs(11))
	} else {
		q = fmt.Sprintf(`INSERT INTO gateway_backends (%s) VALUES (%s)
			ON DUPLICATE KEY UPDATE
			url=VALUES(url), engine=VALUES(engine), weight=VALUES(weight),
			max_concurrency=VALUES(max_concurrency), metrics_path=VALUES(metrics_path),
			health_path=VALUES(health_path), bootstrap_port=VALUES(bootstrap_port),
			labels=VALUES(labels), models=VALUES(models), updated_at=VALUES(updated_at)`,
			cols, s.d.phs(11))
	}
	_, err := s.db.ExecContext(ctx, q,
		bc.ID, bc.URL, bc.Engine, bc.Weight, bc.MaxConcurrency,
		bc.MetricsPath, bc.HealthPath, bc.BootstrapPort,
		string(labels), string(modelsJSON), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("持久化后端 %s 失败: %w", bc.ID, err)
	}
	return nil
}

// DeleteBackend 删除一条动态后端记录；记录不存在不算错误（幂等）。
func (s *Store) DeleteBackend(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM gateway_backends WHERE id = "+s.d.ph(1), id); err != nil {
		return fmt.Errorf("删除持久化后端 %s 失败: %w", id, err)
	}
	return nil
}

// ListBackends 加载全部动态后端（启动期合并进注册表）。
func (s *Store) ListBackends(ctx context.Context) ([]BackendRow, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, url, engine, weight, max_concurrency, metrics_path, health_path,
		 bootstrap_port, labels, models FROM gateway_backends ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("加载持久化后端失败: %w", err)
	}
	defer rows.Close()
	var out []BackendRow
	for rows.Next() {
		var r BackendRow
		var labels, models string
		if err := rows.Scan(&r.Backend.ID, &r.Backend.URL, &r.Backend.Engine,
			&r.Backend.Weight, &r.Backend.MaxConcurrency, &r.Backend.MetricsPath,
			&r.Backend.HealthPath, &r.Backend.BootstrapPort, &labels, &models); err != nil {
			return nil, fmt.Errorf("解析持久化后端行失败: %w", err)
		}
		_ = json.Unmarshal([]byte(labels), &r.Backend.Labels)
		_ = json.Unmarshal([]byte(models), &r.Models)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertPolicy 写入（或覆盖）一条策略。
func (s *Store) UpsertPolicy(ctx context.Context, name, filter, score string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var q string
	if s.d.numbered {
		q = fmt.Sprintf(`INSERT INTO gateway_policies (name, filter, score, updated_at)
			VALUES (%s) ON CONFLICT (name) DO UPDATE SET
			filter=EXCLUDED.filter, score=EXCLUDED.score, updated_at=EXCLUDED.updated_at`,
			s.d.phs(4))
	} else {
		q = fmt.Sprintf(`INSERT INTO gateway_policies (name, filter, score, updated_at)
			VALUES (%s) ON DUPLICATE KEY UPDATE
			filter=VALUES(filter), score=VALUES(score), updated_at=VALUES(updated_at)`,
			s.d.phs(4))
	}
	if _, err := s.db.ExecContext(ctx, q, name, filter, score, time.Now().UTC()); err != nil {
		return fmt.Errorf("持久化策略 %s 失败: %w", name, err)
	}
	return nil
}

// ListPolicies 加载全部持久化策略（启动期覆盖 YAML 同名项）。
func (s *Store) ListPolicies(ctx context.Context) (map[string]config.PolicyConfig, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT name, filter, score FROM gateway_policies")
	if err != nil {
		return nil, fmt.Errorf("加载持久化策略失败: %w", err)
	}
	defer rows.Close()
	out := map[string]config.PolicyConfig{}
	for rows.Next() {
		var name string
		var pc config.PolicyConfig
		if err := rows.Scan(&name, &pc.Filter, &pc.Score); err != nil {
			return nil, fmt.Errorf("解析持久化策略行失败: %w", err)
		}
		out[name] = pc
	}
	return out, rows.Err()
}

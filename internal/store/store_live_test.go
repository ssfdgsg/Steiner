// 真实数据库端到端验证（建表 + upsert + 加载 + 删除全链路）。
// 默认跳过；本地/CI 提供环境变量后启用：
//
//	GATEWAY_TEST_PG_DSN=postgres://user:pass@127.0.0.1:5432/gw?sslmode=disable go test ./internal/store -run Live
//	GATEWAY_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/gw?parseTime=true' go test ./internal/store -run Live
package store

import (
	"context"
	"os"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func liveRoundTrip(t *testing.T, driver, dsn string) {
	s, err := Open(config.StoreConfig{
		Enabled: true, Driver: driver, DSN: dsn,
		MaxOpenConns: 2, OpTimeout: config.Duration(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", driver, err)
	}
	defer s.Close()
	ctx := context.Background()

	bc := sampleBackend()
	bc.ID = "live-b1"
	if err := s.UpsertBackend(ctx, bc, []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	// 二次 upsert 走更新分支。
	bc.Weight = 2
	if err := s.UpsertBackend(ctx, bc, []string{"m1", "m2"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Backend.ID == "live-b1" {
			found = true
			if r.Backend.Weight != 2 || len(r.Models) != 2 {
				t.Fatalf("更新未生效: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("未读回写入的后端")
	}
	if err := s.DeleteBackend(ctx, "live-b1"); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertPolicy(ctx, "live-p1", "healthy", "running"); err != nil {
		t.Fatal(err)
	}
	pols, err := s.ListPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pols["live-p1"].Score != "running" {
		t.Fatalf("策略读回不符: %+v", pols)
	}
}

func TestLivePostgres(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GATEWAY_TEST_PG_DSN，跳过真实 PostgreSQL 验证")
	}
	liveRoundTrip(t, "postgres", dsn)
}

func TestLiveMySQL(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 GATEWAY_TEST_MYSQL_DSN，跳过真实 MySQL 验证")
	}
	liveRoundTrip(t, "mysql", dsn)
}

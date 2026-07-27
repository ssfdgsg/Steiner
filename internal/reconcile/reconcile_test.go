// 对账器单测：用 sqlmock 提供 DB 事实来源，验证错过广播后的三类收敛
// （补注册、补摘除、补策略）与已同步状态的幂等（不换代实例）。
package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/config"
	"ai-gateway/internal/policy"
	"ai-gateway/internal/store"
)

// fixture 构造：YAML 基线 yaml-b + 路由 m1；策略引擎含 default。
func newFixture(t *testing.T) (*backend.Registry, *policy.Engine) {
	t.Helper()
	cfg := &config.Config{
		Backends: []config.BackendConfig{{ID: "yaml-b", URL: "http://127.0.0.1:1", Engine: "vllm"}},
		Models:   []config.ModelRoute{{Name: "m1", Backends: []string{"yaml-b"}}},
	}
	cfg.ApplyDefaults()
	reg, err := backend.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	pol := policy.NewEngine()
	if err := pol.Set("default", "healthy", "waiting * 1.0"); err != nil {
		t.Fatalf("初始化策略失败: %v", err)
	}
	return reg, pol
}

// backendRows 构造 ListBackends 的结果集。
func backendRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "url", "engine", "weight", "max_concurrency",
		"metrics_path", "health_path", "bootstrap_port", "labels", "models"})
	for _, id := range ids {
		rows.AddRow(id, "http://127.0.0.1:9", "vllm", 1.0, 0,
			"/metrics", "/health", 8998, "{}", `["m1"]`)
	}
	return rows
}

func expectRound(mock sqlmock.Sqlmock, backends *sqlmock.Rows, policies *sqlmock.Rows) {
	mock.ExpectQuery("SELECT id, url, engine").WillReturnRows(backends)
	mock.ExpectQuery("SELECT name, filter, score").WillReturnRows(policies)
}

func TestReconcileConverges(t *testing.T) {
	reg, pol := newFixture(t)
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("构造 sqlmock 失败: %v", err)
	}
	defer db.Close()
	st := store.NewWithDB(db, "postgres", time.Second)

	// 预置一个"错过删除广播"的动态后端：本地有、DB 无。
	if _, err := reg.AddBackend(config.BackendConfig{
		ID: "dyn-old", URL: "http://127.0.0.1:2", Engine: "vllm",
	}, []string{"m1"}); err != nil {
		t.Fatalf("预置动态后端失败: %v", err)
	}

	r := New(st, reg, pol, []config.BackendConfig{{ID: "yaml-b"}}, time.Minute)

	// 第一轮：DB 含 dyn-1（本地没有，错过了注册广播）与新版 default 策略。
	polRows := sqlmock.NewRows([]string{"name", "filter", "score"}).
		AddRow("default", "", "running * 2.0")
	expectRound(mock, backendRows("dyn-1"), polRows)
	r.Once(context.Background())

	if reg.Get("dyn-1") == nil {
		t.Fatal("错过注册广播的后端应被对账补齐")
	}
	if reg.Get("dyn-old") != nil {
		t.Fatal("错过删除广播的动态后端应被对账摘除")
	}
	if reg.Get("yaml-b") == nil {
		t.Fatal("YAML 基线后端不应因 DB 缺失被摘除")
	}
	rt, err := reg.Route("m1")
	if err != nil {
		t.Fatalf("查路由失败: %v", err)
	}
	found := false
	for _, b := range rt.Pool() {
		if b.ID == "dyn-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("补齐的后端应加入声明的模型路由池")
	}
	if got := pol.List()["default"]["score"]; got != "running * 2.0" {
		t.Fatalf("错过热更新广播的策略应被对账收敛，实际 score=%q", got)
	}

	// 第二轮：DB 状态不变 → 幂等，不应换代实例（指针不变，无池抖动）。
	before := reg.Get("dyn-1")
	polRows2 := sqlmock.NewRows([]string{"name", "filter", "score"}).
		AddRow("default", "", "running * 2.0")
	expectRound(mock, backendRows("dyn-1"), polRows2)
	r.Once(context.Background())
	if reg.Get("dyn-1") != before {
		t.Fatal("已同步的后端不应被重复 Upsert 换代")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock 预期未满足: %v", err)
	}
}

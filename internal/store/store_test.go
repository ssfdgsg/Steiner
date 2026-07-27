// 持久层单测：用 sqlmock 双方言验证 SQL 语法分支与参数绑定；
// 真实数据库的端到端验证见 store_live_test.go（环境变量门控）。
package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"ai-gateway/internal/config"
)

func newMock(t *testing.T, driver string) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("构造 sqlmock 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewWithDB(db, driver, time.Second), mock
}

func sampleBackend() config.BackendConfig {
	return config.BackendConfig{
		ID: "b9", URL: "http://10.0.0.9:8000", Engine: "vllm", Weight: 1,
		MaxConcurrency: 32, MetricsPath: "/metrics", HealthPath: "/health",
		BootstrapPort: 8998, Labels: map[string]string{"zone": "a"},
	}
}

func TestUpsertBackendPostgres(t *testing.T) {
	s, mock := newMock(t, "postgres")
	mock.ExpectExec(`INSERT INTO gateway_backends .*ON CONFLICT \(id\) DO UPDATE`).
		WithArgs("b9", "http://10.0.0.9:8000", "vllm", 1.0, 32, "/metrics", "/health", 8998,
			`{"zone":"a"}`, `["m1","m2"]`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpsertBackend(context.Background(), sampleBackend(), []string{"m1", "m2"}); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertBackendMySQL(t *testing.T) {
	s, mock := newMock(t, "mysql")
	mock.ExpectExec(`INSERT INTO gateway_backends .*ON DUPLICATE KEY UPDATE`).
		WithArgs("b9", "http://10.0.0.9:8000", "vllm", 1.0, 32, "/metrics", "/health", 8998,
			`{"zone":"a"}`, `["m1","m2"]`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpsertBackend(context.Background(), sampleBackend(), []string{"m1", "m2"}); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBackendPlaceholder(t *testing.T) {
	// postgres 用 $1，mysql 用 ?，删除语句必须按方言生成。
	for driver, pattern := range map[string]string{
		"postgres": `DELETE FROM gateway_backends WHERE id = \$1`,
		"mysql":    `DELETE FROM gateway_backends WHERE id = \?`,
	} {
		s, mock := newMock(t, driver)
		mock.ExpectExec(pattern).WithArgs("b9").WillReturnResult(sqlmock.NewResult(0, 1))
		if err := s.DeleteBackend(context.Background(), "b9"); err != nil {
			t.Fatalf("[%s] delete 失败: %v", driver, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("[%s] %v", driver, err)
		}
	}
}

func TestListBackends(t *testing.T) {
	s, mock := newMock(t, "postgres")
	mock.ExpectQuery(`SELECT id, url, engine, .* FROM gateway_backends ORDER BY id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "url", "engine", "weight", "max_concurrency",
			"metrics_path", "health_path", "bootstrap_port", "labels", "models",
		}).AddRow("b9", "http://10.0.0.9:8000", "vllm", 1.0, 32,
			"/metrics", "/health", 8998, `{"zone":"a"}`, `["m1"]`))
	rows, err := s.ListBackends(context.Background())
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Backend.ID != "b9" ||
		rows[0].Backend.Labels["zone"] != "a" || len(rows[0].Models) != 1 || rows[0].Models[0] != "m1" {
		t.Fatalf("行内容不符: %+v", rows)
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	s, mock := newMock(t, "mysql")
	mock.ExpectExec(`INSERT INTO gateway_policies .*ON DUPLICATE KEY UPDATE`).
		WithArgs("p1", "healthy", "waiting * 2.0", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpsertPolicy(context.Background(), "p1", "healthy", "waiting * 2.0"); err != nil {
		t.Fatalf("upsert 策略失败: %v", err)
	}
	mock.ExpectQuery(`SELECT name, filter, score FROM gateway_policies`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "filter", "score"}).
			AddRow("p1", "healthy", "waiting * 2.0"))
	got, err := s.ListPolicies(context.Background())
	if err != nil {
		t.Fatalf("list 策略失败: %v", err)
	}
	if p, ok := got["p1"]; !ok || p.Score != "waiting * 2.0" {
		t.Fatalf("策略内容不符: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

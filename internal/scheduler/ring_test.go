// M6 复现性单测（修复后语义）：一致性哈希环缓存签名必须基于稳定内容
// （候选 ID 排序后拼接做 fnv64a 哈希），而不是切片底层地址 &pool[0]——
// Go 不保证新池分配不复用已回收数组的地址，地址复用会命中陈旧环
// （一致性哈希对所有键坍缩为固定首候选）。空池必须显式报错而非 panic。
package scheduler

import (
	"testing"

	"ai-gateway/internal/backend"
	"ai-gateway/internal/policy"
)

// TestRingSignatureStableAcrossOrderAndAddress 相同 ID 集合（不同顺序、
// 不同实例地址）→ 签名相同。
func TestRingSignatureStableAcrossOrderAndAddress(t *testing.T) {
	b1, b2, b3 := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")

	// 同一池快照、不同顺序。
	sigOrderA, err := ringSignatureContent([]*backend.Backend{b1, b2, b3})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	sigOrderB, err := ringSignatureContent([]*backend.Backend{b3, b1, b2})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if sigOrderA != sigOrderB {
		t.Fatalf("相同 ID 集合不同顺序应同签名: %q != %q", sigOrderA, sigOrderB)
	}

	// 不同实例（地址不同、ID 相同）。
	b1x, b2x, b3x := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")
	sigInstB, err := ringSignatureContent([]*backend.Backend{b1x, b2x, b3x})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if sigOrderA != sigInstB {
		t.Fatalf("相同 ID 集合不同实例地址应同签名: %q != %q", sigOrderA, sigInstB)
	}
}

// TestRingSignatureDistinguishesIDSets ID 集合不同 → 签名不同。
func TestRingSignatureDistinguishesIDSets(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	b3 := newBackend(t, "b3")

	sigAB, err := ringSignatureContent([]*backend.Backend{b1, b2})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	sigABC, err := ringSignatureContent([]*backend.Backend{b1, b2, b3})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if sigAB == sigABC {
		t.Fatal("不同 ID 集合不应同签名")
	}
}

// TestRingSignatureEmptyPool 空池（nil 与空切片）不 panic，返回明确错误。
func TestRingSignatureEmptyPool(t *testing.T) {
	for _, pool := range [][]*backend.Backend{nil, {}} {
		if _, err := ringSignatureContent(pool); err == nil {
			t.Fatalf("空池应返回错误而非签名, pool=%v", pool)
		}
	}
}

// TestConsistentHashRebuildsOnInstanceReplacement 端到端：内容签名对同 ID
// 集合稳定，因此"同 ID 换实例"（注册表 UpsertBackend 语义）后必须依赖缓存项
// 的池快照逐元素指针校验触发环重建，否则缓存命中会返回已摘除的旧实例指针。
func TestConsistentHashRebuildsOnInstanceReplacement(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	s := New(policy.NewEngine(), nil, kvCfg())
	route := &backend.Route{Name: "m", Strategy: "consistent_hash"}
	route.SetPool([]*backend.Backend{b1, b2})

	req := &Request{Model: "m", SessionID: "user-7"}
	first, err := s.Pick(route, req, nil)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if first != b1 && first != b2 {
		t.Fatalf("首次选路应返回池内实例，实际得到外部指针 %p", first)
	}

	// 同 ID 换实例：SetPool 整体换入新实例切片（upsert 的 copy-on-write 语义）。
	b1x, b2x := newBackend(t, "b1"), newBackend(t, "b2")
	route.SetPool([]*backend.Backend{b1x, b2x})
	got, err := s.Pick(route, req, nil)
	if err != nil {
		t.Fatalf("换实例后选路失败: %v", err)
	}
	if got != b1x && got != b2x {
		t.Fatalf("同 ID 换实例后返回了旧实例指针（陈旧环未重建），got=%p (期望 b1x/b2x)", got)
	}
}

// 前缀树单测：插入/匹配/节点分裂/TTL 清理。
package kvcache

import (
	"strings"
	"testing"
	"time"
)

func TestInsertAndMatch(t *testing.T) {
	tr := NewTree(4096, time.Minute)
	tr.Insert("system:你是助手\nuser:你好", "b1")

	best, keyLen := tr.Match("system:你是助手\nuser:你好")
	if keyLen == 0 {
		t.Fatal("keyLen 不应为 0")
	}
	if best["b1"] != keyLen {
		t.Fatalf("完整匹配期望 %d，实际 %d", keyLen, best["b1"])
	}
}

func TestPartialMatchAndSplit(t *testing.T) {
	tr := NewTree(4096, time.Minute)
	tr.Insert("abcdef", "b1")
	tr.Insert("abcxyz", "b2") // 触发在 "abc" 处分裂

	best, _ := tr.Match("abcdqq")
	// b1 匹配 "abcd"（4 字节），b2 匹配到分裂点 "abc"（3 字节）。
	if best["b1"] != 4 {
		t.Fatalf("b1 期望匹配 4 字节，实际 %d", best["b1"])
	}
	if best["b2"] != 3 {
		t.Fatalf("b2 期望匹配 3 字节，实际 %d", best["b2"])
	}
}

func TestMultiOwnerDepth(t *testing.T) {
	tr := NewTree(4096, time.Minute)
	tr.Insert("common-prefix-AAAA", "b1")
	tr.Insert("common-prefix-BBBB", "b2")

	best, _ := tr.Match("common-prefix-AAAA")
	if best["b1"] <= best["b2"] {
		t.Fatalf("b1 应比 b2 匹配更深: b1=%d b2=%d", best["b1"], best["b2"])
	}
	if best["b2"] != len("common-prefix-") {
		t.Fatalf("b2 应匹配公共前缀长度 %d，实际 %d", len("common-prefix-"), best["b2"])
	}
}

func TestClipLongPrefix(t *testing.T) {
	tr := NewTree(16, time.Minute)
	long := strings.Repeat("x", 100)
	tr.Insert(long, "b1")
	best, keyLen := tr.Match(long)
	if keyLen != 16 {
		t.Fatalf("keyLen 应被截断到 16，实际 %d", keyLen)
	}
	if best["b1"] != 16 {
		t.Fatalf("匹配长度应为 16，实际 %d", best["b1"])
	}
}

func TestPruneExpires(t *testing.T) {
	tr := NewTree(4096, 10*time.Millisecond)
	tr.Insert("hello-world", "b1")
	if st := tr.Size(); st.Nodes == 0 {
		t.Fatal("插入后节点数不应为 0")
	}
	time.Sleep(20 * time.Millisecond)
	st := tr.Prune()
	if st.Nodes != 0 || st.Bytes != 0 {
		t.Fatalf("TTL 过期后应清空，实际 nodes=%d bytes=%d", st.Nodes, st.Bytes)
	}
	best, _ := tr.Match("hello-world")
	if len(best) != 0 {
		t.Fatalf("清理后不应再有匹配: %v", best)
	}
}

func TestPruneKeepsFresh(t *testing.T) {
	tr := NewTree(4096, time.Hour)
	tr.Insert("keep-me", "b1")
	st := tr.Prune()
	if st.Nodes == 0 {
		t.Fatal("未过期的记录不应被清理")
	}
}

func TestEmptyInsertNoop(t *testing.T) {
	tr := NewTree(4096, time.Minute)
	tr.Insert("", "b1")
	if st := tr.Size(); st.Nodes != 0 {
		t.Fatalf("空前缀不应产生节点: %+v", st)
	}
}

// TestH8BudgetEvictsWhenExceeded H8 回归(缺陷翻转):修复前树无预算概念,
// 插入远超规模的条目全量常驻(1 万条 ≈ 1.1 万节点、481KB 无回收,实证见
// 缺陷复现);修复后节点/字节超限即逐代淘汰最久未访问归属,树规模有界。
func TestH8BudgetEvictsWhenExceeded(t *testing.T) {
	tr := NewTreeWithBudget(4096, time.Minute, 4, 1<<20) // 节点上限 4,字节上限 1MiB(不触发)
	if st := tr.Size(); st.Nodes != 0 || st.Bytes != 0 {
		t.Fatalf("初始树应为空: %+v", st)
	}
	// 首字节互异的独立前缀：每条恰好 1 个节点，节点数 == 插入条数，便于断言。
	keys := []string{"a-0001-AAAA", "b-0002-BBBB", "c-0003-CCCC", "d-0004-DDDD",
		"e-0005-EEEE", "f-0006-FFFF", "g-0007-GGGG", "h-0008-HHHH"}
	for i, k := range keys {
		tr.Insert(k, ownerOf(i))
		// 时间戳拉开 1ms：确保各归属"最近访问时间"可区分（LRU 淘汰判据）。
		time.Sleep(time.Millisecond)
	}
	st := tr.Size()
	if st.Nodes > 4 {
		t.Fatalf("H8 修复失败：超限后节点数 %d 仍超出预算 4", st.Nodes)
	}
	// 最久未访问者优先淘汰：最后插入的 keys[7] 是全局最新归属，必被保留。
	if best, _ := tr.Match(keys[7]); best[ownerOf(7)] == 0 {
		t.Fatalf("最新前缀应保留: %v", best)
	}
	// 淘汰不破坏后续 Insert/Match（不 panic 不退化）。
	for _, k := range keys {
		_, _ = tr.Match(k)
	}
	// 划定预算后一切照常：再插入仍受控。
	tr.Insert("z-new-0009", "o9")
	if st := tr.Size(); st.Nodes > 4 {
		t.Fatalf("持续插入后节点数 %d 仍超出预算 4", st.Nodes)
	}
}

// TestH8BudgetBytesCapped H8 字节上限：maxBytes 触顶时同样淘汰，Bytes 有界。
func TestH8BudgetBytesCapped(t *testing.T) {
	// 每条 8 字节独立前缀；maxBytes=16 容 2 条。节点上限放很大，只测字节维度。
	tr := NewTreeWithBudget(4096, time.Minute, 1<<20, 16)
	for _, k := range []string{"a-00001", "b-00002", "c-00003", "d-00004", "e-00005",
		"f-00006", "g-00007", "h-00008", "i-00009", "j-00010"} {
		tr.Insert(k, "o")
		time.Sleep(time.Millisecond)
		if st := tr.Size(); st.Bytes > 16 {
			t.Fatalf("H8 修复失败：字节数 %d 超出预算 16（nodes=%d）", st.Bytes, st.Nodes)
		}
	}
}

// TestH8BudgetHandler 回归锁定：预算参数为 0 表示该维度不限（兼容既有
// NewTree 调用方），不得把 0 理解为"建空树"。
func TestH8BudgetZeroUnlimited(t *testing.T) {
	tr := NewTreeWithBudget(4096, time.Minute, 0, 0)
	for _, k := range []string{"a-0", "b-1", "c-2", "d-3"} {
		tr.Insert(k, "o")
	}
	if st := tr.Size(); st.Nodes != 4 {
		t.Fatalf("0 预算应不限容量,实际 nodes=%d", st.Nodes)
	}
	// 与旧构造器完全等价。
	legacy := NewTree(4096, time.Minute)
	legacy.Insert("x-y-z", "o")
	if st := legacy.Size(); st.Nodes != 1 {
		t.Fatalf("NewTree 应保持不限容量: nodes=%d", st.Nodes)
	}
}

// TestL4RemoveBackedBy L4 回归(缺陷翻转)：修复前树无按后端清理归属的方法，
// 摘除后该后端 owners 残留至 TTL 过期（实测 match 仍返回已摘除 ID，且同 ID
// 重注册会"继承"死归属）；修复后 RemoveBackedBy 立即清空倒逼正确性。
func TestL4RemoveBackedBy(t *testing.T) {
	tr := NewTree(4096, time.Hour) // TTL 1 小时：若不清除，残留窗口极长
	tr.Insert("system:公共提示词\nuser:你好", "b1")
	tr.Insert("system:公共提示词\nuser:再见", "b2")
	tr.Insert("完全不同的另一条前缀", "b3")

	before := tr.Size()
	if before.Nodes == 0 {
		t.Fatal("插入后树不应为空")
	}

	st := tr.RemoveBackedBy("b1")
	if st.Nodes >= before.Nodes {
		t.Fatalf("移除后规模应减小: before=%+v after=%+v", before, st)
	}
	// b1 的归属（含共享中间节点上的条目）全部消失。
	if best, _ := tr.Match("system:公共提示词\nuser:你好"); best["b1"] != 0 {
		t.Fatalf("L4 修复失败：摘除后树内仍残留 b1 归属: %v", best)
	}
	// 其他后端不受影响。
	if best, _ := tr.Match("system:公共提示词\nuser:再见"); best["b2"] == 0 {
		t.Fatalf("b2 归属不应被误删: %v", best)
	}
	if best, _ := tr.Match("完全不同的另一条前缀"); best["b3"] == 0 {
		t.Fatalf("b3 归属不应被误删: %v", best)
	}
	// 同 ID 重注册：不再"继承"死归属，重新服务后重建亲和。
	tr.Insert("system:公共提示词\nuser:你好", "b1")
	if best, _ := tr.Match("system:公共提示词\nuser:你好"); best["b1"] == 0 {
		t.Fatalf("重新注册的 b1 应能建立新归属: %v", best)
	}
}

// TestL4RemoveBackedByUnknownOrEmpty L4 边界：清理不存在的后端 ID 不改变
// 树状态；空树清理不 panic。
func TestL4RemoveBackedByUnknownOrEmpty(t *testing.T) {
	tr := NewTree(4096, time.Hour)
	tr.Insert("prefix-x", "b1")
	before := tr.Size()
	if st := tr.RemoveBackedBy("不存在"); st != before {
		t.Fatalf("清除无归属的 ID 不应改变规模: before=%+v after=%+v", before, st)
	}
	empty := NewTree(4096, time.Hour)
	empty.RemoveBackedBy("b1") // 不得 panic
	if st := empty.Size(); st.Nodes != 0 {
		t.Fatalf("空树清理后应仍为空: %+v", st)
	}
}

// ownerOf 测试辅助：n -> 后端 ID（与 keys 索引一一对应）。
func ownerOf(i int) string {
	return string(rune('o')) + string(rune('1'+i))
}

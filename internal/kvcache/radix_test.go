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

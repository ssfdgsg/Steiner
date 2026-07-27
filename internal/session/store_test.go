// 会话粘性存储单测：绑定/查询/滑动续期/TTL 过期/后端失效解绑/容量淘汰。
package session

import (
	"fmt"
	"testing"
	"time"
)

func TestBindAndLookup(t *testing.T) {
	s := NewStore(time.Minute, 1000)
	s.Bind("sess-1", "b1")
	got, ok := s.Lookup("sess-1")
	if !ok || got != "b1" {
		t.Fatalf("期望命中 b1，实际 %q ok=%v", got, ok)
	}
	if _, ok := s.Lookup("sess-unknown"); ok {
		t.Fatal("未绑定的会话不应命中")
	}
}

func TestRebind(t *testing.T) {
	s := NewStore(time.Minute, 1000)
	s.Bind("sess-1", "b1")
	s.Bind("sess-1", "b2")
	if got, _ := s.Lookup("sess-1"); got != "b2" {
		t.Fatalf("改绑后应指向 b2，实际 %q", got)
	}
}

func TestTTLExpiry(t *testing.T) {
	s := NewStore(20*time.Millisecond, 1000)
	s.Bind("sess-1", "b1")
	time.Sleep(30 * time.Millisecond)
	if _, ok := s.Lookup("sess-1"); ok {
		t.Fatal("过期绑定不应命中")
	}
}

func TestSlidingRenewal(t *testing.T) {
	s := NewStore(50*time.Millisecond, 1000)
	s.Bind("sess-1", "b1")
	// 持续访问应不断续期，总时长超过单个 TTL 仍应命中。
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if _, ok := s.Lookup("sess-1"); !ok {
			t.Fatalf("第 %d 次滑动续期后不应过期", i)
		}
	}
}

func TestInvalidateBackend(t *testing.T) {
	s := NewStore(time.Minute, 1000)
	s.Bind("sess-1", "b1")
	s.Bind("sess-2", "b1")
	s.Bind("sess-3", "b2")
	s.InvalidateBackend("b1")
	if _, ok := s.Lookup("sess-1"); ok {
		t.Fatal("b1 失效后 sess-1 应解绑")
	}
	if _, ok := s.Lookup("sess-2"); ok {
		t.Fatal("b1 失效后 sess-2 应解绑")
	}
	if got, ok := s.Lookup("sess-3"); !ok || got != "b2" {
		t.Fatal("b2 的绑定不应受影响")
	}
}

func TestCapacityEviction(t *testing.T) {
	// 全局容量 64（每分片 1）：同分片第二个键会淘汰最旧键。
	s := NewStore(time.Minute, 64)
	for i := 0; i < 1000; i++ {
		s.Bind(fmt.Sprintf("sess-%d", i), "b1")
	}
	if n := s.Len(); n > 64 {
		t.Fatalf("容量上限失效：%d > 64", n)
	}
}

func TestEmptyKeyNoop(t *testing.T) {
	s := NewStore(time.Minute, 100)
	s.Bind("", "b1")
	if s.Len() != 0 {
		t.Fatal("空会话键不应产生绑定")
	}
}

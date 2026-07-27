// 前缀树基准：Insert 与 Match 的单次耗时（每请求热路径各调用一次）。
package kvcache

import (
	"fmt"
	"testing"
	"time"
)

// benchTree 预置 n 条前缀记录（4 个公共前缀族 × 变化尾部）。
func benchTree(n int) *Tree {
	t := NewTree(4096, 10*time.Minute)
	for i := 0; i < n; i++ {
		t.Insert(fmt.Sprintf("系统提示词族%d：你是一个乐于助人的助手。用户问题编号 %d", i%4, i),
			fmt.Sprintf("b%02d", i%16))
	}
	return t
}

func BenchmarkTreeInsert(b *testing.B) {
	t := benchTree(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.Insert(fmt.Sprintf("系统提示词族%d：你是一个乐于助人的助手。用户问题编号 %d", i%4, i), "b01")
	}
}

func BenchmarkTreeMatch(b *testing.B) {
	t := benchTree(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.Match(fmt.Sprintf("系统提示词族%d：你是一个乐于助人的助手。用户问题编号 %d 的后续", i%4, i))
	}
}

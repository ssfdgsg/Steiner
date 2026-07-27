// Package kvcache 实现"前缀 -> 后端归属"的压缩基数树（radix tree），
// 用于 KV cache 亲和路由：请求前缀曾被某后端服务过，则该后端大概率
// 仍持有对应的 KV cache 块，路由过去可显著提升前缀缓存命中率。
//
// 设计对齐 sglang-router 的 cache-aware 方案：
//   - 树节点记录 owners（后端 ID -> 最近访问时间）；
//   - 匹配返回每个后端的最长命中字节数；
//   - 按 TTL 周期清理过期归属，空叶子节点随之回收。
package kvcache

import (
	"sync"
	"time"
)

// node 压缩基数树节点。edge 为与父节点相连的边（非空，根除外）。
type node struct {
	edge     []byte
	children map[byte]*node
	owners   map[string]int64 // 后端 ID -> 最近访问 unix 纳秒
}

// Tree 线程安全的前缀归属树。
type Tree struct {
	mu         sync.Mutex
	root       *node
	maxPrefix  int
	ttl        time.Duration
	totalBytes int64
	nodeCount  int64
}

// Stats 树的规模统计。
type Stats struct {
	Bytes int64 `json:"bytes"`
	Nodes int64 `json:"nodes"`
}

// NewTree 构造前缀树。maxPrefix 限制单条前缀纳入的最大字节数，
// ttl 为归属记录的过期时间。
func NewTree(maxPrefix int, ttl time.Duration) *Tree {
	return &Tree{
		root:      &node{children: map[byte]*node{}, owners: map[string]int64{}},
		maxPrefix: maxPrefix,
		ttl:       ttl,
	}
}

// clip 截断到最大前缀长度。
func (t *Tree) clip(text string) []byte {
	if len(text) > t.maxPrefix {
		text = text[:t.maxPrefix]
	}
	return []byte(text)
}

// Insert 记录"该前缀由 owner 服务过"。沿途所有节点都会打上 owner 标记，
// 因此任意长度的前缀匹配都能归因到后端。
func (t *Tree) Insert(text, owner string) {
	key := t.clip(text)
	if len(key) == 0 {
		return
	}
	now := time.Now().UnixNano()

	t.mu.Lock()
	defer t.mu.Unlock()

	n := t.root
	for len(key) > 0 {
		child, ok := n.children[key[0]]
		if !ok {
			// 无公共分支：整段挂为新叶子。
			nn := &node{
				edge:     append([]byte(nil), key...),
				children: map[byte]*node{},
				owners:   map[string]int64{owner: now},
			}
			n.children[key[0]] = nn
			t.totalBytes += int64(len(key))
			t.nodeCount++
			return
		}
		cl := commonLen(child.edge, key)
		if cl == len(child.edge) {
			// 边完全匹配：下沉并标记归属。
			child.owners[owner] = now
			key = key[cl:]
			n = child
			continue
		}
		// 边部分匹配：在 cl 处分裂出中间节点。
		mid := &node{
			edge:     append([]byte(nil), child.edge[:cl]...),
			children: map[byte]*node{},
			owners:   copyOwners(child.owners),
		}
		mid.owners[owner] = now
		child.edge = child.edge[cl:]
		mid.children[child.edge[0]] = child
		n.children[key[0]] = mid
		t.nodeCount++
		if cl == len(key) {
			return
		}
		rest := key[cl:]
		nn := &node{
			edge:     append([]byte(nil), rest...),
			children: map[byte]*node{},
			owners:   map[string]int64{owner: now},
		}
		mid.children[rest[0]] = nn
		t.totalBytes += int64(len(rest))
		t.nodeCount++
		return
	}
}

// Match 返回每个后端的最长前缀命中字节数，以及参与匹配的键长
// （键长 = min(len(text), maxPrefix)，可用于换算命中率）。
func (t *Tree) Match(text string) (map[string]int, int) {
	key := t.clip(text)
	keyLen := len(key)
	best := map[string]int{}
	if keyLen == 0 {
		return best, 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	n := t.root
	matched := 0
	for len(key) > 0 {
		child, ok := n.children[key[0]]
		if !ok {
			break
		}
		cl := commonLen(child.edge, key)
		if cl < len(child.edge) {
			// 停在边中间：该节点的归属者按部分匹配长度计。
			for o := range child.owners {
				best[o] = matched + cl
			}
			break
		}
		matched += cl
		key = key[cl:]
		for o := range child.owners {
			best[o] = matched
		}
		n = child
	}
	return best, keyLen
}

// Prune 清理过期归属并回收空节点，返回回收后的统计。
func (t *Tree) Prune() Stats {
	deadline := time.Now().Add(-t.ttl).UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneNode(t.root, deadline)
	return Stats{Bytes: t.totalBytes, Nodes: t.nodeCount}
}

// Size 返回当前统计。
func (t *Tree) Size() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Stats{Bytes: t.totalBytes, Nodes: t.nodeCount}
}

// pruneNode 后序遍历：先清理子树，再清理本节点归属，
// 无归属且无子节点的节点由父节点删除。
func (t *Tree) pruneNode(n *node, deadline int64) {
	for c, child := range n.children {
		t.pruneNode(child, deadline)
		if len(child.owners) == 0 && len(child.children) == 0 {
			delete(n.children, c)
			t.totalBytes -= int64(len(child.edge))
			t.nodeCount--
		}
	}
	for o, ts := range n.owners {
		if ts < deadline {
			delete(n.owners, o)
		}
	}
}

// RunPruner 周期清理，onStats 回调用于上报规模指标，可为 nil。
func (t *Tree) RunPruner(stop <-chan struct{}, interval time.Duration, onStats func(Stats)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			st := t.Prune()
			if onStats != nil {
				onStats(st)
			}
		}
	}
}

func commonLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func copyOwners(src map[string]int64) map[string]int64 {
	dst := make(map[string]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

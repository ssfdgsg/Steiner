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
// 热路径（Match 前缀匹配、Size 读统计）走 RLock 并发读，
// 写路径（Insert/Prune）走独占 Lock。
type Tree struct {
	mu         sync.RWMutex
	root       *node
	maxPrefix  int
	ttl        time.Duration
	totalBytes int64
	nodeCount  int64
	// maxNodes / maxBytes 内存硬上限（H8）：0 表示该维度不限。
	// 插入导致任一维度超限时按"最久未访问归属"逐代淘汰（见 enforceBudget），
	// 保证异常/高基数输入下树规模有界，不依赖 TTL 周期清理兜底。
	maxNodes int64
	maxBytes int64
}

// Stats 树的规模统计。
type Stats struct {
	Bytes int64 `json:"bytes"`
	Nodes int64 `json:"nodes"`
}

// NewTree 构造前缀树。maxPrefix 限制单条前缀纳入的最大字节数，
// ttl 为归属记录的过期时间。内存不设上限（兼容既有调用方，
// 生产装配请用 NewTreeWithBudget 启用 H8 硬上限）。
func NewTree(maxPrefix int, ttl time.Duration) *Tree {
	return NewTreeWithBudget(maxPrefix, ttl, 0, 0)
}

// NewTreeWithBudget 构造带内存硬上限的前缀树。
// maxNodes / maxBytes 为节点数与边字节数上限，任一维度超限时触发淘汰；
// 传 0 表示该维度不限。
func NewTreeWithBudget(maxPrefix int, ttl time.Duration, maxNodes, maxBytes int64) *Tree {
	return &Tree{
		root:      &node{children: map[byte]*node{}, owners: map[string]int64{}},
		maxPrefix: maxPrefix,
		ttl:       ttl,
		maxNodes:  maxNodes,
		maxBytes:  maxBytes,
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
	// 插入完成后检查内存硬上限（H8）：超限时逐代淘汰最久未访问的归属，
	// 为新插入腾出空间。defer 保证所有 return 路径（含分裂提前返回）都受检。
	defer t.enforceBudget()

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

	t.mu.RLock()
	defer t.mu.RUnlock()

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

// Size 返回当前统计（只读热路径，走 RLock）。
func (t *Tree) Size() Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Stats{Bytes: t.totalBytes, Nodes: t.nodeCount}
}

// enforceBudget H8 内存硬上限：任一维度（节点数/边字节数）超限时，
// 按"最久未访问归属"逐代淘汰 —— 每轮取全树最旧的归属时间戳，复用 TTL
// 剪枝删除该代际的全部归属并回收空节点，直到回到预算内。
// 持写锁调用（仅 Insert 触发），淘汰是低频安全阀：稳态每次至多淘汰
// 最旧的一代，Match/Prune 的复杂度不变。
func (t *Tree) enforceBudget() {
	for t.overBudget() {
		minTs := t.oldestOwnerTs(t.root)
		if minTs == 0 {
			// 无归属可淘汰（理论不可达：超限必然由归属产生）。
			return
		}
		// pruneNode 删除 ts < deadline 的归属；deadline = minTs+1 等价于
		// 删除 ts <= minTs 的全部归属（即全树最旧代际，可含多个节点）。
		t.pruneNode(t.root, minTs+1)
	}
}

// overBudget 是否超出任一内存维度上限（上限为 0 表示该维度不限）。
func (t *Tree) overBudget() bool {
	if t.maxNodes > 0 && t.nodeCount > t.maxNodes {
		return true
	}
	if t.maxBytes > 0 && t.totalBytes > t.maxBytes {
		return true
	}
	return false
}

// oldestOwnerTs 返回整棵树最旧的归属时间戳；树内无任何归属时返回 0。
func (t *Tree) oldestOwnerTs(n *node) int64 {
	var min int64
	for _, ts := range n.owners {
		if min == 0 || ts < min {
			min = ts
		}
	}
	for _, c := range n.children {
		if ts := t.oldestOwnerTs(c); ts != 0 && (min == 0 || ts < min) {
			min = ts
		}
	}
	return min
}

// RemoveBackedBy 摘除后端时批量清理树内该后端的全部死归属（L4）：
// 删除 owner == backendID 的条目并回收因此变空的节点。后端不再被
// 调度后其归属即为死数据，若等 TTL 过期会占用内存且同 ID 重注册时
// 把亲和路由引向尚未持有对应 KV cache 的新实例。
// 返回清理后的规模统计。持写锁、全树遍历，仅在后端摘除（低频）时调用。
func (t *Tree) RemoveBackedBy(backendID string) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeOwner(t.root, backendID)
	return Stats{Bytes: t.totalBytes, Nodes: t.nodeCount}
}

// removeOwner 后序遍历删除指定归属：先清理子树，再删除本节点归属，
// 无归属且无子节点的节点由父节点回收（与 pruneNode 同构）。
func (t *Tree) removeOwner(n *node, owner string) {
	for c, child := range n.children {
		t.removeOwner(child, owner)
		if len(child.owners) == 0 && len(child.children) == 0 {
			delete(n.children, c)
			t.totalBytes -= int64(len(child.edge))
			t.nodeCount--
		}
	}
	delete(n.owners, owner)
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

// 一致性哈希环：用于会话亲和路由。每个后端映射 64 个虚拟节点，
// 键（会话 ID / 提示词 / 模型名）落点顺时针找第一个可用后端。
package scheduler

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"

	"ai-gateway/internal/backend"
)

const vnodesPerBackend = 64

// maxRingCache 环缓存上限：超限整体清空重建（环构建为微秒级，简单胜过 LRU）。
const maxRingCache = 128

type ringPoint struct {
	hash uint64
	b    *backend.Backend
}

type ring struct {
	points []ringPoint
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// buildRing 由后端池构建哈希环。
func buildRing(pool []*backend.Backend) *ring {
	r := &ring{points: make([]ringPoint, 0, len(pool)*vnodesPerBackend)}
	for _, b := range pool {
		for i := 0; i < vnodesPerBackend; i++ {
			r.points = append(r.points, ringPoint{hash: hash64(b.ID + "#" + strconv.Itoa(i)), b: b})
		}
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i].hash < r.points[j].hash })
	return r
}

// get 返回键对应的后端；ok 谓词为 false 时继续顺时针找下一个。
func (r *ring) get(key string, ok func(*backend.Backend) bool) *backend.Backend {
	if len(r.points) == 0 {
		return nil
	}
	h := hash64(key)
	idx := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	for i := 0; i < len(r.points); i++ {
		p := r.points[(idx+i)%len(r.points)]
		if ok(p.b) {
			return p.b
		}
	}
	return nil
}

// pickHash 一致性哈希选路。环按"完整池"构建（保证键落点稳定），
// 可用性过滤在环上顺时针查找时进行。
func (s *Scheduler) pickHash(pool, candidates []*backend.Backend, req *Request) *backend.Backend {
	key := req.SessionID
	if key == "" {
		key = req.PromptText
	}
	if key == "" {
		key = req.Model
	}

	sig := ringSignature(pool)
	s.ringMu.Lock()
	rg, okCache := s.rings[sig]
	if !okCache {
		// 上限兜底：整体清空避免动态增删后端时缓存无界增长。
		if len(s.rings) >= maxRingCache {
			s.rings = map[string]*ring{}
		}
		rg = buildRing(pool)
		s.rings[sig] = rg
	}
	s.ringMu.Unlock()

	allowed := make(map[string]bool, len(candidates))
	for _, b := range candidates {
		allowed[b.ID] = true
	}
	if b := rg.get(key, func(b *backend.Backend) bool { return allowed[b.ID] }); b != nil {
		return b
	}
	return candidates[0]
}

// ringSignature 环缓存键：池切片的底层数组地址 + 长度。
// 池是 copy-on-write（Route/Split.SetPool 整体换新切片），同一快照地址稳定、
// 任何增删/替换（含同 ID 换实例）都产生新地址——天然解决两类问题：
//  1. 同 ID 换实例后旧环返回已摘除的旧指针（按 ID 签名无法区分）；
//  2. 每请求对全池做 O(N log N) 排序拼接签名的开销。
func ringSignature(pool []*backend.Backend) string {
	return fmt.Sprintf("%p:%d", &pool[0], len(pool))
}

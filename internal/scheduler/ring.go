// 一致性哈希环：用于会话亲和路由。每个后端映射 64 个虚拟节点，
// 键（会话 ID / 提示词 / 模型名）落点顺时针找第一个可用后端。
package scheduler

import (
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

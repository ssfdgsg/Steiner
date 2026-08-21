// 管理面聚合统计：直接从网关自身 Registry 汇总请求量、错误率与时延分布。
// 与 /metrics 同源同口径，不新增运行期计数状态，供控制台 GET /admin/stats 使用。
package metrics

import (
	"math"
	"sort"
	"strconv"

	dto "github.com/prometheus/client_model/go"
)

// Dist 时延分布（毫秒）。分位数由 Prometheus 直方图桶线性插值估算，
// 精度受桶边界限制，用于趋势判断而非 SLA 计算。
type Dist struct {
	Count float64 `json:"count"`
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

// LabelStat 按单一标签维度（模型 / 后端）聚合的请求统计。
type LabelStat struct {
	Name     string  `json:"name"`
	Requests float64 `json:"requests"`
	Errors   float64 `json:"errors"`
	AvgMs    float64 `json:"avg_ms"`
}

// Aggregate 网关级累计统计（进程启动以来的累计值）。
type Aggregate struct {
	RequestsTotal    float64            `json:"requests_total"`
	ErrorsTotal      float64            `json:"errors_total"`
	ErrorRate        float64            `json:"error_rate"`
	Retries          float64            `json:"retries_total"`
	RateLimited      float64            `json:"rate_limited_total"`
	PickErrors       float64            `json:"pick_errors_total"`
	PromptTokens     float64            `json:"prompt_tokens_total"`
	CompletionTokens float64            `json:"completion_tokens_total"`
	Latency          Dist               `json:"latency"`
	TTFT             Dist               `json:"ttft"`
	ByCode           map[string]float64 `json:"by_code"`
	ByModel          []LabelStat        `json:"by_model"`
	ByBackend        []LabelStat        `json:"by_backend"`
}

// Aggregate 汇总当前 Registry 中的网关指标。
func (g *Gateway) Aggregate() (Aggregate, error) {
	fams, err := g.Registry.Gather()
	if err != nil {
		return Aggregate{}, err
	}
	a := Aggregate{ByCode: map[string]float64{}}
	byModel := map[string]*LabelStat{}
	byBackend := map[string]*LabelStat{}
	// 时延按维度累计 sum/count，用于各维度平均值。
	modelDur := map[string][2]float64{}
	backendDur := map[string][2]float64{}
	var lat, ttft histAcc

	for _, f := range fams {
		switch f.GetName() {
		case "gateway_requests_total":
			for _, m := range f.GetMetric() {
				v := m.GetCounter().GetValue()
				lb := labelsOf(m)
				code := lb["code"]
				isErr := errorCode(code)
				a.RequestsTotal += v
				a.ByCode[code] += v
				if isErr {
					a.ErrorsTotal += v
				}
				ms := statOf(byModel, lb["model"])
				ms.Requests += v
				bs := statOf(byBackend, lb["backend"])
				bs.Requests += v
				if isErr {
					ms.Errors += v
					bs.Errors += v
				}
			}
		case "gateway_request_duration_seconds":
			for _, m := range f.GetMetric() {
				h := m.GetHistogram()
				lat.add(h)
				lb := labelsOf(m)
				addDur(modelDur, lb["model"], h)
				addDur(backendDur, lb["backend"], h)
			}
		case "gateway_time_to_first_byte_seconds":
			for _, m := range f.GetMetric() {
				ttft.add(m.GetHistogram())
			}
		case "gateway_retries_total":
			a.Retries += sumCounters(f)
		case "gateway_rate_limited_total":
			a.RateLimited += sumCounters(f)
		case "gateway_pick_errors_total":
			a.PickErrors += sumCounters(f)
		case "gateway_prompt_tokens_total":
			a.PromptTokens += sumCounters(f)
		case "gateway_completion_tokens_total":
			a.CompletionTokens += sumCounters(f)
		}
	}

	if a.RequestsTotal > 0 {
		a.ErrorRate = a.ErrorsTotal / a.RequestsTotal
	}
	a.Latency = lat.dist()
	a.TTFT = ttft.dist()
	a.ByModel = flatten(byModel, modelDur)
	a.ByBackend = flatten(byBackend, backendDur)
	return a, nil
}

// histAcc 跨序列累加直方图：桶按上界合并，用于全局分位数估算。
type histAcc struct {
	count   float64
	sum     float64
	buckets map[float64]float64
}

func (h *histAcc) add(src *dto.Histogram) {
	if src == nil {
		return
	}
	h.count += float64(src.GetSampleCount())
	h.sum += src.GetSampleSum()
	if h.buckets == nil {
		h.buckets = map[float64]float64{}
	}
	for _, b := range src.GetBucket() {
		h.buckets[b.GetUpperBound()] += float64(b.GetCumulativeCount())
	}
}

func (h *histAcc) dist() Dist {
	d := Dist{Count: h.count}
	if h.count <= 0 {
		return d
	}
	d.AvgMs = h.sum / h.count * 1000
	bounds := make([]float64, 0, len(h.buckets))
	for ub := range h.buckets {
		bounds = append(bounds, ub)
	}
	sort.Float64s(bounds)
	d.P50Ms = h.quantile(bounds, 0.50) * 1000
	d.P90Ms = h.quantile(bounds, 0.90) * 1000
	d.P95Ms = h.quantile(bounds, 0.95) * 1000
	d.P99Ms = h.quantile(bounds, 0.99) * 1000
	return d
}

// quantile 累计桶线性插值。落入 +Inf 桶时返回最大有限上界（无法插值）。
func (h *histAcc) quantile(bounds []float64, q float64) float64 {
	if len(bounds) == 0 || h.count <= 0 {
		return 0
	}
	rank := q * h.count
	var prevBound, prevCount float64
	for _, ub := range bounds {
		c := h.buckets[ub]
		if c < rank {
			prevBound, prevCount = ub, c
			continue
		}
		if math.IsInf(ub, 1) {
			return prevBound
		}
		span := c - prevCount
		if span <= 0 {
			return ub
		}
		return prevBound + (ub-prevBound)*(rank-prevCount)/span
	}
	return prevBound
}

func labelsOf(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// errorCode 判定请求是否计为失败：HTTP 状态码 >= 400，或非数字码（连接失败等）。
func errorCode(code string) bool {
	n, err := strconv.Atoi(code)
	if err != nil {
		return code != ""
	}
	return n >= 400
}

func statOf(m map[string]*LabelStat, name string) *LabelStat {
	if name == "" {
		name = "unknown"
	}
	if s, ok := m[name]; ok {
		return s
	}
	s := &LabelStat{Name: name}
	m[name] = s
	return s
}

func addDur(m map[string][2]float64, name string, h *dto.Histogram) {
	if name == "" {
		name = "unknown"
	}
	cur := m[name]
	m[name] = [2]float64{cur[0] + h.GetSampleSum(), cur[1] + float64(h.GetSampleCount())}
}

func sumCounters(f *dto.MetricFamily) float64 {
	var total float64
	for _, m := range f.GetMetric() {
		total += m.GetCounter().GetValue()
	}
	return total
}

// flatten 输出按请求量降序的稳定列表（同请求量按名称排序，便于前端渲染不跳动）。
func flatten(stats map[string]*LabelStat, dur map[string][2]float64) []LabelStat {
	out := make([]LabelStat, 0, len(stats))
	for name, s := range stats {
		if d, ok := dur[name]; ok && d[1] > 0 {
			s.AvgMs = d[0] / d[1] * 1000
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}

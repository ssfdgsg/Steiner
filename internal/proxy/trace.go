// 追踪辅助：tracer 获取与响应状态码捕获。
// 打点统一走 otel 全局 TracerProvider——未启用追踪时为 noop，开销可忽略，
// 因此代理层无需感知追踪开关（初始化见 internal/tracing）。
package proxy

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"ai-gateway/internal/tracing"
)

// tracer 返回网关统一 tracer。
func tracer() trace.Tracer { return otel.Tracer(tracing.TracerName) }

// statusWriter 捕获写出的状态码供根 span 记录。
// Unwrap 使 http.ResponseController 的 Flush 能力穿透包装（SSE 透传依赖）。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

// Status 已写出的状态码；尚未写出按 200 计。
func (s *statusWriter) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// Unwrap 供 http.ResponseController 取底层 writer。
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

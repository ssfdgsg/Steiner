// 追踪模块单测：OTel 装配路径（禁用/启用、传播器注册、资源属性、关闭函数）。
package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"ai-gateway/internal/config"
)

func tcfg() *config.TracingConfig {
	ins, ratio := true, 1.0
	return &config.TracingConfig{
		Enabled:     true,
		Endpoint:    "127.0.0.1:4318", // 不带 scheme：otlptracehttp.WithEndpoint 会自动补 http://
		Insecure:    &ins,
		SampleRatio: &ratio,
		ServiceName: "test-gw",
		Headers:     map[string]string{"Authorization": "Bearer x"},
	}
}

// TestSetupDisabled 禁用时不安装 SDK Provider，返回空关停函数且不报错。
func TestSetupDisabled(t *testing.T) {
	cfg := tcfg()
	cfg.Enabled = false
	shutdown, err := Setup(context.Background(), *cfg, "v1.0.0")
	if err != nil {
		t.Fatalf("禁用 Setup 不应报错: %v", err)
	}
	if shutdown == nil {
		t.Fatal("禁用 Setup 应返回关停函数")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("禁用 Setup 关停不应报错: %v", err)
	}
}

// TestSetupEnabled 启用时安装 SDK TracerProvider 并返回可调用关停函数。
// OTLP 导出器是惰性拨号的：无 Collector 在跑也能完成装配。
func TestSetupEnabled(t *testing.T) {
	cfg := tcfg()
	shutdown, err := Setup(context.Background(), *cfg, "v1.0.0")
	if err != nil {
		t.Fatalf("启用 Setup 失败: %v", err)
	}
	if shutdown == nil {
		t.Fatal("启用 Setup 应返回关停函数")
	}
	tp := otel.GetTracerProvider()
	tracer := tp.Tracer("tracing-test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	span.End()
	_ = ctx
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("关停失败: %v", err)
	}
}

// TestSetupRegistersPropagators 无论是否启用都注册 W3C TraceContext 传播器，
// 保证入站 traceparent 能透传上游。
func TestSetupRegistersPropagators(t *testing.T) {
	cfg := tcfg()
	cfg.Enabled = false
	if _, err := Setup(context.Background(), *cfg, "v1.0.0"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	p := otel.GetTextMapPropagator()
	// 组合传播器的 Fields() 应包含 traceparent。
	found := false
	for _, f := range p.Fields() {
		if f == "traceparent" {
			found = true
		}
	}
	if !found {
		t.Fatal("未注册 W3C TraceContext 传播器")
	}
	// 传播器可实际完成一次注入/提取。
	carrier := propagation.MapCarrier{}
	p.Inject(context.Background(), carrier)
	if carrier.Get("traceparent") != "" {
		t.Fatal("无根 span 时不应注入 traceparent")
	}
}

// TestSetupDisabledNoopTracer 禁用时全局 Provider 为 noop：打点不 panic。
func TestSetupDisabledNoopTracer(t *testing.T) {
	cfg := tcfg()
	cfg.Enabled = false
	if _, err := Setup(context.Background(), *cfg, "v1.0.0"); err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	_, span := otel.Tracer("noop-test").Start(context.Background(), "x")
	span.SetAttributes()
	span.End()
	<-time.After(0)
}

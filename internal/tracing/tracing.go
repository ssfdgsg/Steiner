// Package tracing 负责 OpenTelemetry 的初始化与全局装配。
//
// 设计取舍：业务代码统一通过 otel 全局 TracerProvider 打点——未启用追踪时
// 全局默认为 noop 实现，打点开销可忽略，因此代理层无需感知开关；
// 本包只在启用时把真实的 SDK Provider（OTLP/HTTP 导出 + 采样器 + 资源属性）
// 安装为全局，并注册 W3C TraceContext + Baggage 传播器。
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"ai-gateway/internal/config"
)

// TracerName 网关统一的 tracer 名称，proxy 等模块打点时使用。
const TracerName = "ai-gateway"

// Setup 按配置安装全局 TracerProvider，返回优雅退出时的关停函数。
// cfg.Enabled 为 false 时不做任何事（全局保持 noop），返回空关停函数。
func Setup(ctx context.Context, cfg config.TracingConfig, version string) (shutdown func(context.Context) error, err error) {
	// 无论是否启用都注册传播器：即便本实例不导出 span，
	// 也应把入站 traceparent 透传给上游，保持链路不断。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if *cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("初始化 OTLP 导出器失败: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("构建资源属性失败: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// ParentBased：父 span 已决定采样则跟随，根 span 按比例采样。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*cfg.SampleRatio))),
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(3*time.Second)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

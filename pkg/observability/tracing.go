package observability

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace" // 标准输出
	// "go.opentelemetry.io/otel/exporters/jaeger" // Jaeger 示例
	// "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc" // OTLP 示例
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0" // 使用最新的语义约定
	// "google.golang.org/grpc" // OTLP 需要
	// "google.golang.org/grpc/credentials/insecure" // OTLP 本地测试可能需要
)

// InitTracerProvider 初始化并设置全局的 TracerProvider
// 返回一个函数，用于在程序结束时关闭 TracerProvider
func InitTracerProvider(serviceName string) (func(context.Context), error) {
	ctx := context.Background()

	// --- 1. 创建资源 (Resource) ---
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName), // 设置服务名 (非常重要!)
			semconv.ServiceVersionKey.String("v0.1.0"), // 设置服务版本 (可选)
		),
	)
	if err != nil {
		log.Printf("创建 OTEL Resource 失败: %v\n", err)
		return nil, err
	}

	// --- 2. 配置 Exporter ---
	// 默认使用标准输出，方便本地查看追踪信息
	traceExporter, err := stdouttrace.New(
		stdouttrace.WithPrettyPrint(),
		// stdouttrace.WithWriter(os.Stdout), // 可以明确指定 writer
	)
	if err != nil {
		log.Printf("创建标准输出 Exporter 失败: %v\n", err)
		return nil, err
	}

	// 如果需要 Jaeger 或 OTLP，在这里替换或添加 Exporter 实现
	// ... (参考之前的 Jaeger/OTLP 示例代码) ...

	// --- 3. 配置 BatchSpanProcessor ---
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)

	// --- 4. 创建并注册 TracerProvider ---
	tp := sdktrace.NewTracerProvider(
		// sdktrace.WithBatcher(bsp), // 使用 Batcher
		sdktrace.WithSpanProcessor(bsp), // 更新：较新版本使用 WithSpanProcessor
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // 开发时始终采样
	)
	otel.SetTracerProvider(tp)

	// --- 5. 设置全局 Propagator ---
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	log.Printf("OpenTelemetry TracerProvider (服务: %s) 初始化完成，使用标准输出 Exporter。\n", serviceName)

	// 返回清理函数
	shutdownFunc := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		log.Printf("正在关闭 OpenTelemetry TracerProvider (服务: %s)...\n", serviceName)
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("关闭 TracerProvider 失败 (服务: %s): %v\n", serviceName, err)
		} else {
			log.Printf("OpenTelemetry TracerProvider (服务: %s) 已关闭\n", serviceName)
		}
	}

	return shutdownFunc, nil
}

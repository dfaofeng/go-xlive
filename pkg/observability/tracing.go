package observability

import (
	"context"
	"fmt"
	"go-xlive/configs" // <-- 新增: 导入 configs 包
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/propagation" // <-- 确保导入 propagation
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0" // 使用最新的语义约定
	// "google.golang.org/grpc" // OTLP 需要
	// "google.golang.org/grpc/credentials/insecure" // OTLP 本地测试可能需要
)

// InitTracerProvider 初始化并设置全局的 TracerProvider
// 返回一个函数，用于在程序结束时关闭 TracerProvider
// <-- 修改: 接受 OtelConfig 参数
func InitTracerProvider(serviceName string, cfg configs.OtelConfig) (func(context.Context), error) {
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

	// --- 修改: 从配置中获取 OTLP Endpoint ---
	otelEndpoint := cfg.OtlpEndpoint
	if otelEndpoint == "" {
		// 如果配置为空，可以尝试环境变量作为备选，或者直接报错
		otelEndpointEnv := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		if otelEndpointEnv != "" {
			otelEndpoint = otelEndpointEnv
			log.Printf("配置文件中 otel.otlp_endpoint 为空，使用环境变量 OTEL_EXPORTER_OTLP_ENDPOINT: %s\n", otelEndpoint)
		} else {
			// 如果配置和环境变量都为空，则报错或使用硬编码默认值（不推荐）
			// 这里选择报错，强制要求配置
			err := fmt.Errorf("otel.otlp_endpoint 未在配置文件中设置，且 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量也未设置")
			log.Println(err.Error())
			return nil, err
			// 或者使用硬编码默认值（不推荐）：
			// otelEndpoint = "localhost:4317"
			// log.Printf("otel.otlp_endpoint 和 OTEL_EXPORTER_OTLP_ENDPOINT 均未设置，使用硬编码默认值: %s\n", otelEndpoint)
		}
	}
	// --- OTLP Endpoint 获取结束 ---

	// 创建到 OTLP Collector 的 gRPC 连接
	// 生产环境应配置安全凭证 grpc.WithTransportCredentials(...)
	conn, err := grpc.DialContext(ctx, otelEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 本地测试使用不安全连接
		grpc.WithBlock(), // 阻塞直到连接成功或失败
	)
	if err != nil {
		log.Printf("连接 OTLP Exporter 失败 (%s): %v\n", otelEndpoint, err)
		return nil, fmt.Errorf("failed to dial OTLP exporter at %s: %w", otelEndpoint, err)
	}
	log.Printf("准备连接到 OTLP Exporter: %s\n", otelEndpoint)
	// 创建 OTLP Trace Exporter
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		conn.Close() // 创建 Exporter 失败时关闭连接
		log.Printf("创建 OTLP Trace Exporter 失败: %v\n", err)
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}
	log.Println("OTLP Trace Exporter 创建成功")

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
	// 使用标准的 W3C Trace Context 和 Baggage 传播器
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	log.Printf("OpenTelemetry TracerProvider (服务: %s) 初始化完成，使用 OTLP Exporter (%s)。\n", serviceName, otelEndpoint)

	// 返回清理函数
	shutdownFunc := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // 增加关闭超时时间
		defer cancel()
		log.Printf("正在关闭 OpenTelemetry TracerProvider (服务: %s)...\n", serviceName)

		// 按顺序关闭：Provider -> Exporter -> Connection
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("关闭 TracerProvider 失败 (服务: %s): %v\n", serviceName, err)
		} else {
			log.Printf("TracerProvider (服务: %s) 已关闭\n", serviceName)
		}

		// Exporter 的关闭由 BSP 触发，但显式关闭连接是好的实践
		if err := conn.Close(); err != nil {
			log.Printf("关闭 OTLP gRPC 连接失败 (服务: %s): %v\n", serviceName, err)
		} else {
			log.Printf("OTLP gRPC 连接 (服务: %s) 已关闭\n", serviceName)
		}
		log.Printf("OpenTelemetry (服务: %s) 关闭完成\n", serviceName)
	}

	return shutdownFunc, nil
}

// --- 新增: NATS 追踪上下文传播辅助函数 ---

// InjectNATSHeaders 将当前 Context 中的 OpenTelemetry 追踪上下文注入到 NATS Header (map[string]string) 中。
// Publisher 在发送消息前调用此函数。
func InjectNATSHeaders(ctx context.Context) map[string]string {
	headers := make(map[string]string)
	// 使用全局设置的 TextMapPropagator (通常是 W3C TraceContext)
	propagator := otel.GetTextMapPropagator()
	// 将上下文注入到 map carrier 中
	propagator.Inject(ctx, propagation.MapCarrier(headers))
	return headers
}

// ExtractNATSHeaders 从 NATS Header (map[string]string) 中提取 OpenTelemetry 追踪上下文。
// Subscriber 在收到消息后调用此函数，以创建包含父 Span 上下文的新 Context。
func ExtractNATSHeaders(ctx context.Context, headers map[string]string) context.Context {
	// 使用全局设置的 TextMapPropagator (通常是 W3C TraceContext)
	propagator := otel.GetTextMapPropagator()
	// 从 map carrier 中提取上下文
	return propagator.Extract(ctx, propagation.MapCarrier(headers))
}

package observability

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	// 注意：在这个版本中，我们不再需要导入 OTEL 相关包，因为追踪逻辑由 Stats Handler 处理
)

// --- 指标定义 ---
// 定义 Prometheus 指标 (使用 promauto 自动注册)
var (
	// gRPC 服务端收到的总请求数
	grpcServerRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_server_requests_total",                            // 指标名称
			Help: "Total number of gRPC requests received by the server.", // 指标帮助信息
		},
		[]string{"grpc_service", "grpc_method", "grpc_code"}, // 标签：服务名、方法名、gRPC状态码
	)
	// gRPC 服务端处理请求的延迟分布
	grpcServerRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_server_request_duration_seconds",              // 指标名称
			Help:    "Latency of gRPC requests processed by the server.", // 指标帮助信息
			Buckets: prometheus.DefBuckets,                               // 使用默认的延迟分桶 (也可以自定义)
		},
		[]string{"grpc_service", "grpc_method"}, // 标签：服务名、方法名
	)
	// gRPC 客户端发出的总请求数
	grpcClientRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_client_requests_total",
			Help: "Total number of gRPC requests sent by the client.",
		},
		[]string{"grpc_target_service", "grpc_target_method", "grpc_code"}, // 客户端指标使用 target 标签
	)
	// gRPC 客户端请求的延迟分布
	grpcClientRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_client_request_duration_seconds",
			Help:    "Latency of gRPC requests sent by the client.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"grpc_target_service", "grpc_target_method"},
	)
)

// --- HTTP Handler for Metrics ---

// NewMetricsHandler 返回一个标准的 http.Handler，用于暴露 Prometheus 指标
func NewMetricsHandler() http.Handler {
	return promhttp.Handler()
}

// --- gRPC Interceptors (仅 Metrics) ---
// 因为 OTEL 追踪现在通过 Stats Handler 实现，拦截器只需要负责 Metrics

// MetricsUnaryServerInterceptor 返回一个只记录 Prometheus 指标的 gRPC Unary 服务端拦截器
func MetricsUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		startTime := time.Now()
		// 调用实际的 RPC 处理程序
		resp, err := handler(ctx, req)
		// 解析服务名、方法名和状态码
		serviceName, methodName := parseFullMethod(info.FullMethod)
		statusCode := status.Code(err).String()
		duration := time.Since(startTime).Seconds()
		// 记录指标
		grpcServerRequestDuration.WithLabelValues(serviceName, methodName).Observe(duration)
		grpcServerRequestsTotal.WithLabelValues(serviceName, methodName, statusCode).Inc()
		return resp, err
	}
}

// MetricsUnaryClientInterceptor 返回一个只记录 Prometheus 指标的 gRPC Unary 客户端拦截器
func MetricsUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string, // gRPC method string: /package.service/method
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		startTime := time.Now()
		// 调用实际的 invoker
		err := invoker(ctx, method, req, reply, cc, opts...)
		// 解析目标服务名、方法名和状态码
		targetService, targetMethod := parseFullMethod(method)
		statusCode := status.Code(err).String()
		duration := time.Since(startTime).Seconds()
		// 记录指标
		grpcClientRequestDuration.WithLabelValues(targetService, targetMethod).Observe(duration)
		grpcClientRequestsTotal.WithLabelValues(targetService, targetMethod, statusCode).Inc()
		return err
	}
}

// --- 辅助函数 ---
// parseFullMethod 从 gRPC 的完整方法名中解析出服务名和方法名
func parseFullMethod(fullMethod string) (string, string) {
	fullMethod = strings.TrimPrefix(fullMethod, "/") // 移除可能的前导斜杠
	if i := strings.Index(fullMethod, "/"); i >= 0 {
		// 找到第一个斜杠，前面是服务名，后面是方法名
		return fullMethod[:i], fullMethod[i+1:]
	}
	// 处理 gRPC 健康检查的特殊情况
	if fullMethod == "grpc.health.v1.Health/Check" || fullMethod == "grpc.health.v1.Health/Watch" {
		return "grpc.health.v1.Health", strings.Split(fullMethod, "/")[1]
	}
	// 如果格式不符合预期，返回未知
	return "unknown_service", "unknown_method"
}

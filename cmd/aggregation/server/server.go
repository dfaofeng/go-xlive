// cmd/aggregation/server/server.go
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	// !!! 替换模块路径 !!!
	aggregationv1 "go-xlive/gen/go/aggregation/v1"
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/stats"
)

// GrpcServer struct
type GrpcServer struct {
	Server       *grpc.Server
	HealthServer *health.Server
	Listener     net.Listener
	Port         int
	Logger       *zap.Logger
}

// MetricsServer struct
type MetricsServer struct {
	Server *http.Server
	Logger *zap.Logger
	Port   int
}

// NewGrpcServer 创建 gRPC 服务器
func NewGrpcServer(logger *zap.Logger, port int, aggSvc aggregationv1.AggregationServiceServer) (*GrpcServer, error) {
	if port == 0 {
		port = 50055
	} // 默认端口
	listenAddr := fmt.Sprintf(":%d", port)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 gRPC 端口 %d 失败: %w", port, err)
	}

	// 定义 OTel 拦截器选项，包括过滤健康检查
	otelInterceptorOpts := []otelgrpc.Option{
		// otelgrpc.WithTracerProvider(otel.GetTracerProvider()), // 通常会自动获取全局 Provider
		// otelgrpc.WithPropagators(otel.GetTextMapPropagator()), // 通常会自动获取全局 Propagator
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
			// 过滤掉健康检查的追踪。返回 true 表示 *不* 追踪这个方法。
			return info.FullMethodName == "/grpc.health.v1.Health/Check" ||
				info.FullMethodName == "/grpc.health.v1.Health/Watch"
		}),
	}

	s := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			otelgrpc.UnaryServerInterceptor(otelInterceptorOpts...), // OTel Unary 拦截器
			observability.MetricsUnaryServerInterceptor(),           // Metrics Unary 拦截器
		),
		grpc.ChainStreamInterceptor( // <-- 新增: 添加 Stream 拦截器链
			otelgrpc.StreamServerInterceptor(otelInterceptorOpts...), // OTel Stream 拦截器
			// 如果需要，也可以添加 Stream 的 Metrics 拦截器
			// observability.MetricsStreamServerInterceptor(),
		),
	)

	aggregationv1.RegisterAggregationServiceServer(s, aggSvc) // 注册 Aggregation 服务
	reflection.Register(s)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus(aggregationv1.AggregationService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_SERVING)

	logger.Info("Aggregation gRPC 服务准备就绪", zap.String("address", lis.Addr().String()))
	return &GrpcServer{Server: s, HealthServer: healthServer, Listener: lis, Port: port, Logger: logger}, nil
}

// Run 启动 gRPC 服务器
func (gs *GrpcServer) Run() <-chan error { /* ... 与其他 server 包相同 ... */
	errChan := make(chan error, 1)
	go func() {
		gs.Logger.Info("启动 gRPC 服务器", zap.Int("port", gs.Port))
		if err := gs.Server.Serve(gs.Listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errChan <- fmt.Errorf("gRPC 服务器 Serve 失败: %w", err)
		}
		close(errChan)
	}()
	return errChan
}

// Shutdown 优雅关闭 gRPC 服务器
func (gs *GrpcServer) Shutdown(ctx context.Context) { /* ... 与其他 server 包相同 ... */
	gs.Logger.Info("正在关闭 gRPC 服务器...")
	gs.HealthServer.SetServingStatus(aggregationv1.AggregationService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	gs.HealthServer.Shutdown()
	done := make(chan struct{})
	go func() { gs.Server.GracefulStop(); close(done) }()
	select {
	case <-done:
		gs.Logger.Info("gRPC 服务器已优雅关闭")
	case <-ctx.Done():
		gs.Logger.Warn("gRPC 服务器优雅关闭超时", zap.Error(ctx.Err()))
		gs.Logger.Warn("...", zap.Error(ctx.Err()))
	}
}

// NewMetricsServer 创建 Prometheus 指标服务器
func NewMetricsServer(logger *zap.Logger, port int) *MetricsServer { /* ... 与其他 server 包相同 ... */
	addr := fmt.Sprintf(":%d", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.NewMetricsHandler())
	// 实现 /healthz 端点
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	return &MetricsServer{Server: &http.Server{Addr: addr, Handler: mux}, Logger: logger, Port: port}
}

// Run 启动指标服务器
func (ms *MetricsServer) Run() <-chan error { /* ... 与其他 server 包相同 ... */
	errChan := make(chan error, 1)
	go func() {
		ms.Logger.Info("启动 Metrics 服务器", zap.String("address", ms.Server.Addr))
		if err := ms.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("Metrics 服务器 ListenAndServe 失败: %w", err)
		}
		close(errChan)
	}()
	return errChan
}

// Shutdown 优雅关闭指标服务器
func (ms *MetricsServer) Shutdown(ctx context.Context) { /* ... 与其他 server 包相同 ... */
	ms.Logger.Info("正在关闭 Metrics 服务器...")
	if err := ms.Server.Shutdown(ctx); err != nil {
		ms.Logger.Error("Metrics 服务器关闭失败", zap.Error(err))
	} else {
		ms.Logger.Info("Metrics 服务器已优雅关闭")
	}
}

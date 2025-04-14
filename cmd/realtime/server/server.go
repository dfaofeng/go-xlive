// cmd/realtime/server/server.go
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	// !!! 替换模块路径 !!!
	realtimev1 "go-xlive/gen/go/realtime/v1"
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
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
func NewGrpcServer(logger *zap.Logger, port int, realtimeSvc realtimev1.RealtimeServiceServer) (*GrpcServer, error) {
	if port == 0 {
		port = 50056
	} // 设置默认端口
	listenAddr := fmt.Sprintf(":%d", port)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 gRPC 端口 %d 失败: %w", port, err)
	}

	otelServerHandler := otelgrpc.NewServerHandler( /* ... otel options ... */
		otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
		otelgrpc.WithPropagators(otel.GetTextMapPropagator()),
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { /* ... filter ... */
			return info.FullMethodName == "/grpc.health.v1.Health/Check" ||
				info.FullMethodName == "/grpc.health.v1.Health/Watch"
		}),
	)

	s := grpc.NewServer(
		grpc.StatsHandler(otelServerHandler),
		grpc.ChainUnaryInterceptor(observability.MetricsUnaryServerInterceptor()),
		// !!! 添加 Stream 拦截器 !!!
		grpc.ChainStreamInterceptor(
			otelgrpc.StreamServerInterceptor( // OTEL Stream Interceptor
				otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
				otelgrpc.WithPropagators(otel.GetTextMapPropagator()),
			),
			// TODO: 添加 Metrics Stream Interceptor (如果需要监控流式 RPC 指标)
			// observability.MetricsStreamServerInterceptor(),
		),
	)

	realtimev1.RegisterRealtimeServiceServer(s, realtimeSvc) // 注册 Realtime 服务
	reflection.Register(s)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus(realtimev1.RealtimeService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_SERVING)

	logger.Info("Realtime gRPC 服务准备就绪", zap.String("address", lis.Addr().String()))

	return &GrpcServer{Server: s, HealthServer: healthServer, Listener: lis, Port: port, Logger: logger}, nil
}

// Run 启动 gRPC 服务器
func (gs *GrpcServer) Run() <-chan error { /* ... 与其他 server 包相同 ... */
	errChan := make(chan error, 1)
	go func() {
		gs.Logger.Info("gRPC 服务器已启动", zap.Int("port", gs.Port))
		if err := gs.Server.Serve(gs.Listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errChan <- fmt.Errorf("启动 gRPC 服务失败: %w", err)
		}
		close(errChan)
	}()
	return errChan
}

// Shutdown 优雅关闭 gRPC 服务器
func (gs *GrpcServer) Shutdown(ctx context.Context) { /* ... 与其他 server 包相同 ... */
	gs.Logger.Info("正在优雅关闭 gRPC 服务器...")
	gs.HealthServer.SetServingStatus(realtimev1.RealtimeService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	gs.HealthServer.Shutdown()
	shutdownDone := make(chan struct{})
	go func() { gs.Server.GracefulStop(); close(shutdownDone) }()
	select {
	case <-shutdownDone:
		gs.Logger.Info("gRPC 服务器已关闭")
	case <-ctx.Done():
		gs.Logger.Warn("gRPC 服务器优雅关闭超时", zap.Error(ctx.Err()))
	}
}

// NewMetricsServer 创建 Prometheus 指标服务器
func NewMetricsServer(logger *zap.Logger, port int) *MetricsServer { /* ... 与其他 server 包相同 ... */
	addr := fmt.Sprintf(":%d", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.NewMetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { /*...*/ })
	return &MetricsServer{Server: &http.Server{Addr: addr, Handler: mux}, Logger: logger, Port: port}
}

// Run 启动指标服务器
func (ms *MetricsServer) Run() <-chan error { /* ... 与其他 server 包相同 ... */
	errChan := make(chan error, 1)
	go func() {
		ms.Logger.Info("启动 Prometheus 指标服务器", zap.String("address", ms.Server.Addr))
		if err := ms.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("启动 Prometheus 指标服务器失败: %w", err)
		}
		close(errChan)
	}()
	return errChan
}

// Shutdown 优雅关闭指标服务器
func (ms *MetricsServer) Shutdown(ctx context.Context) { /* ... 与其他 server 包相同 ... */
	ms.Logger.Info("正在关闭 Prometheus 指标服务器...")
	if err := ms.Server.Shutdown(ctx); err != nil {
		ms.Logger.Error("关闭 Prometheus 指标服务器失败", zap.Error(err))
	} else {
		ms.Logger.Info("Prometheus 指标服务器已关闭")
	}
}

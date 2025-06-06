// cmd/user/server/server.go
package server

import (
	"context"
	"errors"
	"fmt"
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/pkg/observability"
	"net"
	"net/http"

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
func NewGrpcServer(logger *zap.Logger, port int, userSvc userv1.UserServiceServer) (*GrpcServer, error) {
	if port == 0 {
		port = 50051
	} // 默认端口
	listenAddr := fmt.Sprintf(":%d", port)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 gRPC 端口 %d 失败: %w", port, err)
	}

	//otelServerHandler := otelgrpc.NewServerHandler(
	//	otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
	//	otelgrpc.WithPropagators(otel.GetTextMapPropagator()),
	//	otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
	//		return info.FullMethodName == "/grpc.health.v1.Health/Check" ||
	//			info.FullMethodName == "/grpc.health.v1.Health/Watch"
	//	}),
	//)
	// 定义 OTel 拦截器选项，包括之前的 Filter
	// 注意：现在 Filter 是作为 Interceptor 的一个选项传入
	otelInterceptorOpts := []otelgrpc.Option{
		// otelgrpc.WithTracerProvider(otel.GetTracerProvider()), // 通常会自动获取全局 Provider
		// otelgrpc.WithPropagators(otel.GetTextMapPropagator()), // 通常会自动获取全局 Propagator
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
			// 过滤掉健康检查的追踪。返回 true 表示 *不* 追踪这个方法。
			// 根据你的日志，被过滤掉的方法名是小写，所以这里也用小写比较保险
			// 如果你的 gRPC 库或 OTel 库版本较高，可能需要用 info.FullMethod
			// 请根据实际情况调整，可以通过打印 info.FullMethodName 来确认
			return info.FullMethodName == "/grpc.health.v1.Health/Check" ||
				info.FullMethodName == "/grpc.health.v1.Health/Watch"
		}),
	}
	s := grpc.NewServer(
		//grpc.StatsHandler(otelServerHandler), // StatsHandler 已被拦截器取代
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

	// 注册 User 服务
	userv1.RegisterUserServiceServer(s, userSvc)
	reflection.Register(s)

	// 注册健康检查服务
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus(userv1.UserService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_SERVING)

	logger.Info("User gRPC 服务准备就绪", zap.String("address", lis.Addr().String()))

	return &GrpcServer{
		Server:       s,
		HealthServer: healthServer,
		Listener:     lis,
		Port:         port,
		Logger:       logger,
	}, nil
}

// Run 启动 gRPC 服务器
func (gs *GrpcServer) Run() <-chan error {
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
func (gs *GrpcServer) Shutdown(ctx context.Context) {
	gs.Logger.Info("正在优雅关闭 gRPC 服务器...")
	gs.HealthServer.SetServingStatus(userv1.UserService_ServiceDesc.ServiceName, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	gs.HealthServer.Shutdown()
	shutdownDone := make(chan struct{})
	go func() {
		gs.Server.GracefulStop()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		gs.Logger.Info("gRPC 服务器已关闭")
	case <-ctx.Done():
		gs.Logger.Warn("gRPC 服务器优雅关闭超时", zap.Error(ctx.Err()))
	}
}

// NewMetricsServer 创建 Prometheus 指标服务器
func NewMetricsServer(logger *zap.Logger, port int) *MetricsServer {
	addr := fmt.Sprintf(":%d", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.NewMetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); w.Write([]byte("OK")) })
	return &MetricsServer{
		Server: &http.Server{Addr: addr, Handler: mux},
		Logger: logger,
		Port:   port,
	}
}

// Run 启动指标服务器
func (ms *MetricsServer) Run() <-chan error {
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
func (ms *MetricsServer) Shutdown(ctx context.Context) {
	ms.Logger.Info("正在关闭 Prometheus 指标服务器...")
	if err := ms.Server.Shutdown(ctx); err != nil {
		ms.Logger.Error("关闭 Prometheus 指标服务器失败", zap.Error(err))
	} else {
		ms.Logger.Info("Prometheus 指标服务器已关闭")
	}
}

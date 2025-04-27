package main

import (
	"context"
	"flag" // 使用标准 log
	"go-xlive/pkg/log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc" // <-- 新增: 导入 otelgrpc
	"google.golang.org/grpc"                                                      // <-- 新增: 导入 grpc
	"google.golang.org/grpc/credentials/insecure"                                 // <-- 新增: 导入 insecure

	// !!! 替换模块路径 !!!
	"go-xlive/cmd/room/client" // 导入 client 包
	"go-xlive/cmd/room/server" // 导入 server 包
	"go-xlive/configs"
	sessionv1 "go-xlive/gen/go/session/v1" // <-- 新增: 导入 session proto
	"go-xlive/internal/room/repository"
	roomservice "go-xlive/internal/room/service"
	"go-xlive/pkg/observability"

	"go.uber.org/zap"
)

const (
	serviceName = "room-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	metricsPort := flag.Int("metrics_port", 9092, "Prometheus 指标暴露端口")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		// 使用 zap logger 替换 log.Fatalf
		zap.L().Fatal("无法加载配置", zap.String("path", *configPath), zap.Error(err))
	}

	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("房间服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	// <-- 修改: 传递 cfg.Otel
	tpShutdown, err := observability.InitTracerProvider(serviceName, cfg.Otel)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	// !!! 确保在日志之后，客户端之前，尽早 defer !!!
	defer func() {
		// 使用带有超时的后台 context 进行关闭
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tpShutdown(shutdownCtx)
	}()
	logger.Info("TracerProvider 初始化完成")
	// --- 初始化基础设施连接 ---
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	dbpool, err := pgxpool.New(dbCtx, cfg.Database.PostgresDSN)
	if err != nil {
		logger.Fatal("无法连接到 PostgreSQL", zap.Error(err))
	}
	defer dbpool.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := dbpool.Ping(pingCtx); err != nil {
		logger.Fatal("无法 Ping PostgreSQL 数据库", zap.Error(err))
	}
	logger.Info("成功连接到 PostgreSQL")

	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 初始化 gRPC 客户端 ---
	// *** 定义通用的 gRPC 客户端 Dial 选项，包含 OTel 拦截器 ***
	grpcClientOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 生产应替换为安全凭证
		grpc.WithChainUnaryInterceptor(
			otelgrpc.UnaryClientInterceptor(), // 添加 OTel 追踪拦截器
		),
		grpc.WithChainStreamInterceptor( // 添加流拦截器
			otelgrpc.StreamClientInterceptor(),
		),
	}
	logger.Info("gRPC 客户端已配置 OTel 拦截器")

	// User Service Client
	userClientWrapper, err := client.NewUserClient(cfg.Client) // 使用 client 包创建
	if err != nil {
		logger.Fatal("初始化用户服务客户端失败", zap.Error(err))
	}
	defer userClientWrapper.Close() // 确保关闭连接

	// Session Service Client <-- 新增: 初始化 Session 客户端
	if len(cfg.Client.SessionService.Addresses) == 0 {
		logger.Fatal("Session Service 地址未在配置中找到")
	}
	sessionTarget := cfg.Client.SessionService.Addresses[0]
	sessionConn, err := grpc.Dial(sessionTarget, grpcClientOpts...) // <-- 使用通用选项
	if err != nil {
		logger.Fatal("连接 Session Service 失败", zap.String("target", sessionTarget), zap.Error(err))
	}
	defer sessionConn.Close()
	sessionClient := sessionv1.NewSessionServiceClient(sessionConn)
	logger.Info("成功连接到 Session Service", zap.String("target", sessionTarget))

	// --- 组装应用 ---
	roomRepo := repository.NewRepository(dbpool)
	// <-- 修改: 传递 sessionClient 给 NewRoomService
	roomSvc := roomservice.NewRoomService(logger, roomRepo, userClientWrapper.Client, sessionClient, natsConn)
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Room.GrpcPort, roomSvc)
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 启动服务器 ---
	grpcErrChan := grpcServer.Run()
	metricsErrChan := metricsServer.Run()

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("收到关闭信号", zap.String("signal", sig.String()))
	case err := <-grpcErrChan:
		if err != nil {
			logger.Error("gRPC 服务器异常退出", zap.Error(err))
		}
	case err := <-metricsErrChan:
		if err != nil {
			logger.Error("Prometheus 指标服务器异常退出", zap.Error(err))
		}
	}

	// 设置总的关闭超时
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownOverallCancel()

	// 按相反顺序关闭：先关闭接收外部请求的服务器，再关闭内部资源（客户端连接等已 defer）
	// 为 Metrics Server 设置单独的超时
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)

	// 为 gRPC Server 设置单独的超时
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 10*time.Second) // 调整超时
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx) // 内部包含 GracefulStop

	logger.Info("房间服务已关闭")
}

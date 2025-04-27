package main

import (
	"context"
	"flag"
	"go-xlive/pkg/log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"go-xlive/cmd/session/server"
	"go-xlive/cmd/session/subscriber"
	"go-xlive/configs"
	grpchandler "go-xlive/internal/session/handler/grpc" // <-- Corrected import
	"go-xlive/internal/session/repository"
	sessionservice "go-xlive/internal/session/service" // <-- Service 包现在只包含业务逻辑
	"go-xlive/pkg/observability"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	aggregationv1 "go-xlive/gen/go/aggregation/v1"
	eventv1 "go-xlive/gen/go/event/v1"
	roomv1 "go-xlive/gen/go/room/v1"
	// sessionv1 "go-xlive/gen/go/session/v1" // <-- 移除未使用的导入
)

const (
	serviceName = "session-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	metricsPort := flag.Int("metrics_port", 9093, "Prometheus 指标暴露端口")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		zap.L().Fatal("无法加载配置", zap.String("path", *configPath), zap.Error(err))
	}

	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("场次服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	tpShutdown, err := observability.InitTracerProvider(serviceName, cfg.Otel)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	defer func() {
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

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer redisCancel()
	if _, err := redisClient.Ping(redisCtx).Result(); err != nil {
		logger.Fatal("无法连接到 Redis", zap.Error(err))
	}
	defer redisClient.Close()
	logger.Info("成功连接到 Redis", zap.String("address", cfg.Redis.Addr))

	// --- 初始化 gRPC 客户端 (添加 OTel 拦截器) ---
	grpcClientOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(
			otelgrpc.UnaryClientInterceptor(),
		),
		// grpc.WithChainStreamInterceptor(otelgrpc.StreamClientInterceptor()), // 如果需要流拦截器
	}
	logger.Info("gRPC 客户端已配置 OTel 拦截器")

	// Room Service 客户端
	if len(cfg.Client.RoomService.Addresses) == 0 {
		logger.Fatal("Room Service 地址未在配置中找到")
	}
	roomTarget := cfg.Client.RoomService.Addresses[0]
	roomConn, err := grpc.Dial(roomTarget, grpcClientOpts...)
	if err != nil {
		logger.Fatal("连接 Room Service 失败", zap.String("target", roomTarget), zap.Error(err))
	}
	defer roomConn.Close()
	roomClient := roomv1.NewRoomServiceClient(roomConn)
	logger.Info("成功连接到 Room Service", zap.String("target", roomTarget))

	// Event Service 客户端
	if len(cfg.Client.EventService.Addresses) == 0 {
		logger.Fatal("Event Service 地址未在配置中找到")
	}
	eventTarget := cfg.Client.EventService.Addresses[0]
	eventConn, err := grpc.Dial(eventTarget, grpcClientOpts...)
	if err != nil {
		logger.Fatal("连接 Event Service 失败", zap.String("target", eventTarget), zap.Error(err))
	}
	defer eventConn.Close()
	eventClient := eventv1.NewEventServiceClient(eventConn)
	logger.Info("成功连接到 Event Service", zap.String("target", eventTarget))

	// Aggregation Service 客户端
	if len(cfg.Client.AggregationService.Addresses) == 0 {
		logger.Fatal("Aggregation Service 地址未在配置中找到")
	}
	aggregationTarget := cfg.Client.AggregationService.Addresses[0]
	aggregationConn, err := grpc.Dial(aggregationTarget, grpcClientOpts...)
	if err != nil {
		logger.Fatal("连接 Aggregation Service 失败", zap.String("target", aggregationTarget), zap.Error(err))
	}
	defer aggregationConn.Close()
	aggregationClient := aggregationv1.NewAggregationServiceClient(aggregationConn)
	logger.Info("成功连接到 Aggregation Service", zap.String("target", aggregationTarget))

	// --- 创建服务和处理器实例 ---
	sessionRepo := repository.NewRepository(dbpool)
	// 1. 创建业务逻辑服务实例
	sessionSvc := sessionservice.NewService(
		logger,
		sessionRepo,
		roomClient,
		eventClient,
		redisClient,
		natsConn,
		aggregationClient,
	)
	// 2. 创建 gRPC 处理器实例，注入业务服务
	sessionHdlr := grpchandler.NewGrpcHandler(sessionSvc, logger) // <-- 修正参数顺序：Service 在前，Logger 在后

	// --- 初始化 NATS 订阅者 ---
	// 订阅者需要业务逻辑服务来处理事件
	platformSubscriber := subscriber.NewPlatformEventSubscriber(logger, natsConn, sessionSvc) // <-- 类型错误将在下一步修复 subscriber 包

	// --- 创建服务器实例 ---
	// 将 gRPC 处理器注册到服务器
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Session.GrpcPort, sessionHdlr)
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 启动服务器和订阅者 ---
	mainCtx, mainCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(3) // gRPC Server, Metrics Server, NATS Subscriber

	// 启动 gRPC 服务器
	go func() {
		defer wg.Done()
		logger.Info("启动 gRPC 服务器...", zap.Int("port", cfg.Server.Session.GrpcPort))
		errCh := grpcServer.Run()
		if err := <-errCh; err != nil {
			logger.Error("gRPC 服务器异常退出", zap.Error(err))
			mainCancel()
		} else {
			logger.Info("gRPC 服务器正常关闭")
		}
	}()

	// 启动 Metrics 服务器
	go func() {
		defer wg.Done()
		logger.Info("启动 Prometheus 指标服务器...", zap.Int("port", *metricsPort))
		errCh := metricsServer.Run()
		if err := <-errCh; err != nil {
			logger.Error("Prometheus 指标服务器异常退出", zap.Error(err))
			mainCancel()
		} else {
			logger.Info("Prometheus 指标服务器正常关闭")
		}
	}()

	// 启动 NATS 订阅者
	go func() {
		defer wg.Done()
		logger.Info("启动平台事件 NATS 订阅者...")
		sessionSvc.StartSubscribers()
		if err := platformSubscriber.Start(mainCtx); err != nil && err != context.Canceled {
			logger.Error("平台事件 NATS 订阅者异常退出", zap.Error(err))
			mainCancel()
		}
		<-mainCtx.Done()
		sessionSvc.StopSubscribers()
		logger.Info("平台事件 NATS 订阅者已停止")
	}()

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("收到关闭信号", zap.String("signal", sig.String()))
	case <-mainCtx.Done():
		logger.Info("收到内部取消信号")
	}

	mainCancel()

	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownOverallCancel()

	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)

	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 10*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)

	logger.Info("等待后台 goroutine 完成...")
	wg.Wait()

	logger.Info("场次服务已关闭")
}

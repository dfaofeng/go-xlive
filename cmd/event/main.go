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

	"github.com/go-redis/redis/v8" // Import Redis client
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	// !!! 替换模块路径 !!!
	"go-xlive/cmd/event/server"     // 导入 server 包
	"go-xlive/cmd/event/subscriber" // 导入 subscriber 包
	"go-xlive/configs"

	"go-xlive/internal/event/repository"
	eventservice "go-xlive/internal/event/service"
	"go-xlive/pkg/observability"

	"go.uber.org/zap"
)

const (
	serviceName = "event-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "...")
	metricsPort := flag.Int("metrics_port", 9094, "...")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		zap.L().Fatal("加载配置失败", zap.String("path", *configPath), zap.Error(err))
	}

	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("事件服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	// <-- 修改: 传递 cfg.Otel
	tpShutdown, err := observability.InitTracerProvider(serviceName, cfg.Otel)
	if err != nil {
		logger.Fatal("初始化 Tracer Provider 失败", zap.Error(err))
	}
	defer func() {
		// 使用带有超时的后台 context 进行关闭
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tpShutdown(shutdownCtx)
	}()
	logger.Info("TracerProvider 初始化完成")

	// --- 初始化基础设施连接 ---
	// PostgreSQL
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	dbpool, err := pgxpool.New(dbCtx, cfg.Database.PostgresDSN)
	if err != nil {
		logger.Fatal("连接 PostgreSQL 失败", zap.String("dsn", cfg.Database.PostgresDSN), zap.Error(err))
	}
	defer dbpool.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := dbpool.Ping(pingCtx); err != nil {
		logger.Fatal("Ping PostgreSQL 失败", zap.Error(err))
	}
	logger.Info("成功连接到 PostgreSQL")

	// NATS
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("连接 NATS 失败", zap.String("url", cfg.Nats.URL), zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// Redis
	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer redisCancel()
	if _, err := redisClient.Ping(redisCtx).Result(); err != nil {
		logger.Fatal("连接 Redis 失败", zap.String("addr", cfg.Redis.Addr), zap.Error(err))
	}
	logger.Info("成功连接到 Redis")
	// defer redisClient.Close() // Consider if needed based on application lifecycle

	// --- 组装应用 ---
	eventRepo := repository.NewRepository(dbpool)
	// --- 移除 NATS 连接参数 ---
	eventSvc := eventservice.NewEventService(logger, eventRepo)
	// --- 修正: 传入 redisClient ---
	natsSubscriber := subscriber.NewNatsSubscriber(logger, natsConn, eventRepo, redisClient)
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Event.GrpcPort, eventSvc)
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 启动 NATS 订阅 ---
	natsCtx, natsCancel := context.WithCancel(context.Background())
	var natsWg sync.WaitGroup
	natsSubscriber.Start(natsCtx, &natsWg)
	logger.Info("EventService NATS 订阅者已启动")

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
			logger.Error("gRPC 服务器运行时出错", zap.Error(err))
		}
	case err := <-metricsErrChan:
		if err != nil {
			logger.Error("Metrics 服务器运行时出错", zap.Error(err))
		}
	}
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()
	logger.Info("正在通知 NATS 订阅者退出 (EventService)...")
	natsCancel()
	natsSubscriber.Shutdown()
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 15*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)
	logger.Info("正在等待 NATS 订阅者完成 (EventService)...")
	waitChan := make(chan struct{})
	go func() { natsWg.Wait(); close(waitChan) }()
	select {
	case <-waitChan:
		logger.Info("NATS 订阅者已完成 (EventService)")
	case <-shutdownOverallCtx.Done():
		logger.Warn("等待 NATS 订阅者完成超时", zap.Error(shutdownOverallCtx.Err()))
	}
	// Consider closing redisClient here if needed
	// redisClient.Close()
	logger.Info("事件服务已关闭")
}

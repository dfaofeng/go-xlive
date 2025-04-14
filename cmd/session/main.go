package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	"go-xlive/cmd/session/client" // 导入 client 包
	"go-xlive/cmd/session/server" // 导入 server 包
	"go-xlive/configs"
	// sessionv1 "go-xlive/gen/go/session/v1" // main 不直接用
	"go-xlive/internal/session/repository"
	sessionservice "go-xlive/internal/session/service"
	"go-xlive/pkg/observability"
	"go.uber.org/zap"
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
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	logger.Info("场次服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	tpShutdown, err := observability.InitTracerProvider(serviceName)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	defer tpShutdown(context.Background())
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

	// --- 初始化 gRPC 客户端 (Room Service) ---
	roomClientWrapper, err := client.NewRoomClient(cfg.Client)
	if err != nil {
		logger.Fatal("初始化房间服务客户端失败", zap.Error(err))
	}
	defer roomClientWrapper.Close()

	// --- 组装应用 ---
	sessionRepo := repository.NewRepository(dbpool)
	sessionSvc := sessionservice.NewSessionService(logger, sessionRepo, roomClientWrapper.Client, redisClient, natsConn) // 注入依赖
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Session.GrpcPort, sessionSvc)
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second) // 调整超时时间
	defer shutdownCancel()

	// 按相反顺序关闭
	metricsServer.Shutdown(shutdownCtx)
	grpcServer.Shutdown(shutdownCtx) // 内部包含 GracefulStop

	// NATS, DB, Redis, Room Client 的连接已在 defer 中处理

	logger.Info("场次服务已关闭")
}

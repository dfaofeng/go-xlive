package main

import (
	"context"
	"flag"
	"log" // 使用标准 log
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	"go-xlive/cmd/user/server" // 导入 server 包
	"go-xlive/configs"
	// userv1 "go-xlive/gen/go/user/v1" // main 不直接用
	"go-xlive/internal/user/repository"
	userservice "go-xlive/internal/user/service"
	"go-xlive/pkg/observability"
	"go.uber.org/zap"
)

const (
	serviceName = "user-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	metricsPort := flag.Int("metrics_port", 9091, "Prometheus 指标暴露端口")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	logger.Info("用户服务正在启动...", zap.String("service", serviceName))

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
	// User Service 不直接连接 Redis 或 NATS (根据当前设计)

	// --- 组装应用 ---
	userRepo := repository.NewRepository(dbpool)
	userSvc := userservice.NewUserService(logger, userRepo)
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.User.GrpcPort, userSvc)
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	// 按相反顺序关闭
	metricsServer.Shutdown(shutdownCtx)
	grpcServer.Shutdown(shutdownCtx)

	// 其他资源 (DB, Tracer) 已在 defer 中处理

	logger.Info("用户服务已关闭")
}

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
	"github.com/nats-io/nats.go" // <-- 导入 NATS 客户端

	// !!! 替换模块路径 !!!
	"go-xlive/cmd/user/server" // 导入 server 包
	"go-xlive/configs"

	// userv1 "go-xlive/gen/go/user/v1" // main 不直接用 - 已移除
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
		// 使用 zap logger 替换 log.Fatalf
		zap.L().Fatal("无法加载配置", zap.String("path", *configPath), zap.Error(err))
	}

	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("用户服务正在启动...", zap.String("service", serviceName))
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
	// PostgreSQL
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

	// NATS <-- 新增 NATS 连接
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.String("url", cfg.Nats.URL), zap.Error(err))
	}
	defer natsConn.Close() // 确保关闭连接
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 组装应用 ---
	userRepo := repository.NewRepository(dbpool)
	userSvc := userservice.NewUserService(logger, userRepo, natsConn)
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

	// 设置总的关闭超时
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownOverallCancel()

	// 按相反顺序关闭
	// 为 Metrics Server 设置单独的超时
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)

	// 为 gRPC Server 设置单独的超时
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 10*time.Second) // 调整超时
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)

	// 其他资源 (DB, Tracer, NATS) 已在 defer 中处理

	logger.Info("用户服务已关闭")
}

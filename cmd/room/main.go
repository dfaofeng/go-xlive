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
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	"go-xlive/cmd/room/client" // 导入 client 包
	"go-xlive/cmd/room/server" // 导入 server 包
	"go-xlive/configs"
	// roomv1 "go-xlive/gen/go/room/v1" // main 不直接用
	// userv1 "go-xlive/gen/go/user/v1" // 由 client 包处理
	"go-xlive/internal/room/repository"
	roomservice "go-xlive/internal/room/servie"
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
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	logger.Info("房间服务正在启动...", zap.String("service", serviceName))

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

	// --- 初始化 gRPC 客户端 (User Service) ---
	userClientWrapper, err := client.NewUserClient(cfg.Client) // 使用 client 包创建
	if err != nil {
		logger.Fatal("初始化用户服务客户端失败", zap.Error(err))
	}
	defer userClientWrapper.Close() // 确保关闭连接

	// --- 组装应用 ---
	roomRepo := repository.NewRepository(dbpool)
	roomSvc := roomservice.NewRoomService(logger, roomRepo, userClientWrapper.Client, natsConn) // 注入客户端
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	// 按相反顺序关闭：先关闭接收外部请求的服务器，再关闭内部资源（客户端连接等已 defer）
	metricsServer.Shutdown(shutdownCtx)
	grpcServer.Shutdown(shutdownCtx) // 内部包含 GracefulStop

	logger.Info("房间服务已关闭")
}

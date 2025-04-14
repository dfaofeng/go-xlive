package main

import (
	"context"
	"flag"
	"github.com/nats-io/nats.go"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	// !!! 替换模块路径 !!!
	"go-xlive/cmd/realtime/server"     // 导入 server 包
	"go-xlive/cmd/realtime/subscriber" // 导入 subscriber 包
	"go-xlive/configs"
	// realtimev1 "go-xlive/gen/go/realtime/v1" // main 不需要直接用 pb
	realtimeservice "go-xlive/internal/realtime/service"
	"go-xlive/pkg/observability"
	"go.uber.org/zap"
)

const (
	serviceName = "realtime-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	metricsPort := flag.Int("metrics_port", 9096, "Prometheus 指标暴露端口")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	logger.Info("实时服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	tpShutdown, err := observability.InitTracerProvider(serviceName)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	defer tpShutdown(context.Background())
	logger.Info("TracerProvider 初始化完成")

	// --- NATS 连接 ---
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 组装应用 ---
	realtimeSvc := realtimeservice.NewRealtimeService(logger, natsConn)
	natsSubscriber := subscriber.NewNatsSubscriber(logger, natsConn, realtimeSvc) // 创建订阅者实例, 传入 Hub
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Realtime.GrpcPort, realtimeSvc)
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 启动 NATS 订阅 ---
	natsCtx, natsCancel := context.WithCancel(context.Background())
	var natsWg sync.WaitGroup
	natsSubscriber.Start(natsCtx, &natsWg) // 使用订阅者实例启动
	logger.Info("RealtimeService NATS 订阅者已启动")

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

	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()

	// 1. 通知 NATS 订阅者退出
	logger.Info("正在通知 NATS 订阅者退出 (RealtimeService)...")
	natsCancel()
	natsSubscriber.StopNatsSubscription() // 调用显式停止

	// 2. 关闭 HTTP Metrics 服务器
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)

	// 3. 优雅关闭 gRPC 服务器
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 15*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)

	// 4. 等待 NATS 订阅者 goroutine 完成
	logger.Info("正在等待 NATS 订阅者完成 (RealtimeService)...")
	waitChan := make(chan struct{})
	go func() { natsWg.Wait(); close(waitChan) }()
	select {
	case <-waitChan:
		logger.Info("所有 NATS 订阅者已退出 (RealtimeService)")
	case <-shutdownOverallCtx.Done():
		logger.Warn("等待 NATS 订阅者退出超时 (RealtimeService)", zap.Error(shutdownOverallCtx.Err()))
	}

	logger.Info("实时服务已关闭")
}

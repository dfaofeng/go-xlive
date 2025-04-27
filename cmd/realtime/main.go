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

	"go-xlive/cmd/realtime/server"     // 导入 server 包
	"go-xlive/cmd/realtime/subscriber" // 导入 subscriber 包
	"go-xlive/configs"
	realtimeservice "go-xlive/internal/realtime/service" // 导入 service 包
	"go-xlive/pkg/observability"

	"github.com/nats-io/nats.go"

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
		// 使用 zap logger 替换 log.Fatalf
		zap.L().Fatal("无法加载配置", zap.String("path", *configPath), zap.Error(err))
	}

	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".") // 简化错误处理
	defer logger.Sync()
	logger.Info("实时服务正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	// <-- 修改: 传递 cfg.Otel
	tpShutdown, err := observability.InitTracerProvider(serviceName, cfg.Otel)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	defer func() {
		// 使用带有超时的后台 context 进行关闭
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tpShutdown(shutdownCtx)
	}()
	logger.Info("TracerProvider 初始化完成")

	// --- NATS 连接 ---
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.String("url", cfg.Nats.URL), zap.Error(err))
	}
	defer natsConn.Close() // 确保关闭 NATS 连接
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 组装应用 ---
	realtimeSvc := realtimeservice.NewRealtimeService(logger, natsConn)                        // 创建 Service 实例
	natsSubscriber := subscriber.NewNatsSubscriber(logger, natsConn, realtimeSvc.GetHub())     // 创建订阅者实例, 传入 Hub
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Realtime.GrpcPort, realtimeSvc) // 创建 gRPC Server
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort) // 创建 Metrics Server
	logger.Info("应用组件初始化完成")

	// --- 启动 NATS 订阅 ---
	natsCtx, natsCancel := context.WithCancel(context.Background()) // 创建用于 NATS 的 Context
	var natsWg sync.WaitGroup
	natsSubscriber.Start(natsCtx, &natsWg) // 使用订阅者实例启动
	logger.Info("RealtimeService NATS 订阅者已启动")

	// --- 启动服务器 ---
	grpcErrChan := grpcServer.Run()       // 启动 gRPC 服务器 Goroutine
	metricsErrChan := metricsServer.Run() // 启动 Metrics 服务器 Goroutine

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("收到关闭信号", zap.String("signal", sig.String()))
	case err := <-grpcErrChan: // 监听 gRPC 服务器错误
		if err != nil {
			logger.Error("gRPC 服务器异常退出", zap.Error(err))
		}
	case err := <-metricsErrChan: // 监听 Metrics 服务器错误
		if err != nil {
			logger.Error("Prometheus 指标服务器异常退出", zap.Error(err))
		}
	}

	// 设置总的关闭超时
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()

	// 1. 通知 NATS 订阅者退出
	logger.Info("正在通知 NATS 订阅者退出 (RealtimeService)...")
	natsCancel() // 取消 NATS 订阅者的 context
	// natsSubscriber.Shutdown() // 移除显式调用，依赖 context 和 defer

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
	go func() {
		natsWg.Wait()
		close(waitChan)
	}()
	select {
	case <-waitChan:
		logger.Info("所有 NATS 订阅者已退出 (RealtimeService)")
	case <-shutdownOverallCtx.Done(): // 使用总超时来等待 NATS 退出
		logger.Warn("等待 NATS 订阅者退出超时 (RealtimeService)", zap.Error(shutdownOverallCtx.Err()))
	}

	// 5. 关闭其他资源 (NATS Conn, Tracer 已在 defer 中处理)

	logger.Info("实时服务已关闭")
}

package main

import (
	"context"
	"flag"
	"log" // 移到这里，用于 logger 初始化前
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	// !!! 替换模块路径 !!!
	"go-xlive/configs"
	adapterservice "go-xlive/internal/adapter/service" // 导入 adapter service 包
	"go-xlive/pkg/observability"
)

const (
	serviceName = "adapter-service"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	httpPort := flag.Int("http_port", 8081, "Webhook HTTP 监听端口") // 为 Adapter Service 添加 HTTP 端口配置
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		// 在 logger 初始化前，先用标准库 log 致命退出
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, err := zap.NewProduction() // 使用 NewProduction 更安全
	if err != nil {
		log.Fatalf("无法初始化 zap logger: %v", err)
	}
	defer logger.Sync() // 程序退出前刷新 buffer
	logger.Info("平台适配器服务正在启动...", zap.String("service", serviceName))

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

	// --- 初始化基础设施连接 ---
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 组装应用 ---
	adapterSvc := adapterservice.NewAdapterService(logger, natsConn, *httpPort) // 创建 AdapterService 实例

	// --- 启动 HTTP Webhook 服务器 ---
	var wg sync.WaitGroup
	httpErrChan := adapterSvc.RunHTTPServer(&wg) // 启动 HTTP 服务器

	// --- 启动 Prometheus 指标服务器 (可选，但推荐) ---
	// metricsServer := server.NewMetricsServer(logger, *metricsPort) // 复用其他服务的 MetricsServer 或单独实现
	// metricsErrChan := metricsServer.Run()

	logger.Info("应用组件初始化完成")

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("收到关闭信号，开始优雅关闭...", zap.String("signal", sig.String()))
	case err := <-httpErrChan:
		if err != nil {
			logger.Error("Webhook HTTP 服务器异常退出", zap.Error(err))
		}
		// case err := <-metricsErrChan:
		// 	if err != nil {
		// 		logger.Error("Prometheus 指标服务器异常退出", zap.Error(err))
		// 	}
	}

	// --- 执行关闭 ---
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second) // 关闭超时
	defer shutdownCancel()

	// 关闭 HTTP 服务器
	adapterSvc.ShutdownHTTPServer(shutdownCtx)

	// 关闭 Metrics 服务器 (如果启动了)
	// metricsServer.Shutdown(shutdownCtx)

	// 等待所有后台 goroutine 完成 (如果 HTTP 服务器内部启动了 goroutine)
	// wg.Wait() // 确保 WaitGroup 被正确使用

	logger.Info("平台适配器服务已关闭")
}

// 移除文件末尾多余的 import

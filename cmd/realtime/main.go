package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	"go-xlive/cmd/realtime/server"
	"go-xlive/cmd/realtime/subscriber" // 导入 subscriber 包
	"go-xlive/configs"
	// realtimev1 "go-xlive/gen/go/realtime/v1" // main 不直接用
	realtimeservice "go-xlive/internal/realtime/service" // 导入 service 包
	"go-xlive/pkg/observability"
	"go.uber.org/zap"
)

const (
	serviceName = "realtime-service"
)

func main() {
	// --- 配置加载, 日志, Tracer, NATS 初始化 ---
	configPath := flag.String("config", "./configs", "...")
	mp := flag.Int("metrics_port", 9096, "...")
	flag.Parse()
	cfg, e := configs.LoadConfig(*cp)
	if e != nil {
		log.Fatalf("...: %v", e)
	}
	l, _ := zap.NewProduction()
	defer l.Sync()
	ts, e := observability.InitTracerProvider(serviceName)
	if e != nil {
		l.Fatal("...", zap.Error(e))
	}
	defer ts(context.Background())
	nc, e := nats.Connect(cfg.Nats.URL)
	if e != nil {
		l.Fatal("...", zap.Error(e))
	}
	defer nc.Close()
	l.Info("...", zap.String("service", serviceName))
	l.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 组装应用 ---
	realtimeSvc := realtimeservice.NewRealtimeService(logger, natsConn) // 创建 Service 实例
	// --- !!! 修改: 调用 GetHub() 获取 Hub 实例并传入 !!! ---
	natsSubscriber := subscriber.NewNatsSubscriber(logger, natsConn, realtimeSvc.GetHub()) // <--- 调用 GetHub()
	// --- 修改结束 ---
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Realtime.GrpcPort, realtimeSvc) // 创建 gRPC Server
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort) // 创建 Metrics Server
	logger.Info("应用组件初始化完成")

	// --- 启动 NATS 订阅 ---
	natsCtx, natsCancel := context.WithCancel(context.Background())
	var natsWg sync.WaitGroup
	natsSubscriber.Start(natsCtx, &natsWg) // 使用订阅者实例启动
	logger.Info("RealtimeService NATS 订阅者已启动")

	// --- 启动服务器 ---
	grpcErrChan := grpcServer.Run()
	metricsErrChan := metricsServer.Run()

	// --- 优雅关闭处理 (保持不变) ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		l.Info("...", zap.String("signal", sig.String()))
	case e := <-grpcErrChan:
		if e != nil {
			l.Error("...", zap.Error(e))
		}
	case e := <-metricsErrChan:
		if e != nil {
			l.Error("...", zap.Error(e))
		}
	}
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()
	l.Info("正在通知 NATS 订阅者退出 (RealtimeService)...")
	natsCancel()
	natsSubscriber.Shutdown() // 调用 Shutdown
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 15*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)
	l.Info("正在等待 NATS 订阅者完成 (RealtimeService)...")
	waitChan := make(chan struct{})
	go func() { natsWg.Wait(); close(waitChan) }()
	select {
	case <-waitChan:
		l.Info("...")
	case <-shutdownOverallCtx.Done():
		l.Warn("...", zap.Error(shutdownOverallCtx.Err()))
	}
	l.Info("实时服务已关闭")
}

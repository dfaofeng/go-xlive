package main

import (
	"context"
	"flag"
	"fmt" // 需要 fmt
	"log"
	"os"
	"os/signal"
	// "runtime" // 不再需要
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"go-xlive/cmd/aggregation/server"     // 导入 server 包
	"go-xlive/cmd/aggregation/subscriber" // 导入 subscriber 包
	"go-xlive/configs"
	eventv1 "go-xlive/gen/go/event/v1"
	sessionv1 "go-xlive/gen/go/session/v1"
	aggregationservice "go-xlive/internal/aggregation/service"
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials/insecure"
	// "google.golang.org/grpc/health" // server 包会处理
	// "google.golang.org/grpc/health/grpc_health_v1"
	// "google.golang.org/grpc/reflection" // server 包会处理
	// "google.golang.org/grpc/resolver"
	// "google.golang.org/grpc/stats"
)

const (
	serviceName = "aggregation-service"
)

func main() {
	// --- 配置加载, 日志, Tracer 初始化 ---
	configPath := flag.String("config", "./configs", "...")
	metricsPort := flag.Int("metrics_port", 9095, "...")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("...: %v", err)
	}
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	tpShutdown, err := observability.InitTracerProvider(serviceName)
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	defer tpShutdown(context.Background())
	logger.Info("聚合服务正在启动...", zap.String("service", serviceName))

	// --- NATS 连接 ---
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- 初始化 gRPC 客户端 ---
	otelClientHandler := otelgrpc.NewClientHandler( /* ... */ )
	loadBalancingConfig := fmt.Sprintf(`{"loadBalancingConfig": [{"%s":{}}]}`, roundrobin.Name)
	grpcOpts := []grpc.DialOption{ /* ... */ grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(otelClientHandler), grpc.WithChainUnaryInterceptor(observability.MetricsUnaryClientInterceptor()), grpc.WithDefaultServiceConfig(loadBalancingConfig)}
	// 连接 Event Service
	eventSvcAddr := cfg.Client.EventService.Addresses
	if len(eventSvcAddr) == 0 {
		eventSvcAddr = []string{"localhost:50054"}
	}
	eventTarget := "passthrough:///" + strings.Join(eventSvcAddr, ",")
	eventConn, err := grpc.Dial(eventTarget, grpcOpts...)
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	defer eventConn.Close()
	eventClient := eventv1.NewEventServiceClient(eventConn)
	logger.Info("已连接到 EventService")
	// 连接 Session Service
	sessionSvcAddr := cfg.Client.SessionService.Addresses
	if len(sessionSvcAddr) == 0 {
		sessionSvcAddr = []string{"localhost:50053"}
	}
	sessionTarget := "passthrough:///" + strings.Join(sessionSvcAddr, ",")
	sessionConn, err := grpc.Dial(sessionTarget, grpcOpts...)
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	defer sessionConn.Close()
	sessionClient := sessionv1.NewSessionServiceClient(sessionConn)
	logger.Info("已连接到 SessionService")

	// --- 组装应用 ---
	aggSvc := aggregationservice.NewAggregationService(logger, eventClient, sessionClient) // 移除 natsConn
	natsSubscriber := subscriber.NewNatsSubscriber(logger, natsConn, aggSvc)               // 创建订阅者实例, 传入 aggSvc
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Aggregation.GrpcPort, aggSvc)
	if err != nil {
		logger.Fatal("...", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 启动 NATS 订阅 ---
	natsCtx, natsCancel := context.WithCancel(context.Background())
	var natsWg sync.WaitGroup
	natsSubscriber.Start(natsCtx, &natsWg) // 使用订阅者实例启动
	logger.Info("AggregationService NATS 订阅者已启动")

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
			logger.Error("...", zap.Error(err))
		}
	case err := <-metricsErrChan:
		if err != nil {
			logger.Error("...", zap.Error(err))
		}
	}
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()
	logger.Info("正在通知 NATS 订阅者退出 (AggregationService)...")
	natsCancel()
	natsSubscriber.Shutdown() // 调用 subscriber 的 Shutdown
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 15*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)
	logger.Info("正在等待 NATS 订阅者完成 (AggregationService)...")
	waitChan := make(chan struct{})
	go func() { natsWg.Wait(); close(waitChan) }()
	select {
	case <-waitChan:
		logger.Info("...")
	case <-shutdownOverallCtx.Done():
		logger.Warn("...", zap.Error(shutdownOverallCtx.Err()))
	}
	logger.Info("聚合服务已关闭")
}

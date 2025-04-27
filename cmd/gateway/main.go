// cmd/gateway/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"go-xlive/pkg/log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	// !!! 替换模块路径 !!!
	"go-xlive/cmd/gateway/client"  // 导入 client 包
	"go-xlive/cmd/gateway/handler" // 导入 handler 包
	"go-xlive/cmd/gateway/router"  // 导入 router 包
	"go-xlive/cmd/gateway/server"  // 导入 server 包
	"go-xlive/configs"
	"go-xlive/pkg/observability"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
)

const (
	serviceName = "api-gateway"
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	flag.Parse()
	// 本地加载配置
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		// 使用 zap logger 替换 log.Fatalf
		zap.L().Fatal("无法加载配置", zap.String("path", *configPath), zap.Error(err))
	}
	// --- 日志初始化 ---
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("API 网关正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	// <-- 修改: 传递 cfg.Otel
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

	// --- 初始化 gRPC 客户端 ---
	grpcClients, err := client.NewClients(cfg.Client)
	if err != nil {
		logger.Fatal("初始化 gRPC 客户端失败", zap.Error(err))
	}
	defer grpcClients.Close() // 确保关闭所有客户端连接
	logger.Info("所有 gRPC 客户端已连接")

	// --- 初始化 WebSocket Hub ---
	wsHub := handler.NewHub(logger)
	go wsHub.Run() // 启动 Hub 的事件循环
	logger.Info("WebSocket Hub 已启动")

	// --- 创建 gRPC-Gateway Mux 并注册 Handlers ---
	restMux := runtime.NewServeMux()
	grpcOpts := grpcClients.GetDefaultGrpcClientOptions() // 假设 client.Clients 提供此方法
	err = handler.RegisterGrpcGatewayHandlers(
		context.Background(), // 使用后台 Context 注册
		restMux,
		grpcOpts,
		cfg.Client.UserService.Addresses[0],
		cfg.Client.RoomService.Addresses[0],
		cfg.Client.SessionService.Addresses[0],
		cfg.Client.EventService.Addresses[0],
		cfg.Client.AggregationService.Addresses[0],
		cfg.Client.RealtimeService.Addresses[0],
		logger,
	)
	if err != nil {
		logger.Fatal("注册 gRPC-Gateway Handlers 失败", zap.Error(err))
	}

	// --- 创建并配置主 HTTP 路由器 ---
	mainRouter := router.NewRouter(logger, grpcClients.Realtime, grpcClients.Session, grpcClients.Event, restMux, wsHub) // 传入依赖
	logger.Info("主 HTTP 路由器已配置")
	// *** HTTP 服务器插桩 ***
	// 使用 otelhttp 包装主路由器。 "gateway-http-server" 是这个 Span 的操作名。
	instrumentedRouter := otelhttp.NewHandler(mainRouter, "gateway-http-server",
		otelhttp.WithTracerProvider(otel.GetTracerProvider()), // 明确指定 Provider
		otelhttp.WithPropagators(otel.GetTextMapPropagator()), // 明确指定 Propagator
	)
	logger.Info("HTTP 路由器已使用 OpenTelemetry 进行插桩")
	// --- 启动 HTTP 服务器 ---
	httpPort := cfg.Server.Gateway.HttpPort
	if httpPort == 0 {
		httpPort = 8080
	}
	listenAddr := fmt.Sprintf(":%d", httpPort)
	httpServer, errChan := server.RunHTTPServer(context.Background(), logger, listenAddr, instrumentedRouter)
	logger.Info("HTTP 服务器已启动")

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("收到关闭信号", zap.String("signal", sig.String()))
	case err := <-errChan: // 监听服务器启动错误
		if err != nil {
			logger.Fatal("HTTP 网关启动失败", zap.Error(err)) // 启动失败是致命的
		}
	}

	// 执行关闭
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server.ShutdownHTTPServer(shutdownCtx, logger, httpServer, 10*time.Second) // 调用封装的关闭函数
	// 注意：wsHub 和 grpcClients 的关闭在 defer 中处理

	logger.Info("API 网关服务已关闭")
}

// cmd/gateway/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("无法加载配置: %v", err)
	}

	// --- 日志初始化 ---
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	logger.Info("API 网关正在启动...", zap.String("service", serviceName))

	// --- 初始化 TracerProvider ---
	tpShutdown, err := observability.InitTracerProvider(serviceName)
	if err != nil {
		logger.Fatal("初始化 TracerProvider 失败", zap.Error(err))
	}
	defer tpShutdown(context.Background())
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
	// 获取 gRPC Dial 选项 (含拦截器和 LB 配置) - 这部分可以移到 client 包或保持在此
	c, _ := client.NewClients(cfg.Client)
	grpcOpts := c.GetDefaultGrpcClientOptions() // 假设 client 包提供此函数
	err = handler.RegisterGrpcGatewayHandlers(
		context.Background(), // 使用后台 Context 注册
		restMux,
		grpcOpts,
		cfg.Client.UserService.Addresses[0], // 简化，仍然只取第一个地址
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
	mainRouter := router.NewRouter(logger, grpcClients.Realtime, restMux, wsHub) // 传入依赖
	logger.Info("主 HTTP 路由器已配置")

	// --- 启动 HTTP 服务器 ---
	httpPort := cfg.Server.Gateway.HttpPort
	if httpPort == 0 {
		httpPort = 8080
	}
	listenAddr := fmt.Sprintf(":%d", httpPort)
	httpServer, errChan := server.RunHTTPServer(context.Background(), logger, listenAddr, mainRouter)
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

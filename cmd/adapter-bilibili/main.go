package main

import (
	"context"
	"errors" // <-- 确保 errors 包已导入
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	// "time" // <-- 移除未使用的 time 包导入

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc" // <-- 新增: 导入 otelgrpc
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure" // For insecure connection

	"go-xlive/configs"
	p "go-xlive/pkg/log"

	// !!! 替换模块路径 !!!
	roomv1 "go-xlive/gen/go/room/v1"
	userv1 "go-xlive/gen/go/user/v1" // <-- 正确添加 User Service proto 导入
	adapterservice "go-xlive/internal/adapter-bilibili/service"
	"go-xlive/pkg/observability"
)

const (
	serviceName      = "adapter-bilibili-service"
	bilibiliPlatform = "bilibili" // 定义平台标识常量
)

func main() {
	// --- 配置加载 ---
	configPath := flag.String("config", "./configs", "配置文件路径")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("无法加载配置: %v", err)
	}

	//配置日志等级.时间.格式化
	logger := p.Init_Logger(serviceName, ".")
	defer logger.Sync()
	logger.Info("Bilibili 平台适配器服务正在启动...", zap.String("service", serviceName))

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

	// --- 初始化基础设施连接 ---
	// NATS
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("无法连接到 NATS", zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- gRPC 客户端连接 ---
	// *** 定义通用的 gRPC 客户端 Dial 选项，包含 OTel 拦截器 ***
	grpcClientOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 生产应替换为安全凭证
		grpc.WithChainUnaryInterceptor(
			otelgrpc.UnaryClientInterceptor(), // 添加 OTel 追踪拦截器
			// 如果有其他客户端拦截器（如 Metrics），也加在这里
			// observability.MetricsUnaryClientInterceptor(),
		),
		// 如果调用了流式 RPC，也需要添加流拦截器
		// grpc.WithChainStreamInterceptor(
		// 	otelgrpc.StreamClientInterceptor(),
		// ),
	}
	logger.Info("gRPC 客户端已配置 OTel 拦截器")

	// Session Service gRPC Client - REMOVED: No longer needed
	// if len(cfg.Client.SessionService.Addresses) == 0 {
	// 	logger.Fatal("配置中未找到 Session Service gRPC 地址 (client.session_service.addresses)")
	// }
	// sessionServiceAddr := cfg.Client.SessionService.Addresses[0]
	// sessionConn, err := grpc.Dial(sessionServiceAddr, grpcClientOpts...) // <-- 使用通用选项
	// if err != nil {
	// 	logger.Fatal("无法连接到 Session Service", zap.String("addr", sessionServiceAddr), zap.Error(err))
	// }
	// defer sessionConn.Close()
	// sessionClient := sessionv1.NewSessionServiceClient(sessionConn) // REMOVED declaration
	// logger.Info("成功连接到 Session Service", zap.String("addr", sessionServiceAddr))

	// Room Service gRPC Client
	if len(cfg.Client.RoomService.Addresses) == 0 {
		logger.Fatal("配置中未找到 Room Service gRPC 地址 (client.room_service.addresses)")
	}
	roomServiceAddr := cfg.Client.RoomService.Addresses[0]
	roomConn, err := grpc.Dial(roomServiceAddr, grpcClientOpts...) // <-- 使用通用选项
	if err != nil {
		logger.Fatal("无法连接到 Room Service", zap.String("addr", roomServiceAddr), zap.Error(err))
	}
	defer roomConn.Close()
	roomClient := roomv1.NewRoomServiceClient(roomConn) // <-- 创建 roomClient
	logger.Info("成功连接到 Room Service", zap.String("addr", roomServiceAddr))

	// User Service gRPC Client <-- 新增 User Service 客户端连接
	if len(cfg.Client.UserService.Addresses) == 0 {
		logger.Fatal("配置中未找到 User Service gRPC 地址 (client.user_service.addresses)")
	}
	userServiceAddr := cfg.Client.UserService.Addresses[0]
	userConn, err := grpc.Dial(userServiceAddr, grpcClientOpts...) // <-- 使用通用选项
	if err != nil {
		logger.Fatal("无法连接到 User Service", zap.String("addr", userServiceAddr), zap.Error(err))
	}
	defer userConn.Close()
	userClient := userv1.NewUserServiceClient(userConn) // <-- 创建 userClient
	logger.Info("成功连接到 User Service", zap.String("addr", userServiceAddr))

	// --- 组装应用 ---
	// REMOVED: sessionClient argument from NewBilibiliAdapterService call
	adapterSvc := adapterservice.NewBilibiliAdapterService(logger, natsConn, roomClient, userClient)

	// --- 启动监听器管理 ---
	ctx, cancel := context.WithCancel(context.Background()) // 创建主 context
	var wg sync.WaitGroup                                   // WaitGroup 用于等待管理 goroutine 退出

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("启动 Bilibili 监听器管理 goroutine...")
		// !!! 修正: 移除 roomClient 参数 !!!
		if err := adapterSvc.StartManagingListeners(ctx); err != nil {
			if err != context.Canceled && !errors.Is(err, context.Canceled) { // 检查 context.Canceled
				logger.Fatal("监听器管理 goroutine 异常退出", zap.Error(err))
			} else {
				logger.Info("监听器管理 goroutine 正常退出")
			}
		}
	}()

	// --- 优雅关闭处理 ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	<-quit // 等待关闭信号
	logger.Info("收到关闭信号，开始优雅关闭...")

	// --- 执行关闭 ---
	cancel() // 取消主 context，通知管理 goroutine 退出

	// 等待管理 goroutine 完成退出
	logger.Info("等待监听器管理 goroutine 关闭...")
	wg.Wait() // 等待 StartManagingListeners goroutine 结束

	logger.Info("Bilibili 平台适配器服务已关闭")
}

package main

import (
	"context"
	"flag"

	// "fmt" // <-- 移除未使用的导入
	"go-xlive/pkg/log"
	"os"
	"os/signal"
	"strings"

	// "sync" // <-- 移除未使用的导入
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"    // Import Redis client
	"github.com/jackc/pgx/v5/pgxpool" // Import pgxpool
	"github.com/nats-io/nats.go"

	"go-xlive/cmd/aggregation/server" // 导入 server 包
	// "go-xlive/cmd/aggregation/subscriber" // 导入 subscriber 包 - 移除，因为是 noop
	"go-xlive/configs"

	sessionv1 "go-xlive/gen/go/session/v1"
	aggRepository "go-xlive/internal/aggregation/repository" // <-- 修正: 正确导入 aggregation repository
	aggregationservice "go-xlive/internal/aggregation/service"
	"go-xlive/internal/event/repository" // Import event repository
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	// "google.golang.org/grpc/balancer/roundrobin" // <-- 移除未使用的导入
	"google.golang.org/grpc/credentials/insecure"
)

const (
	serviceName               = "aggregation-service"
	defaultSessionServiceAddr = "localhost:50053"
)

func main() {
	// --- 配置加载, 日志, Tracer 初始化 ---
	configPath := flag.String("config", "./configs", "配置文件目录路径")
	metricsPort := flag.Int("metrics_port", 9095, "...")
	flag.Parse()
	cfg, err := configs.LoadConfig(*configPath)
	if err != nil {
		zap.L().Fatal("加载配置失败", zap.String("path", *configPath), zap.Error(err))
	}
	logger := log.Init_Logger(serviceName, ".")
	defer logger.Sync()
	// <-- 修改: 传递 cfg.Otel
	tpShutdown, err := observability.InitTracerProvider(serviceName, cfg.Otel)
	if err != nil {
		logger.Fatal("初始化 Tracer Provider 失败", zap.Error(err))
	}
	defer func() {
		// 使用带有超时的后台 context 进行关闭
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tpShutdown(shutdownCtx)
	}()
	logger.Info("聚合服务正在启动...", zap.String("service", serviceName))

	// --- 数据库连接 ---
	dbPool, err := pgxpool.New(context.Background(), cfg.Database.PostgresDSN) // Use PostgresDSN
	if err != nil {
		logger.Fatal("连接数据库失败", zap.String("dsn", cfg.Database.PostgresDSN), zap.Error(err)) // Use PostgresDSN
	}
	defer dbPool.Close()
	if err := dbPool.Ping(context.Background()); err != nil {
		logger.Fatal("Ping 数据库失败", zap.Error(err))
	}
	logger.Info("成功连接到数据库")

	// --- NATS 连接 ---
	natsConn, err := nats.Connect(cfg.Nats.URL)
	if err != nil {
		logger.Fatal("连接 NATS 失败", zap.String("url", cfg.Nats.URL), zap.Error(err))
	}
	defer natsConn.Close()
	logger.Info("成功连接到 NATS", zap.String("url", cfg.Nats.URL))

	// --- Redis 连接 ---
	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer redisCancel()
	if _, err := redisClient.Ping(redisCtx).Result(); err != nil {
		logger.Fatal("连接 Redis 失败", zap.String("addr", cfg.Redis.Addr), zap.Error(err))
	}
	logger.Info("成功连接到 Redis")
	// defer redisClient.Close() // Consider closing if needed

	// --- 初始化 gRPC 客户端 ---
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

	// 连接 Session Service
	sessionSvcAddr := cfg.Client.SessionService.Addresses
	if len(sessionSvcAddr) == 0 {
		logger.Warn("未在配置中找到 SessionService 地址，使用默认值", zap.String("default", defaultSessionServiceAddr))
		sessionSvcAddr = []string{defaultSessionServiceAddr}
	}
	sessionTarget := "passthrough:///" + strings.Join(sessionSvcAddr, ",")
	sessionConn, err := grpc.Dial(sessionTarget, grpcClientOpts...) // <-- 使用通用选项
	if err != nil {
		logger.Fatal("连接 SessionService 失败", zap.String("target", sessionTarget), zap.Error(err))
	}
	defer sessionConn.Close()
	sessionClient := sessionv1.NewSessionServiceClient(sessionConn)
	logger.Info("已连接到 SessionService")

	// --- 创建事件仓库实例 ---
	eventRepo := repository.NewRepository(dbPool)
	// --- 新增: 创建聚合仓库实例 ---
	aggRepo := aggRepository.NewRepository(dbPool)

	// --- 组装应用 ---
	// --- 修正: 传入 redisClient 和 aggRepo ---
	aggSvc := aggregationservice.NewAggregationService(logger, eventRepo, aggRepo, sessionClient, redisClient)
	// <-- 移除未使用的 natsSubscriber 变量声明
	grpcServer, err := server.NewGrpcServer(logger, cfg.Server.Aggregation.GrpcPort, aggSvc)
	if err != nil {
		logger.Fatal("创建 gRPC 服务器失败", zap.Error(err))
	}
	metricsServer := server.NewMetricsServer(logger, *metricsPort)
	logger.Info("应用组件初始化完成")

	// --- 新增: 启动周期性指标快照保存 ---
	snapshotInterval := 3 * time.Second // 每3s保存一次快照 (可配置)
	snapshotCtx, snapshotCancel := context.WithCancel(context.Background())
	go func(ctx context.Context, interval time.Duration) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.Info("周期性指标快照保存 goroutine 已启动", zap.Duration("interval", interval))
		for {
			select {
			case <-ticker.C:
				logger.Debug("触发周期性指标快照保存")
				listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second) // 设置 RPC 调用超时
				resp, err := sessionClient.ListLiveSessions(listCtx, &sessionv1.ListLiveSessionsRequest{})
				listCancel() // 及时释放 listCtx
				if err != nil {
					logger.Error("周期性保存：获取直播中场次列表失败", zap.Error(err))
					continue // 跳过此次保存
				}
				liveSessionIDs := resp.GetSessionIds()
				if len(liveSessionIDs) == 0 {
					logger.Debug("周期性保存：当前无直播中场次")
					continue
				}
				logger.Info("周期性保存：获取到直播中场次", zap.Int("count", len(liveSessionIDs)), zap.Strings("ids", liveSessionIDs))
				for _, sessionID := range liveSessionIDs {
					saveCtx, saveCancel := context.WithTimeout(ctx, 15*time.Second)
					err := aggSvc.SaveMetricSnapshot(saveCtx, sessionID)
					saveCancel()
					if err != nil {
						logger.Error("周期性保存：保存指标快照失败", zap.String("session_id", sessionID), zap.Error(err))
					}
				}
			case <-ctx.Done():
				logger.Info("周期性指标快照保存 goroutine 收到退出信号，正在退出...")
				return
			}
		}
	}(snapshotCtx, snapshotInterval)

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
			logger.Error("gRPC 服务器运行时出错", zap.Error(err))
		}
	case err := <-metricsErrChan:
		if err != nil {
			logger.Error("Metrics 服务器运行时出错", zap.Error(err))
		}
	}
	shutdownOverallCtx, shutdownOverallCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownOverallCancel()

	// --- 新增: 取消周期性快照 goroutine ---
	logger.Info("正在通知周期性快照 goroutine 退出...")
	snapshotCancel()
	// --- 取消逻辑结束 ---
	metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(shutdownOverallCtx, 5*time.Second)
	defer metricsShutdownCancel()
	metricsServer.Shutdown(metricsShutdownCtx)
	grpcShutdownCtx, grpcShutdownCancel := context.WithTimeout(shutdownOverallCtx, 15*time.Second)
	defer grpcShutdownCancel()
	grpcServer.Shutdown(grpcShutdownCtx)
	logger.Info("聚合服务已关闭")
}

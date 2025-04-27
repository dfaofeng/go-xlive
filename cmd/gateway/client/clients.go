// cmd/gateway/client/clients.go
package client

import (
	"fmt"
	"go-xlive/configs"
	agggw "go-xlive/gen/go/aggregation/v1"
	eventv1 "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1"
	roomv1 "go-xlive/gen/go/room/v1"
	sessionv1 "go-xlive/gen/go/session/v1"
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/pkg/observability" // 需要 Metrics 拦截器
	"log"                        // 用于关键初始化错误
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials/insecure"
)

// Clients 包含所有后端服务的 gRPC 客户端连接和实例
type Clients struct {
	User        userv1.UserServiceClient
	Room        roomv1.RoomServiceClient
	Session     sessionv1.SessionServiceClient
	Event       eventv1.EventServiceClient
	Aggregation agggw.AggregationServiceClient
	Realtime    realtimev1.RealtimeServiceClient
	// 连接需要单独管理和关闭
	UserConn        *grpc.ClientConn
	RoomConn        *grpc.ClientConn
	SessionConn     *grpc.ClientConn
	EventConn       *grpc.ClientConn
	AggregationConn *grpc.ClientConn
	RealtimeConn    *grpc.ClientConn
}

// Close 关闭所有 gRPC 客户端连接
func (c *Clients) Close() {
	log.Println("正在关闭 gRPC 客户端连接...")
	if c.UserConn != nil {
		c.UserConn.Close()
	}
	if c.RoomConn != nil {
		c.RoomConn.Close()
	}
	if c.SessionConn != nil {
		c.SessionConn.Close()
	}
	if c.EventConn != nil {
		c.EventConn.Close()
	}
	if c.AggregationConn != nil {
		c.AggregationConn.Close()
	}
	if c.RealtimeConn != nil {
		c.RealtimeConn.Close()
	}
	log.Println("所有 gRPC 客户端连接已关闭")
}

// NewClients 创建并初始化所有 gRPC 客户端
func NewClients(cfg configs.ClientConfig) (*Clients, error) {
	// --- 创建 OTEL gRPC 客户端 Stats Handler ---
	//otelClientHandler := otelgrpc.NewClientHandler(
	//	otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
	//	otelgrpc.WithPropagators(otel.GetTextMapPropagator()),
	//)
	// --- gRPC 连接选项 ---
	loadBalancingConfig := fmt.Sprintf(`{"loadBalancingConfig": [{"%s":{}}]}`, roundrobin.Name)
	grpcOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 注意：生产环境需要安全凭证
		//grpc.WithStatsHandler(otelClientHandler), // StatsHandler 已被拦截器取代
		grpc.WithChainUnaryInterceptor( // 添加 Unary 拦截器链
			otelgrpc.UnaryClientInterceptor(),             // OTel 追踪拦截器
			observability.MetricsUnaryClientInterceptor(), // Metrics 拦截器
		),
		grpc.WithChainStreamInterceptor( // <-- 新增: 添加 Stream 拦截器链
			otelgrpc.StreamClientInterceptor(), // OTel 追踪拦截器
			// 如果需要，也可以添加 Stream 的 Metrics 拦截器
			// observability.MetricsStreamClientInterceptor(),
		),
		grpc.WithDefaultServiceConfig(loadBalancingConfig), // 启用轮询负载均衡
	}

	// --- 连接 User Service ---
	userSvcAddr := cfg.UserService.Addresses
	if len(userSvcAddr) == 0 {
		userSvcAddr = []string{"localhost:50051"}
	}
	userTarget := "passthrough:///" + strings.Join(userSvcAddr, ",")
	userConn, err := grpc.Dial(userTarget, grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("连接用户服务失败 (%s): %w", userTarget, err)
	}
	userClient := userv1.NewUserServiceClient(userConn)
	log.Printf("准备连接到用户服务: %v\n", userSvcAddr)

	// --- 连接 Room Service ---
	roomSvcAddr := cfg.RoomService.Addresses
	if len(roomSvcAddr) == 0 {
		roomSvcAddr = []string{"localhost:50052"}
	}
	roomTarget := "passthrough:///" + strings.Join(roomSvcAddr, ",")
	roomConn, err := grpc.Dial(roomTarget, grpcOpts...)
	if err != nil {
		userConn.Close()
		return nil, fmt.Errorf("连接房间服务失败 (%s): %w", roomTarget, err)
	}
	roomClient := roomv1.NewRoomServiceClient(roomConn)
	log.Printf("准备连接到房间服务: %v\n", roomSvcAddr)

	// --- 连接 Session Service ---
	sessionSvcAddr := cfg.SessionService.Addresses
	if len(sessionSvcAddr) == 0 {
		sessionSvcAddr = []string{"localhost:50053"}
	}
	sessionTarget := "passthrough:///" + strings.Join(sessionSvcAddr, ",")
	sessionConn, err := grpc.Dial(sessionTarget, grpcOpts...)
	if err != nil {
		userConn.Close()
		roomConn.Close()
		return nil, fmt.Errorf("连接场次服务失败 (%s): %w", sessionTarget, err)
	}
	sessionClient := sessionv1.NewSessionServiceClient(sessionConn)
	log.Printf("准备连接到场次服务: %v\n", sessionSvcAddr)

	// --- 连接 Event Service ---
	eventSvcAddr := cfg.EventService.Addresses
	if len(eventSvcAddr) == 0 {
		eventSvcAddr = []string{"localhost:50054"}
	}
	eventTarget := "passthrough:///" + strings.Join(eventSvcAddr, ",")
	eventConn, err := grpc.Dial(eventTarget, grpcOpts...)
	if err != nil {
		userConn.Close()
		roomConn.Close()
		sessionConn.Close()
		return nil, fmt.Errorf("连接事件服务失败 (%s): %w", eventTarget, err)
	}
	eventClient := eventv1.NewEventServiceClient(eventConn)
	log.Printf("准备连接到事件服务: %v\n", eventSvcAddr)

	// --- 连接 Aggregation Service ---
	aggregationSvcAddr := cfg.AggregationService.Addresses
	if len(aggregationSvcAddr) == 0 {
		aggregationSvcAddr = []string{"localhost:50055"}
	}
	aggregationTarget := "passthrough:///" + strings.Join(aggregationSvcAddr, ",")
	aggregationConn, err := grpc.Dial(aggregationTarget, grpcOpts...)
	if err != nil {
		userConn.Close()
		roomConn.Close()
		sessionConn.Close()
		eventConn.Close()
		return nil, fmt.Errorf("连接聚合服务失败 (%s): %w", aggregationTarget, err)
	}
	aggregationClient := agggw.NewAggregationServiceClient(aggregationConn)
	log.Printf("准备连接到聚合服务: %v\n", aggregationSvcAddr)

	// --- 连接 Realtime Service ---
	realtimeSvcAddr := cfg.RealtimeService.Addresses
	if len(realtimeSvcAddr) == 0 {
		realtimeSvcAddr = []string{"localhost:50056"}
	}
	realtimeTarget := "passthrough:///" + strings.Join(realtimeSvcAddr, ",")
	// 注意：连接 RealtimeService 时，可能需要不同的拦截器或选项（例如处理流）
	// 但这里为了简单，复用 grpcOpts
	realtimeConn, err := grpc.Dial(realtimeTarget, grpcOpts...)
	if err != nil {
		userConn.Close()
		roomConn.Close()
		sessionConn.Close()
		eventConn.Close()
		aggregationConn.Close()
		return nil, fmt.Errorf("连接实时服务失败 (%s): %w", realtimeTarget, err)
	}
	realtimeClient := realtimev1.NewRealtimeServiceClient(realtimeConn)
	log.Printf("准备连接到实时服务: %v\n", realtimeSvcAddr)

	return &Clients{
		User:            userClient,
		Room:            roomClient,
		Session:         sessionClient,
		Event:           eventClient,
		Aggregation:     aggregationClient,
		Realtime:        realtimeClient,
		UserConn:        userConn, // 保存连接用于关闭
		RoomConn:        roomConn,
		SessionConn:     sessionConn,
		EventConn:       eventConn,
		AggregationConn: aggregationConn,
		RealtimeConn:    realtimeConn,
	}, nil
}

// GetDefaultGrpcClientOptions 示例函数 (应放在 client 包中)
// 这里仅作演示，实际应放在 client/clients.go
func (c *Clients) GetDefaultGrpcClientOptions() []grpc.DialOption {
	//otelClientHandler := otelgrpc.NewClientHandler( /* ... */ )
	loadBalancingConfig := fmt.Sprintf(`{"loadBalancingConfig": [{"%s":{}}]}`, roundrobin.Name)
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		//grpc.WithStatsHandler(otelClientHandler),
		grpc.WithChainUnaryInterceptor(observability.MetricsUnaryClientInterceptor(), otelgrpc.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(otelgrpc.StreamClientInterceptor()), // <-- 确保这里也有 Stream 拦截器
		grpc.WithDefaultServiceConfig(loadBalancingConfig),
	}
}

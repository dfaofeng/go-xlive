// cmd/gateway/handler/grpc_gateway.go
package handler

import (
	"context"
	"fmt"
	// !!! 替换模块路径 !!!
	agggw "go-xlive/gen/go/aggregation/v1"
	eventgw "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1"
	roomgw "go-xlive/gen/go/room/v1"
	sessiongw "go-xlive/gen/go/session/v1"
	usergw "go-xlive/gen/go/user/v1"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap" // 引入 zap
	"google.golang.org/grpc"
)

// RegisterGrpcGatewayHandlers 注册所有需要通过 REST 访问的 gRPC 服务处理器
func RegisterGrpcGatewayHandlers(
	ctx context.Context,
	mux *runtime.ServeMux,
	grpcOpts []grpc.DialOption,
	userServiceTarget string, // 传入目标地址
	roomServiceTarget string,
	sessionServiceTarget string,
	eventServiceTarget string,
	aggregationServiceTarget string,
	realtimeServiceTarget string, // Realtime Service 地址
	logger *zap.Logger, // 传入 logger
) error {
	logger.Info("开始注册 gRPC-Gateway Handlers...")

	// User Service
	if err := usergw.RegisterUserServiceHandlerFromEndpoint(ctx, mux, userServiceTarget, grpcOpts); err != nil {
		return fmt.Errorf("注册用户网关失败: %w", err)
	}
	logger.Info("已注册 UserService Handlers", zap.String("proxy_to", userServiceTarget), zap.Strings("example_routes", []string{"POST /v1/users", "GET /v1/users/{user_id}", "GET /v1/users/healthz"}))

	// Room Service
	if err := roomgw.RegisterRoomServiceHandlerFromEndpoint(ctx, mux, roomServiceTarget, grpcOpts); err != nil {
		return fmt.Errorf("注册房间网关失败: %w", err)
	}
	logger.Info("已注册 RoomService Handlers", zap.String("proxy_to", roomServiceTarget), zap.Strings("example_routes", []string{"POST /v1/rooms", "GET /v1/rooms/{room_id}", "GET /v1/rooms/healthz"}))

	// Session Service
	if err := sessiongw.RegisterSessionServiceHandlerFromEndpoint(ctx, mux, sessionServiceTarget, grpcOpts); err != nil {
		return fmt.Errorf("注册场次网关失败: %w", err)
	}
	logger.Info("已注册 SessionService Handlers", zap.String("proxy_to", sessionServiceTarget), zap.Strings("example_routes", []string{"POST /v1/sessions", "PUT /v1/sessions/{session_id}/end", "GET /v1/sessions/{session_id}", "POST /v1/sessions/{session_id}/chat", "GET /v1/sessions/healthz"}))

	// Event Service
	if err := eventgw.RegisterEventServiceHandlerFromEndpoint(ctx, mux, eventServiceTarget, grpcOpts); err != nil {
		return fmt.Errorf("注册事件服务网关失败: %w", err)
	}
	logger.Info("已注册 EventService Handlers", zap.String("proxy_to", eventServiceTarget), zap.Strings("example_routes", []string{"POST /v1/events:query", "GET /v1/events/healthz"}))

	// Aggregation Service
	if err := agggw.RegisterAggregationServiceHandlerFromEndpoint(ctx, mux, aggregationServiceTarget, grpcOpts); err != nil {
		return fmt.Errorf("注册聚合服务网关失败: %w", err)
	}
	logger.Info("已注册 AggregationService Handlers", zap.String("proxy_to", aggregationServiceTarget), zap.Strings("example_routes", []string{"POST /v1/aggregation/trigger", "GET /v1/aggregation/healthz"}))

	// Realtime Service (仅 HealthCheck)
	if err := realtimev1.RegisterRealtimeServiceHandlerFromEndpoint(ctx, mux, realtimeServiceTarget, grpcOpts); err != nil { // 使用 realtimeServiceAddr
		return fmt.Errorf("注册实时服务网关失败: %w", err)
	}
	logger.Info("已注册 RealtimeService Handlers (仅 HealthCheck)", zap.String("proxy_to", realtimeServiceTarget), zap.Strings("example_routes", []string{"GET /v1/realtime/healthz"}))

	logger.Info("所有 gRPC-Gateway REST Handlers 已注册完成")
	return nil
}

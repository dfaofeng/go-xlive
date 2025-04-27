package grpc

import (
	"context"
	"errors"

	// !!! 确保这里的路径是正确的业务逻辑 service 包路径 !!!
	sessionv1 "go-xlive/gen/go/session/v1"
	"go-xlive/internal/session/service"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BusinessService 定义了 gRPC Handler 依赖的业务逻辑层接口。
// 这使得 Handler 与具体的 Service 实现解耦。
type BusinessService interface {
	DeleteSession(ctx context.Context, sessionID string) error
	HealthCheck(ctx context.Context) (string, error)
	GetSession(ctx context.Context, sessionID string) (*sessionv1.Session, error)
	GetLiveSessionByRoomID(ctx context.Context, roomID string) (*sessionv1.Session, error)
	SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (string, error)
	CheckSessionActive(ctx context.Context, roomID string) (bool, string, error)
	ListSessions(ctx context.Context, req *sessionv1.ListSessionsRequest) ([]*sessionv1.Session, int32, error)
	ListLiveSessions(ctx context.Context, req *sessionv1.ListLiveSessionsRequest) (*sessionv1.ListLiveSessionsResponse, error)
	UpdateSessionAggregates(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.Session, error)
	// GetSessionDetails is intentionally omitted if it just calls GetSession
	// Add other methods like CreateSession, EndSession if the handler needs to call them directly.
}

// GrpcHandler 实现 sessionv1.SessionServiceServer 接口。
// 它作为传输层，处理 gRPC 请求并调用业务逻辑服务。
type GrpcHandler struct {
	sessionv1.UnimplementedSessionServiceServer                 // 嵌入以便向前兼容
	svc                                         BusinessService // 依赖业务逻辑接口
	logger                                      *zap.Logger
}

// NewGrpcHandler 创建一个新的 gRPC handler 实例。
func NewGrpcHandler(svc BusinessService, logger *zap.Logger) *GrpcHandler { // <-- 接受接口类型
	if svc == nil {
		panic("GrpcHandler requires a non-nil BusinessService") // 更新错误消息
	}
	if logger == nil {
		panic("GrpcHandler requires a non-nil logger") // 添加 nil 检查
	}
	return &GrpcHandler{
		svc:    svc,
		logger: logger.Named("grpc_handler"), // 为 handler 日志添加特定名称
	}
}

// DeleteSession 处理删除场次的 gRPC 请求。
func (h *GrpcHandler) DeleteSession(ctx context.Context, req *sessionv1.DeleteSessionRequest) (*emptypb.Empty, error) {
	tracer := otel.Tracer("session-handler-grpc") // 为 handler 使用特定的 tracer 名称
	ctx, span := tracer.Start(ctx, "DeleteSession")
	defer span.End()
	span.SetAttributes(attribute.String("grpc.request.session_id", req.GetSessionId()))

	h.logger.Info("gRPC: 收到 DeleteSession 请求", zap.String("session_id", req.GetSessionId()))

	if req.GetSessionId() == "" {
		err := status.Error(codes.InvalidArgument, "session_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: DeleteSession 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑服务的 DeleteSession 方法 (假设它已从 DeleteSessionBiz 重命名)
	// 我们稍后将在 service 中重命名 DeleteSessionBiz 为 DeleteSession。
	err := h.svc.DeleteSession(ctx, req.GetSessionId()) // 调用 *重命名后* 的 service 方法
	if err != nil {
		// 将业务错误映射到 gRPC 状态码
		if errors.Is(err, service.ErrSessionNotFound) { // 使用从 service 包导出的错误
			// 在删除期间将 "未找到" 视为成功 (幂等性)
			h.logger.Info("gRPC: 要删除的场次未找到，视为成功", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Ok, "场次已被删除或从未存在")
			return &emptypb.Empty{}, nil
		}
		// 对于其他错误，返回内部错误
		h.logger.Error("gRPC: 调用服务时 DeleteSession 失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.RecordError(err) // 记录原始错误
		span.SetStatus(otelCodes.Error, "内部服务错误")
		// 向客户端返回通用的内部错误
		return nil, status.Errorf(codes.Internal, "删除场次时发生内部错误")
	}

	h.logger.Info("gRPC: DeleteSession 成功", zap.String("session_id", req.GetSessionId()))
	span.SetStatus(otelCodes.Ok, "场次已删除")
	return &emptypb.Empty{}, nil
}

// --- 其他 gRPC 方法将移动到这里 ---
// GetSession(...)
// ListSessions(...)
// CheckSessionActive(...)
// GetLiveSessionByRoomID(...)
// SendChatMessage(...)
// HealthCheck(...)
// ListLiveSessions(...)
// GetSessionDetails(...)

// 注意: CreateSession 和 EndSession 在当前设计中是由内部事件触发，
// 所以它们可能不需要在 gRPC Handler 中实现，除非有直接的 API 调用需求。
// 如果需要，也需要将它们的 gRPC 实现（如果之前有）移到这里。

// HealthCheck 实现健康检查端点
func (h *GrpcHandler) HealthCheck(ctx context.Context, req *emptypb.Empty) (*sessionv1.HealthCheckResponse, error) {
	// 健康检查通常直接调用业务逻辑层的健康检查
	// 因为它需要检查业务逻辑层依赖的组件（DB, Redis 等）
	statusStr, err := h.svc.HealthCheck(ctx) // 调用 *重命名后* 的业务逻辑层检查
	if err != nil {
		h.logger.Error("gRPC HealthCheck: 服务不健康", zap.Error(err))
		// 即使不健康，通常也返回 SERVING 状态，但附带错误信息，
		// 或者根据具体策略返回 NOT_SERVING。这里我们返回实际状态。
		return &sessionv1.HealthCheckResponse{Status: statusStr}, nil // 返回从业务层获取的状态
	}
	return &sessionv1.HealthCheckResponse{Status: statusStr}, nil
}

// GetSession 处理获取场次信息的 gRPC 请求
func (h *GrpcHandler) GetSession(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "GetSession")
	defer span.End()
	span.SetAttributes(attribute.String("grpc.request.session_id", req.GetSessionId()))

	h.logger.Info("gRPC: 收到 GetSession 请求", zap.String("session_id", req.GetSessionId()))

	if req.GetSessionId() == "" {
		err := status.Error(codes.InvalidArgument, "session_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: GetSession 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑
	session, err := h.svc.GetSession(ctx, req.GetSessionId()) // 假设 Service 中有 GetSession 方法
	if err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			h.logger.Info("gRPC: GetSession 未找到场次", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Ok, "Session not found") // Not an error state for gRPC, return NotFound status
			return nil, status.Errorf(codes.NotFound, "场次 %s 未找到", req.GetSessionId())
		}
		h.logger.Error("gRPC: 调用服务时 GetSession 失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "获取场次时发生内部错误")
	}

	h.logger.Info("gRPC: GetSession 成功", zap.String("session_id", req.GetSessionId()))
	span.SetStatus(otelCodes.Ok, "场次已获取")
	return &sessionv1.GetSessionResponse{Session: session}, nil
}

// GetLiveSessionByRoomID 处理根据房间 ID 获取直播场次的 gRPC 请求
func (h *GrpcHandler) GetLiveSessionByRoomID(ctx context.Context, req *sessionv1.GetLiveSessionByRoomIDRequest) (*sessionv1.GetLiveSessionByRoomIDResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "GetLiveSessionByRoomID")
	defer span.End()
	span.SetAttributes(attribute.String("grpc.request.room_id", req.GetRoomId()))

	h.logger.Info("gRPC: 收到 GetLiveSessionByRoomID 请求", zap.String("room_id", req.GetRoomId()))

	if req.GetRoomId() == "" {
		err := status.Error(codes.InvalidArgument, "room_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: GetLiveSessionByRoomID 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑
	session, err := h.svc.GetLiveSessionByRoomID(ctx, req.GetRoomId()) // 假设 Service 中有 GetLiveSessionByRoomID 方法
	if err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			h.logger.Info("gRPC: GetLiveSessionByRoomID 未找到活跃场次", zap.String("room_id", req.GetRoomId()))
			span.SetStatus(otelCodes.Ok, "Live session not found")
			// 对于 GetLiveSession，找不到通常不返回 NotFound 错误，而是返回空响应
			return &sessionv1.GetLiveSessionByRoomIDResponse{Session: nil}, nil
		}
		h.logger.Error("gRPC: 调用服务时 GetLiveSessionByRoomID 失败", zap.Error(err), zap.String("room_id", req.GetRoomId()))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "获取直播场次时发生内部错误")
	}

	h.logger.Info("gRPC: GetLiveSessionByRoomID 成功", zap.String("room_id", req.GetRoomId()), zap.String("session_id", session.GetSessionId()))
	span.SetStatus(otelCodes.Ok, "直播场次已获取")
	return &sessionv1.GetLiveSessionByRoomIDResponse{Session: session}, nil
}

// SendChatMessage 处理发送聊天消息的 gRPC 请求
func (h *GrpcHandler) SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (*sessionv1.SendChatMessageResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "SendChatMessage")
	defer span.End()
	span.SetAttributes(
		attribute.String("grpc.request.session_id", req.GetSessionId()),
		attribute.String("grpc.request.user_id", req.GetUserId()),
	)

	h.logger.Info("gRPC: 收到 SendChatMessage 请求", zap.String("session_id", req.GetSessionId()), zap.String("user_id", req.GetUserId()))

	if req.GetSessionId() == "" || req.GetUserId() == "" || req.GetContent() == "" {
		err := status.Error(codes.InvalidArgument, "session_id, user_id 和 content 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: SendChatMessage 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑
	messageID, err := h.svc.SendChatMessage(ctx, req) // 假设 Service 中有 SendChatMessage 方法
	if err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			h.logger.Warn("gRPC: SendChatMessage 的场次未找到", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Error, "Session not found")
			return nil, status.Errorf(codes.NotFound, "场次 %s 未找到", req.GetSessionId())
		}
		if errors.Is(err, service.ErrSessionNotLive) {
			h.logger.Warn("gRPC: SendChatMessage 的场次非直播状态", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Error, "Session not live")
			return nil, status.Errorf(codes.FailedPrecondition, "场次 %s 非直播状态", req.GetSessionId())
		}
		h.logger.Error("gRPC: 调用服务时 SendChatMessage 失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "发送聊天消息时发生内部错误")
	}

	h.logger.Info("gRPC: SendChatMessage 成功", zap.String("session_id", req.GetSessionId()), zap.String("message_id", messageID))
	span.SetStatus(otelCodes.Ok, "聊天消息已发送")
	return &sessionv1.SendChatMessageResponse{MessageId: messageID, Success: true}, nil
}

// CheckSessionActive 处理检查会话是否活跃的 gRPC 请求 (通常内部调用)
func (h *GrpcHandler) CheckSessionActive(ctx context.Context, req *sessionv1.CheckSessionActiveRequest) (*sessionv1.CheckSessionActiveResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "CheckSessionActive")
	defer span.End()
	span.SetAttributes(attribute.String("grpc.request.room_id", req.GetRoomId()))

	h.logger.Debug("gRPC: 收到 CheckSessionActive 请求", zap.String("room_id", req.GetRoomId())) // 使用 Debug 级别，因为可能是高频内部调用

	if req.GetRoomId() == "" {
		err := status.Error(codes.InvalidArgument, "room_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: CheckSessionActive 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑
	isActive, sessionID, err := h.svc.CheckSessionActive(ctx, req.GetRoomId()) // 假设 Service 中有 CheckSessionActive 方法
	if err != nil {
		h.logger.Error("gRPC: 调用服务时 CheckSessionActive 失败", zap.Error(err), zap.String("room_id", req.GetRoomId()))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "检查会话活跃状态时发生内部错误")
	}

	h.logger.Debug("gRPC: CheckSessionActive 成功", zap.String("room_id", req.GetRoomId()), zap.Bool("is_active", isActive), zap.String("session_id", sessionID))
	span.SetStatus(otelCodes.Ok, "会话活跃状态已检查")
	return &sessionv1.CheckSessionActiveResponse{IsActive: isActive, SessionId: sessionID}, nil
}

// ListSessions 处理列出场次的 gRPC 请求
func (h *GrpcHandler) ListSessions(ctx context.Context, req *sessionv1.ListSessionsRequest) (*sessionv1.ListSessionsResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "ListSessions")
	defer span.End()
	span.SetAttributes(
		attribute.Int("grpc.request.page", int(req.GetPage())),
		attribute.Int("grpc.request.page_size", int(req.GetPageSize())),
		attribute.String("grpc.request.filter.status", req.GetStatus()),
		attribute.String("grpc.request.filter.room_id", req.GetRoomId()),
		attribute.String("grpc.request.filter.owner_user_id", req.GetOwnerUserId()),
	)

	h.logger.Info("gRPC: 收到 ListSessions 请求",
		zap.Int32("page", req.GetPage()),
		zap.Int32("page_size", req.GetPageSize()),
		zap.String("status", req.GetStatus()),
		zap.String("room_id", req.GetRoomId()),
		zap.String("owner_user_id", req.GetOwnerUserId()))

	// 基本验证
	if req.GetPage() <= 0 {
		req.Page = 1 // 默认到第一页
	}
	if req.GetPageSize() <= 0 {
		req.PageSize = 10 // 默认页面大小
	}
	// 可以添加更多验证，例如 page_size 上限

	// 调用业务逻辑
	sessions, total, err := h.svc.ListSessions(ctx, req) // 假设 Service 中有 ListSessions 方法
	if err != nil {
		h.logger.Error("gRPC: 调用服务时 ListSessions 失败", zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "列出场次时发生内部错误")
	}

	h.logger.Info("gRPC: ListSessions 成功", zap.Int("count", len(sessions)), zap.Int32("total", total))
	span.SetStatus(otelCodes.Ok, "场次列表已获取")
	return &sessionv1.ListSessionsResponse{
		Sessions: sessions,
		Total:    total,
		Page:     req.GetPage(),
		PageSize: req.GetPageSize(),
	}, nil
}

// GetSessionDetails 处理获取场次详情的 gRPC 请求 (复用 GetSession)
func (h *GrpcHandler) GetSessionDetails(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	// 直接调用 GetSession 的逻辑
	return h.GetSession(ctx, req)
}

// ListLiveSessions 处理列出所有直播中场次 ID 的 gRPC 请求 (通常内部调用)
func (h *GrpcHandler) ListLiveSessions(ctx context.Context, req *sessionv1.ListLiveSessionsRequest) (*sessionv1.ListLiveSessionsResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "ListLiveSessions")
	defer span.End()

	h.logger.Debug("gRPC: 收到 ListLiveSessions 请求") // Debug 级别

	// 调用业务逻辑
	sessionIDs, err := h.svc.ListLiveSessions(ctx, req) // 假设 Service 中有 ListLiveSessions 方法
	if err != nil {
		h.logger.Error("gRPC: 调用服务时 ListLiveSessions 失败", zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "列出直播场次 ID 时发生内部错误")
	}

	h.logger.Debug("gRPC: ListLiveSessions 成功", zap.Int("count", len(sessionIDs.GetSessionIds())))
	span.SetStatus(otelCodes.Ok, "直播场次 ID 列表已获取")
	return sessionIDs, nil
}

// UpdateSessionAggregates 处理更新场次聚合数据的 gRPC 请求 (通常内部调用)
func (h *GrpcHandler) UpdateSessionAggregates(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.UpdateSessionAggregatesResponse, error) {
	tracer := otel.Tracer("session-handler-grpc")
	ctx, span := tracer.Start(ctx, "UpdateSessionAggregates")
	defer span.End()
	span.SetAttributes(attribute.String("grpc.request.session_id", req.GetSessionId()))

	h.logger.Debug("gRPC: 收到 UpdateSessionAggregates 请求", zap.String("session_id", req.GetSessionId())) // Debug 级别

	if req.GetSessionId() == "" {
		err := status.Error(codes.InvalidArgument, "session_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		h.logger.Warn("gRPC: UpdateSessionAggregates 的无效参数", zap.Error(err))
		return nil, err
	}

	// 调用业务逻辑
	updatedSession, err := h.svc.UpdateSessionAggregates(ctx, req) // 假设 Service 中有 UpdateSessionAggregates 方法
	if err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			h.logger.Warn("gRPC: UpdateSessionAggregates 的场次未找到", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Error, "Session not found")
			return nil, status.Errorf(codes.NotFound, "场次 %s 未找到", req.GetSessionId())
		}
		h.logger.Error("gRPC: 调用服务时 UpdateSessionAggregates 失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "内部服务错误")
		return nil, status.Errorf(codes.Internal, "更新场次聚合数据时发生内部错误")
	}

	h.logger.Debug("gRPC: UpdateSessionAggregates 成功", zap.String("session_id", req.GetSessionId()))
	span.SetStatus(otelCodes.Ok, "场次聚合数据已更新")
	return &sessionv1.UpdateSessionAggregatesResponse{UpdatedSession: updatedSession}, nil
}

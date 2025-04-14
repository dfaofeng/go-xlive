package service

import (
	"context"
	"fmt"
	"log" // 保留 log 用于 New 函数检查
	// "runtime" // 不再需要
	// "strings" // 不再需要
	// "sync"    // 不再需要
	"time"

	// "github.com/nats-io/nats.go" // Service 层不再直接依赖 NATS Conn
	"google.golang.org/protobuf/proto"
	// !!! 替换模块路径 !!!
	aggregationv1 "go-xlive/gen/go/aggregation/v1"
	eventv1 "go-xlive/gen/go/event/v1"
	sessionv1 "go-xlive/gen/go/session/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace" // 导入 trace 用于明确创建 Span
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AggregationService 结构体 (移除 NATS 相关字段)
type AggregationService struct {
	aggregationv1.UnimplementedAggregationServiceServer
	logger        *zap.Logger
	eventClient   eventv1.EventServiceClient     // 事件服务客户端
	sessionClient sessionv1.SessionServiceClient // 场次服务客户端
	tracer        trace.Tracer                   // 添加 Tracer
}

// NewAggregationService 创建实例 (移除 NATS Conn 参数)
func NewAggregationService(logger *zap.Logger, eventClient eventv1.EventServiceClient, sessionClient sessionv1.SessionServiceClient) *AggregationService {
	if logger == nil {
		log.Fatal("AggregationService 需要 Logger")
	}
	if eventClient == nil {
		log.Fatal("AggregationService 需要 EventServiceClient")
	}
	if sessionClient == nil {
		log.Fatal("AggregationService 需要 SessionServiceClient")
	}
	return &AggregationService{
		logger:        logger.Named("agg_service"),
		eventClient:   eventClient,
		sessionClient: sessionClient,
		tracer:        otel.Tracer("aggregation-service"), // 初始化 Tracer
	}
}

// ProcessAggregation 是核心聚合处理逻辑
// ctx: 传入的上下文，可能来自 NATS handler 或 gRPC 请求
// sessionID: 需要聚合的场次 ID
// 返回: 更新后的 Session 信息 和 错误
func (s *AggregationService) ProcessAggregation(ctx context.Context, sessionID string) (*sessionv1.Session, error) {
	// 1. 创建一个新的子 Span，继承传入的 Context
	procCtx, span := s.tracer.Start(ctx, "ProcessAggregation") // 方法名改为 ProcessAggregation
	defer span.End()
	span.SetAttributes(attribute.String("session_id", sessionID))
	s.logger.Info("开始处理场次聚合", zap.String("session_id", sessionID))

	// 2. 调用 EventService 查询该场次的所有事件
	s.logger.Info("正在查询事件...", zap.String("session_id", sessionID))
	queryCtx, queryCancel := context.WithTimeout(procCtx, 30*time.Second) // 使用 procCtx
	defer queryCancel()
	// 假设查询接口支持不指定 EventType 来获取所有事件
	eventResp, err := s.eventClient.QueryEvents(queryCtx, &eventv1.QueryEventsRequest{SessionId: sessionID})
	if err != nil {
		s.logger.Error("查询事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to query events")
		st, _ := status.FromError(err)
		if st.Code() == codes.DeadlineExceeded {
			return nil, fmt.Errorf("查询事件超时: %w", err)
		}
		return nil, fmt.Errorf("查询事件失败: %w", err)
	}
	eventCount := len(eventResp.GetEvents())
	s.logger.Info("查询到事件数量", zap.String("session_id", sessionID), zap.Int("count", eventCount))
	span.SetAttributes(attribute.Int("events.queried_count", eventCount))

	// 3. 执行聚合计算
	totalEvents := int64(eventCount)
	var totalDanmaku int64 = 0
	var totalGiftsValue int64 = 0
	for _, ev := range eventResp.GetEvents() {
		switch ev.EventType {
		case eventv1.EventType_EVENT_TYPE_CHAT_MESSAGE:
			totalDanmaku++
		case eventv1.EventType_EVENT_TYPE_GIFT:
			var giftEvent eventv1.GiftSent
			if err := proto.Unmarshal(ev.Data, &giftEvent); err == nil {
				totalGiftsValue += giftEvent.GetCost() * int64(giftEvent.GetQuantity())
			} else {
				s.logger.Warn("无法解析 GiftSent 事件数据进行聚合", zap.String("event_id", ev.GetEventId()), zap.Error(err))
				span.AddEvent("Unmarshal GiftSent Error", trace.WithAttributes(attribute.String("event.id", ev.GetEventId())))
			}
		}
	}
	s.logger.Info("聚合计算完成",
		zap.String("session_id", sessionID),
		zap.Int64("total_events", totalEvents),
		zap.Int64("total_danmaku", totalDanmaku),
		zap.Int64("total_gifts_value", totalGiftsValue))
	span.SetAttributes(
		attribute.Int64("aggregation.total_events", totalEvents),
		attribute.Int64("aggregation.total_danmaku", totalDanmaku),
		attribute.Int64("aggregation.total_gifts_value", totalGiftsValue),
	)

	// 4. 调用 SessionService 更新聚合结果
	s.logger.Info("正在更新场次聚合结果...", zap.String("session_id", sessionID))
	updateCtx, updateCancel := context.WithTimeout(procCtx, 5*time.Second) // 使用 procCtx
	defer updateCancel()
	updateReq := &sessionv1.UpdateSessionAggregatesRequest{
		SessionId:       sessionID,
		TotalEvents:     totalEvents,
		TotalDanmaku:    totalDanmaku,
		TotalGiftsValue: totalGiftsValue,
	}
	updateResp, err := s.sessionClient.UpdateSessionAggregates(updateCtx, updateReq)
	if err != nil {
		s.logger.Error("更新场次聚合结果失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to update session aggregates")
		st, _ := status.FromError(err)
		// 检查是否因为 Session 不存在而更新失败 (SessionService 返回 NotFound)
		if st.Code() == codes.NotFound {
			return nil, fmt.Errorf("更新聚合结果失败：未找到场次 %s: %w", sessionID, err)
		} else if st.Code() == codes.DeadlineExceeded {
			return nil, fmt.Errorf("更新场次聚合结果超时: %w", err)
		}
		return nil, fmt.Errorf("更新场次聚合结果失败: %w", err)
	}
	s.logger.Info("场次聚合结果更新成功", zap.String("session_id", sessionID))

	span.SetStatus(otelCodes.Ok, "Aggregation processed successfully")
	return updateResp.GetUpdatedSession(), nil // 返回更新后的 Session
}

// TriggerAggregation 手动触发聚合 (可选的 gRPC 接口)
func (s *AggregationService) TriggerAggregation(ctx context.Context, req *aggregationv1.TriggerAggregationRequest) (*aggregationv1.TriggerAggregationResponse, error) {
	procCtx, span := s.tracer.Start(ctx, "TriggerAggregationRPC") // 使用传入的 gRPC ctx
	defer span.End()
	span.SetAttributes(attribute.String("session_id", req.GetSessionId()))

	s.logger.Info("收到手动触发聚合请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		span.SetStatus(otelCodes.Error, "Invalid argument: session_id required")
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id")
	}

	// 调用核心处理逻辑，传递带有新 Span 的 Context
	updatedSession, err := s.ProcessAggregation(procCtx, req.GetSessionId()) // 传递 procCtx
	if err != nil {
		s.logger.Error("手动触发聚合处理失败", zap.String("session_id", req.GetSessionId()), zap.Error(err))
		// Span 状态已在 ProcessAggregation 中设置
		return nil, status.Errorf(codes.Internal, "处理聚合失败: %v", err)
	}

	s.logger.Info("手动触发聚合处理成功", zap.String("session_id", req.GetSessionId()))
	return &aggregationv1.TriggerAggregationResponse{UpdatedSession: updatedSession}, nil
}

// HealthCheck 实现
func (s *AggregationService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*aggregationv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (AggregationService)")
	healthStatus := "SERVING"
	var firstError error

	// 检查 EventService
	checkCtx1, cancel1 := context.WithTimeout(ctx, 1*time.Second)
	defer cancel1()
	_, err := s.eventClient.HealthCheck(checkCtx1, &emptypb.Empty{})
	if err != nil {
		s.logger.Error("依赖健康检查失败: EventService", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// 检查 SessionService
	checkCtx2, cancel2 := context.WithTimeout(ctx, 1*time.Second)
	defer cancel2()
	_, err = s.sessionClient.HealthCheck(checkCtx2, &emptypb.Empty{})
	if err != nil {
		s.logger.Error("依赖健康检查失败: SessionService", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// Aggregation Service 不直接依赖 NATS 或 DB 进行核心逻辑

	if firstError != nil {
		return &aggregationv1.HealthCheckResponse{Status: healthStatus}, nil
	} // 不暴露内部错误
	return &aggregationv1.HealthCheckResponse{Status: healthStatus}, nil
}

// --- 移除 StartNatsSubscription 和 StopNatsSubscription ---

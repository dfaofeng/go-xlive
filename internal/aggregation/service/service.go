package service

import (
	"context"
	"fmt"
	"log" // 保留 log 用于 New 函数检查

	// "math" // No longer needed for MaxInt32
	"strconv" // Import strconv for Redis value parsing
	"time"

	"github.com/go-redis/redis/v8" // Import Redis client
	"github.com/jackc/pgx/v5/pgtype"

	// "github.com/jackc/pgx/v5/pgtype" // No longer needed directly here
	// "google.golang.org/protobuf/proto" // No longer needed here

	// !!! 替换模块路径 !!!
	aggregationv1 "go-xlive/gen/go/aggregation/v1"
	// eventv1 "go-xlive/gen/go/event/v1" // No longer needed here
	sessionv1 "go-xlive/gen/go/session/v1"
	"go-xlive/internal/event/repository" // Import event repository

	// db "go-xlive/internal/event/repository/db" // No longer needed here

	aggRepository "go-xlive/internal/aggregation/repository" // <-- 移动到这里
	db "go-xlive/internal/aggregation/repository/db"         // <-- 添加 db 别名

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// getLiveStatsKey generates the Redis key for live session statistics.
func getLiveStatsKey(sessionID string) string {
	return fmt.Sprintf("live:stats:%s", sessionID)
}

// AggregationService 结构体
type AggregationService struct {
	aggregationv1.UnimplementedAggregationServiceServer
	logger        *zap.Logger
	eventRepo     repository.Repository    // Added Event Repository
	aggRepo       aggRepository.Repository // <-- 新增: Aggregation Repository
	sessionClient sessionv1.SessionServiceClient
	redisClient   *redis.Client // Add Redis client
	tracer        trace.Tracer
}

// aggregationResult 存储聚合计算的中间结果
type aggregationResult struct {
	totalEvents        int64 // Sum of counts from queried tables
	totalDanmaku       int64 // Sourced from Redis (preferred) or DB
	totalGiftsValue    int64 // Sourced from Redis
	totalUniqueViewers int64 // Sourced from DB events with user_id
	totalLikes         int64 // Sourced from Redis
	totalWatched       int64 // Sourced from Redis
}

// NewAggregationService 创建实例
func NewAggregationService(logger *zap.Logger, eventRepo repository.Repository, aggRepo aggRepository.Repository, sessionClient sessionv1.SessionServiceClient, redisClient *redis.Client) *AggregationService { // <-- 添加 aggRepo
	if logger == nil {
		log.Fatal("AggregationService 需要 Logger")
	}
	if eventRepo == nil {
		log.Fatal("AggregationService 需要 Event Repository")
	}
	if aggRepo == nil { // <-- 添加检查
		log.Fatal("AggregationService 需要 Aggregation Repository")
	}
	if sessionClient == nil {
		log.Fatal("AggregationService 需要 SessionServiceClient")
	}
	if redisClient == nil {
		log.Fatal("AggregationService 需要 Redis Client")
	}
	return &AggregationService{
		logger:        logger.Named("agg_service"),
		eventRepo:     eventRepo,
		aggRepo:       aggRepo, // <-- 存储 aggRepo
		sessionClient: sessionClient,
		redisClient:   redisClient,
		tracer:        otel.Tracer("aggregation-service"),
	}
}

// ProcessAggregation 是核心聚合处理逻辑
func (s *AggregationService) ProcessAggregation(ctx context.Context, sessionID string) (*sessionv1.Session, error) {
	procCtx, span := s.tracer.Start(ctx, "ProcessAggregation")
	defer span.End()
	span.SetAttributes(attribute.String("session_id", sessionID))
	s.logger.Info("开始处理场次聚合", zap.String("session_id", sessionID))

	aggResult := aggregationResult{} // Initialize result struct
	userSet := make(map[string]bool) // For UV calculation
	var totalEventsCount int64 = 0   // For total events count

	queryCtx, queryCancel := context.WithTimeout(procCtx, 30*time.Second) // Context for all DB queries
	defer queryCancel()

	// 1. 从数据库查询特定事件 (弹幕, 存在, 舰长, SC, 互动) - 不再查询礼物
	s.logger.Info("正在从数据库查询特定事件...", zap.String("session_id", sessionID))

	// --- 移除 GiftSent 查询 ---
	// giftSentEvents, err := s.eventRepo.QueryGiftSentBySessionID(queryCtx, sessionID)
	// ... (移除相关处理逻辑) ...

	// Query Chat Messages
	chatMessageEvents, err := s.eventRepo.QueryChatMessageBySessionID(queryCtx, sessionID)
	if err != nil {
		s.logger.Error("查询 ChatMessage 事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("query.type", "ChatMessage")))
	} else {
		totalEventsCount += int64(len(chatMessageEvents))
		aggResult.totalDanmaku = int64(len(chatMessageEvents)) // DB Danmaku count (will be overwritten by Redis if available)
		for _, msg := range chatMessageEvents {
			if msg.UserID != "" {
				userSet[msg.UserID] = true
			}
		}
		s.logger.Debug("查询 ChatMessage 事件完成", zap.String("session_id", sessionID), zap.Int("count", len(chatMessageEvents)))
	}

	// Query User Presence
	userPresenceEvents, err := s.eventRepo.QueryUserPresenceBySessionID(queryCtx, sessionID)
	if err != nil {
		s.logger.Error("查询 UserPresence 事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("query.type", "UserPresence")))
	} else {
		totalEventsCount += int64(len(userPresenceEvents))
		for _, presence := range userPresenceEvents {
			if presence.UserID != "" {
				userSet[presence.UserID] = true
			}
		}
		s.logger.Debug("查询 UserPresence 事件完成", zap.String("session_id", sessionID), zap.Int("count", len(userPresenceEvents)))
	}

	// Query Guard Purchases
	guardPurchaseEvents, err := s.eventRepo.QueryGuardPurchaseBySessionID(queryCtx, sessionID)
	if err != nil {
		s.logger.Error("查询 GuardPurchase 事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("query.type", "GuardPurchase")))
	} else {
		totalEventsCount += int64(len(guardPurchaseEvents))
		for _, guard := range guardPurchaseEvents {
			if guard.UserID != "" {
				userSet[guard.UserID] = true
			}
		}
		s.logger.Debug("查询 GuardPurchase 事件完成", zap.String("session_id", sessionID), zap.Int("count", len(guardPurchaseEvents)))
	}

	// Query Super Chats
	superChatEvents, err := s.eventRepo.QuerySuperChatMessageBySessionID(queryCtx, sessionID)
	if err != nil {
		s.logger.Error("查询 SuperChatMessage 事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("query.type", "SuperChatMessage")))
	} else {
		totalEventsCount += int64(len(superChatEvents))
		for _, sc := range superChatEvents {
			if sc.UserID != "" {
				userSet[sc.UserID] = true
			}
		}
		s.logger.Debug("查询 SuperChatMessage 事件完成", zap.String("session_id", sessionID), zap.Int("count", len(superChatEvents)))
	}

	// Query User Interactions
	userInteractionEvents, err := s.eventRepo.QueryUserInteractionBySessionID(queryCtx, sessionID)
	if err != nil {
		s.logger.Error("查询 UserInteraction 事件失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("query.type", "UserInteraction")))
	} else {
		totalEventsCount += int64(len(userInteractionEvents))
		for _, interaction := range userInteractionEvents {
			if interaction.UserID != "" {
				userSet[interaction.UserID] = true
			}
		}
		s.logger.Debug("查询 UserInteraction 事件完成", zap.String("session_id", sessionID), zap.Int("count", len(userInteractionEvents)))
	}

	// Calculate final DB-based aggregates (UV, Total Events)
	aggResult.totalUniqueViewers = int64(len(userSet))
	aggResult.totalEvents = totalEventsCount // Sum of counts from queried tables

	s.logger.Info("数据库事件聚合计算完成 (UV, DB弹幕)", // Updated log message
		zap.String("session_id", sessionID),
		zap.Int64("total_events_db", aggResult.totalEvents),
		zap.Int64("total_danmaku_db", aggResult.totalDanmaku), // Log DB danmaku before potential Redis overwrite
		zap.Int64("total_uv_approx", aggResult.totalUniqueViewers),
	)
	span.SetAttributes(
		attribute.Int64("db.events_count", aggResult.totalEvents),
		attribute.Int64("db.danmaku_count", aggResult.totalDanmaku),
		attribute.Int64("db.uv_approx", aggResult.totalUniqueViewers),
	)

	// 2. 从 Redis 获取实时统计数据 (弹幕, 点赞, 观看, 礼物价值) - 覆盖 DB 弹幕数
	s.logger.Info("正在从 Redis 查询实时统计数据...", zap.String("session_id", sessionID))
	redisKey := getLiveStatsKey(sessionID)
	redisCtx, redisCancel := context.WithTimeout(procCtx, 2*time.Second)
	defer redisCancel()
	redisStats, err := s.redisClient.HGetAll(redisCtx, redisKey).Result()
	if err != nil && err != redis.Nil { // Ignore key not found error
		s.logger.Error("从 Redis 查询实时统计失败", zap.String("session_id", sessionID), zap.String("key", redisKey), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("redis.key", redisKey)))
		// Proceed using DB data as fallback where applicable (e.g., danmaku)
	} else if err == redis.Nil {
		s.logger.Warn("未在 Redis 中找到实时统计数据", zap.String("session_id", sessionID), zap.String("key", redisKey))
		span.AddEvent("Redis key not found for live stats", trace.WithAttributes(attribute.String("redis.key", redisKey)))
		// Keep DB values or set to 0 if no DB fallback exists
		aggResult.totalGiftsValue = 0 // Ensure gift value is 0 if key not found
		aggResult.totalLikes = 0
		aggResult.totalWatched = 0
	} else {
		s.logger.Info("从 Redis 查询到实时统计数据", zap.String("session_id", sessionID), zap.String("key", redisKey), zap.Any("stats", redisStats))
		span.AddEvent("Fetched live stats from Redis", trace.WithAttributes(attribute.String("redis.key", redisKey)))

		// Parse and update aggResult with Redis values
		if valStr, ok := redisStats["danmaku"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				aggResult.totalDanmaku = valInt // Overwrite DB count with Redis count
			} else {
				s.logger.Warn("无法解析 Redis 中的 danmaku 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
			}
		}
		if valStr, ok := redisStats["likes"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				aggResult.totalLikes = valInt
			} else {
				s.logger.Warn("无法解析 Redis 中的 likes 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
			}
		}
		if valStr, ok := redisStats["watched"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				aggResult.totalWatched = valInt
			} else {
				s.logger.Warn("无法解析 Redis 中的 watched 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
			}
		}
		// --- 新增: 解析 gift_value ---
		if valStr, ok := redisStats["gift_value"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				aggResult.totalGiftsValue = valInt // Get gift value from Redis
			} else {
				s.logger.Warn("无法解析 Redis 中的 gift_value 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
				aggResult.totalGiftsValue = 0 // Default to 0 on parse error
			}
		} else {
			// If gift_value field doesn't exist, assume 0
			aggResult.totalGiftsValue = 0
		}
		// --- gift_value 解析结束 ---
	}

	// 3. 记录最终聚合结果
	s.logger.Info("最终聚合结果计算完成",
		zap.String("session_id", sessionID),
		zap.Int64("total_events", aggResult.totalEvents),
		zap.Int64("total_danmaku", aggResult.totalDanmaku),        // Final danmaku count (prefer Redis)
		zap.Int64("total_gifts_value", aggResult.totalGiftsValue), // Final gift value (from Redis)
		zap.Int64("total_uv_approx", aggResult.totalUniqueViewers),
		zap.Int64("total_likes", aggResult.totalLikes),
		zap.Int64("total_watched", aggResult.totalWatched),
	)
	span.SetAttributes(
		attribute.Int64("aggregation.total_events", aggResult.totalEvents),
		attribute.Int64("aggregation.total_danmaku", aggResult.totalDanmaku),
		attribute.Int64("aggregation.total_gifts_value", aggResult.totalGiftsValue), // Value from Redis
		attribute.Int64("aggregation.uv_approx", aggResult.totalUniqueViewers),
		attribute.Int64("aggregation.total_likes", aggResult.totalLikes),
		attribute.Int64("aggregation.total_watched", aggResult.totalWatched),
	)

	// 4. 更新场次聚合结果到 Session Service (数据库)
	s.logger.Info("正在更新场次聚合结果...", zap.String("session_id", sessionID))
	updateCtx, updateCancel := context.WithTimeout(procCtx, 5*time.Second)
	defer updateCancel()
	updateReq := &sessionv1.UpdateSessionAggregatesRequest{
		SessionId:       sessionID,
		TotalEvents:     aggResult.totalEvents,
		TotalDanmaku:    aggResult.totalDanmaku,
		TotalGiftsValue: aggResult.totalGiftsValue, // Use value from Redis
		TotalLikes:      aggResult.totalLikes,
		TotalWatched:    aggResult.totalWatched,
	}
	updateResp, err := s.sessionClient.UpdateSessionAggregates(updateCtx, updateReq)
	if err != nil {
		s.logger.Error("更新场次聚合结果失败", zap.String("session_id", sessionID), zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to update session aggregates")
		st, _ := status.FromError(err)
		if st.Code() == codes.NotFound {
			//return nil, fmt.Errorf("更新聚合失败: 未找到场次 %s: %w", sessionID, err)
			return updateResp.GetUpdatedSession(), err
		} else if st.Code() == codes.DeadlineExceeded {
			return updateResp.GetUpdatedSession(), err
		}
		return nil, fmt.Errorf("更新场次聚合结果失败: %w", err)
	}
	s.logger.Info("场次聚合结果更新成功", zap.String("session_id", sessionID))

	// --- 新增: 将聚合结果快照存入时间线表 ---
	metricParams := db.InsertSessionMetricParams{ // <-- 使用 db 包下的类型
		SessionID:    sessionID,
		Timestamp:    pgtype.Timestamptz{Time: time.Now(), Valid: true}, // 使用当前时间作为快照时间点
		DanmakuCount: aggResult.totalDanmaku,
		GiftValue:    aggResult.totalGiftsValue, // Use value from Redis
		LikeCount:    aggResult.totalLikes,
		WatchedCount: aggResult.totalWatched,
	}
	_, insertErr := s.aggRepo.InsertSessionMetric(ctx, metricParams) // 使用传入的 ctx
	if insertErr != nil {
		// 只记录错误，不影响聚合结果的返回
		s.logger.Error("保存聚合指标快照失败", zap.String("session_id", sessionID), zap.Error(insertErr))
		span.RecordError(insertErr, trace.WithAttributes(attribute.String("operation", "InsertSessionMetricSnapshot")))
	} else {
		s.logger.Info("成功保存聚合指标快照", zap.String("session_id", sessionID))
	}
	// --- 快照存储结束 ---

	span.SetStatus(otelCodes.Ok, "Aggregation processed successfully")
	return updateResp.GetUpdatedSession(), nil
}

// TriggerAggregation 手动触发聚合 (可选的 gRPC 接口)
func (s *AggregationService) TriggerAggregation(ctx context.Context, req *aggregationv1.TriggerAggregationRequest) (*aggregationv1.TriggerAggregationResponse, error) {
	procCtx, span := s.tracer.Start(ctx, "TriggerAggregationRPC")
	defer span.End()
	span.SetAttributes(attribute.String("session_id", req.GetSessionId()))

	s.logger.Info("收到手动触发聚合请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		span.SetStatus(otelCodes.Error, "Invalid argument: session_id required")
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id")
	}
	updatedSession, err := s.ProcessAggregation(procCtx, req.GetSessionId())
	if err != nil {
		s.logger.Error("手动触发聚合处理失败", zap.String("session_id", req.GetSessionId()), zap.Error(err))
		// Return the potentially partially updated session along with the error
		return &aggregationv1.TriggerAggregationResponse{UpdatedSession: updatedSession}, err
	}

	s.logger.Info("手动触发聚合处理成功", zap.String("session_id", req.GetSessionId()))
	return &aggregationv1.TriggerAggregationResponse{UpdatedSession: updatedSession}, nil
}

// HandleStreamStatus 实现新的 gRPC 方法
func (s *AggregationService) HandleStreamStatus(ctx context.Context, req *aggregationv1.StreamStatus) (*emptypb.Empty, error) {
	_, span := s.tracer.Start(ctx, "HandleStreamStatus")
	defer span.End()
	span.SetAttributes(
		attribute.String("session_id", req.GetSessionId()),
		attribute.String("room_id", req.GetRoomId()),
		attribute.Bool("is_live", req.GetIsLive()),
	)

	s.logger.Info("收到 HandleStreamStatus 请求",
		zap.String("session_id", req.GetSessionId()),
		zap.String("room_id", req.GetRoomId()),
		zap.Bool("is_live", req.GetIsLive()),
		zap.Time("timestamp", req.GetTimestamp().AsTime()),
	)

	if !req.GetIsLive() { // 直播结束时触发聚合
		s.logger.Info("检测到直播结束，准备触发聚合", zap.String("session_id", req.GetSessionId()))
		// 异步触发聚合，避免阻塞 gRPC 调用方
		go func() {
			// 创建一个新的后台 context，不依赖原始 gRPC 请求的 context
			aggCtx, aggCancel := context.WithTimeout(context.Background(), 5*time.Minute) // 设置一个较长的超时时间
			defer aggCancel()
			_, err := s.ProcessAggregation(aggCtx, req.GetSessionId())
			if err != nil {
				s.logger.Error("异步聚合处理失败", zap.String("session_id", req.GetSessionId()), zap.Error(err))
				// 这里可以考虑添加重试逻辑或告警
			} else {
				s.logger.Info("异步聚合处理成功", zap.String("session_id", req.GetSessionId()))
			}
		}()
	} else {
		s.logger.Info("检测到直播开始，无需立即聚合", zap.String("session_id", req.GetSessionId()))
	}

	return &emptypb.Empty{}, nil
}

// HealthCheck 实现
func (s *AggregationService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*aggregationv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (AggregationService)")
	healthStatus := "SERVING"
	var firstError error

	// 检查 Event Database
	checkCtxEventRepo, cancelEventRepo := context.WithTimeout(ctx, 2*time.Second)
	defer cancelEventRepo()
	// Use Ping method from the repository interface
	if err := s.eventRepo.Ping(checkCtxEventRepo); err != nil {
		s.logger.Error("依赖健康检查失败: Event Database", zap.Error(err))
		healthStatus = "NOT_SERVING"
		firstError = err
	}

	// 检查 Redis
	checkCtxRedis, cancelRedis := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRedis()
	if _, err := s.redisClient.Ping(checkCtxRedis).Result(); err != nil {
		s.logger.Error("依赖健康检查失败: Redis", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// 检查 SessionService
	checkCtxSession, cancelSession := context.WithTimeout(ctx, 1*time.Second)
	defer cancelSession()
	_, err := s.sessionClient.HealthCheck(checkCtxSession, &emptypb.Empty{})
	if err != nil {
		s.logger.Error("依赖健康检查失败: SessionService", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// Return status based on checks
	if firstError != nil {
		// Optionally return the first error encountered for more detail,
		// but the spec just asks for status string.
		return &aggregationv1.HealthCheckResponse{Status: healthStatus}, nil
	}
	return &aggregationv1.HealthCheckResponse{Status: healthStatus}, nil
}

// --- 新增: 查询场次指标时间线 ---
// GetSessionMetricsTimelineBiz 处理查询指定场次在时间范围内的指标时间线数据的业务逻辑
func (s *AggregationService) GetSessionMetricsTimelineBiz(ctx context.Context, sessionID string, startTime, endTime time.Time) ([]db.GetSessionMetricsTimelineRow, error) {
	bizCtx, span := s.tracer.Start(ctx, "GetSessionMetricsTimelineBiz")
	defer span.End()
	span.SetAttributes(
		attribute.String("session_id", sessionID),
		attribute.String("start_time", startTime.Format(time.RFC3339)),
		attribute.String("end_time", endTime.Format(time.RFC3339)),
	)

	s.logger.Info("开始查询场次指标时间线",
		zap.String("session_id", sessionID),
		zap.Time("start_time", startTime),
		zap.Time("end_time", endTime),
	)

	// 参数校验 (可选，但推荐)
	if sessionID == "" {
		err := status.Error(codes.InvalidArgument, "session_id 不能为空")
		span.SetStatus(otelCodes.Error, err.Error())
		return nil, err
	}
	if startTime.IsZero() || endTime.IsZero() || startTime.After(endTime) {
		err := status.Error(codes.InvalidArgument, "无效的时间范围")
		span.SetStatus(otelCodes.Error, err.Error())
		return nil, err
	}

	// 调用 Repository 获取数据
	timelineData, err := s.aggRepo.GetSessionMetricsTimeline(bizCtx, sessionID, startTime, endTime)
	if err != nil {
		s.logger.Error("查询场次指标时间线失败",
			zap.String("session_id", sessionID),
			zap.Time("start_time", startTime),
			zap.Time("end_time", endTime),
			zap.Error(err),
		)
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to get session metrics timeline from repository")
		// 可以根据错误类型返回不同的 gRPC 状态码，例如 pgx.ErrNoRows -> codes.NotFound
		return nil, status.Errorf(codes.Internal, "查询时间线数据失败: %v", err)
	}

	s.logger.Info("成功查询到场次指标时间线数据",
		zap.String("session_id", sessionID),
		zap.Int("data_points", len(timelineData)),
	)
	span.SetAttributes(attribute.Int("result.data_points", len(timelineData)))
	span.SetStatus(otelCodes.Ok, "Successfully retrieved session metrics timeline")

	return timelineData, nil
}

// --- 新增: 实现 GetSessionMetricsTimeline gRPC 接口 ---
func (s *AggregationService) GetSessionMetricsTimeline(ctx context.Context, req *aggregationv1.GetSessionMetricsTimelineRequest) (*aggregationv1.GetSessionMetricsTimelineResponse, error) {
	// 验证请求参数
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "请求不能为空")
	}
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id 不能为空")
	}
	if req.GetStartTime() == nil || req.GetEndTime() == nil {
		return nil, status.Error(codes.InvalidArgument, "start_time 和 end_time 不能为空")
	}
	startTime := req.GetStartTime().AsTime()
	endTime := req.GetEndTime().AsTime()
	if startTime.IsZero() || endTime.IsZero() || startTime.After(endTime) {
		return nil, status.Error(codes.InvalidArgument, "无效的时间范围")
	}

	// 调用业务逻辑层
	timelineData, err := s.GetSessionMetricsTimelineBiz(ctx, req.GetSessionId(), startTime, endTime)
	if err != nil {
		// GetSessionMetricsTimelineBiz 内部已经记录了错误日志和 span 状态
		// 直接返回从 Biz 层获取的 gRPC 状态错误
		return nil, err
	}

	// 转换结果为 Protobuf 格式
	respDataPoints := make([]*aggregationv1.SessionMetricPoint, 0, len(timelineData))
	for _, row := range timelineData {
		// 确保 pgtype.Timestamptz 有效
		var ts *timestamppb.Timestamp
		if row.Timestamp.Valid {
			ts = timestamppb.New(row.Timestamp.Time)
		} else {
			// 如果时间戳无效，可以跳过或记录警告，这里选择跳过
			s.logger.Warn("查询到的时间线数据点时间戳无效", zap.String("session_id", req.GetSessionId()), zap.Any("row", row))
			continue
		}

		respDataPoints = append(respDataPoints, &aggregationv1.SessionMetricPoint{
			Timestamp:       ts,
			DanmakuCount:    row.DanmakuCount,
			GiftValue:       row.GiftValue,
			LikeCount:       row.LikeCount,
			WatchedCount:    row.WatchedCount,
			OnlineRankCount: row.OnlineRankCount, // 新增
		})
	}

	return &aggregationv1.GetSessionMetricsTimelineResponse{
		DataPoints: respDataPoints,
	}, nil
}

// SaveMetricSnapshot fetches current live stats (mainly from Redis) and saves a snapshot.
func (s *AggregationService) SaveMetricSnapshot(ctx context.Context, sessionID string) error {
	saveCtx, span := s.tracer.Start(ctx, "SaveMetricSnapshot")
	defer span.End()
	span.SetAttributes(attribute.String("session_id", sessionID))
	s.logger.Debug("开始保存指标快照", zap.String("session_id", sessionID))

	var danmakuCount, likeCount, watchedCount, onlineRankCount, giftValue int64 // Added giftValue

	// 1. 从 Redis 获取实时统计数据 (弹幕, 点赞, 观看, 在线排名计数, 礼物价值)
	redisKey := getLiveStatsKey(sessionID)
	redisCtx, redisCancel := context.WithTimeout(saveCtx, 2*time.Second)
	defer redisCancel()
	redisStats, err := s.redisClient.HGetAll(redisCtx, redisKey).Result()
	if err != nil && err != redis.Nil {
		s.logger.Error("保存快照时从 Redis 查询实时统计失败", zap.String("session_id", sessionID), zap.String("key", redisKey), zap.Error(err))
		span.RecordError(err, trace.WithAttributes(attribute.String("redis.key", redisKey)))
		span.SetStatus(otelCodes.Error, "Failed to get live stats from Redis")
		// 如果无法从 Redis 获取，则不保存快照。
		return fmt.Errorf("从 Redis 获取实时统计失败: %w", err)
	} else if err == redis.Nil {
		s.logger.Warn("保存快照时未在 Redis 中找到实时统计数据，将使用 0 值", zap.String("session_id", sessionID), zap.String("key", redisKey))
		span.AddEvent("Redis key not found for live stats, using 0 values for snapshot", trace.WithAttributes(attribute.String("redis.key", redisKey)))
		// 使用默认值 0
		danmakuCount = 0
		likeCount = 0
		watchedCount = 0
		onlineRankCount = 0
		giftValue = 0
	} else {
		// Parse Redis values
		if valStr, ok := redisStats["danmaku"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				danmakuCount = valInt
			}
		}
		if valStr, ok := redisStats["likes"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				likeCount = valInt
			}
		}
		if valStr, ok := redisStats["watched"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				watchedCount = valInt
			}
		}
		if valStr, ok := redisStats["online_rank_count"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				onlineRankCount = valInt
			} else {
				s.logger.Warn("无法解析 Redis 中的 online_rank_count 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
			}
		}
		// --- 新增: 解析 gift_value ---
		if valStr, ok := redisStats["gift_value"]; ok {
			if valInt, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				giftValue = valInt
			} else {
				s.logger.Warn("无法解析 Redis 中的 gift_value 计数值", zap.String("session_id", sessionID), zap.String("value", valStr), zap.Error(err))
			}
		}
		// --- gift_value 解析结束 ---
		s.logger.Debug("保存快照时从 Redis 获取到实时统计", zap.String("session_id", sessionID), zap.Int64("danmaku", danmakuCount), zap.Int64("likes", likeCount), zap.Int64("watched", watchedCount), zap.Int64("online_rank_count", onlineRankCount), zap.Int64("gift_value", giftValue))
	}

	// 2. 插入快照到数据库
	metricParams := db.InsertSessionMetricParams{
		SessionID:       sessionID,
		Timestamp:       pgtype.Timestamptz{Time: time.Now(), Valid: true}, // 使用当前时间
		DanmakuCount:    danmakuCount,
		GiftValue:       giftValue, // Use value from Redis
		LikeCount:       likeCount,
		WatchedCount:    watchedCount,
		OnlineRankCount: onlineRankCount, // 新增
	}
	_, insertErr := s.aggRepo.InsertSessionMetric(saveCtx, metricParams)
	if insertErr != nil {
		s.logger.Error("保存聚合指标快照失败 (SaveMetricSnapshot)", zap.String("session_id", sessionID), zap.Error(insertErr))
		span.RecordError(insertErr, trace.WithAttributes(attribute.String("operation", "InsertSessionMetricSnapshot")))
		span.SetStatus(otelCodes.Error, "Failed to insert metric snapshot")
		return fmt.Errorf("插入指标快照失败: %w", insertErr)
	}

	s.logger.Info("成功保存指标快照", zap.String("session_id", sessionID))
	span.SetStatus(otelCodes.Ok, "Metric snapshot saved")
	return nil
}

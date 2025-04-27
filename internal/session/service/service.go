package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-xlive/pkg/observability"
	"log"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"  // Keep for potential internal status creation
	"google.golang.org/grpc/status" // Keep for potential internal status creation/checking
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	aggregationv1 "go-xlive/gen/go/aggregation/v1"
	eventv1 "go-xlive/gen/go/event/v1"
	roomv1 "go-xlive/gen/go/room/v1"
	sessionv1 "go-xlive/gen/go/session/v1"

	"go-xlive/internal/session/repository" // Keep repository dependency
	// <-- 新增: 用于 JSON 序列化
	db "go-xlive/internal/session/repository/db"
)

// --- Define simple business errors (optional, but good practice) ---
var (
	ErrSessionNotFound      = errors.New("session not found")
	ErrSessionAlreadyExists = errors.New("session already exists")
	ErrSessionNotLive       = errors.New("session is not live")
	ErrRoomNotFound         = errors.New("room not found")
	ErrInvalidInput         = errors.New("invalid input")
)

// --- NATS 主题常量 ---
const (
	NatsPlatformStreamStatusSubject = "platform.event.stream.status"
	NatsSessionUpdatedSubject       = "platform.event.session.updated" // <-- 新增: Session 更新主题 (给 adapter 等服务使用)
)

// SessionStatusUpdateEventJSON 定义 NATS 会话状态更新事件消息结构 (JSON)
// 用于通知其他服务（如 adapter）会话状态变化
type SessionStatusUpdateEventJSON struct {
	RoomID    string `json:"room_id"` // Internal Room ID
	SessionID string `json:"session_id"`
	IsLive    bool   `json:"is_live"`   // true for created/active, false for ended/deleted
	Timestamp int64  `json:"timestamp"` // Unix timestamp in milliseconds
}

// --- PlatformStreamStatusEventJSON 结构体 ---
type PlatformStreamStatusEventJSON struct {
	RoomID       string `json:"room_id"`
	SessionID    string `json:"session_id,omitempty"`
	IsLive       bool   `json:"is_live"`
	Timestamp    int64  `json:"timestamp"`
	RoomTitle    string `json:"room_title"`
	StreamerName string `json:"streamer_name"`
}

// Service 结构体 (原 SessionService) - 现在只包含业务逻辑
type Service struct {
	repo              repository.Repository
	logger            *zap.Logger
	roomClient        roomv1.RoomServiceClient
	eventClient       eventv1.EventServiceClient
	redisClient       *redis.Client
	natsConn          *nats.Conn
	aggregationClient aggregationv1.AggregationServiceClient
	natsSubs          []*nats.Subscription
}

// NewService 创建业务逻辑服务实例 (原 NewSessionService)
func NewService(
	logger *zap.Logger,
	repo repository.Repository,
	roomClient roomv1.RoomServiceClient,
	eventClient eventv1.EventServiceClient,
	redisClient *redis.Client,
	natsConn *nats.Conn,
	aggregationClient aggregationv1.AggregationServiceClient,
) *Service {
	if logger == nil {
		log.Fatal("Session Service 需要 Logger")
	}
	if repo == nil {
		log.Fatal("Session Service 需要 Repository")
	}
	if roomClient == nil {
		log.Fatal("Session Service 需要 RoomServiceClient")
	}
	if eventClient == nil {
		log.Fatal("Session Service 需要 EventServiceClient")
	}
	if redisClient == nil {
		log.Fatal("Session Service 需要 Redis Client")
	}
	if natsConn == nil {
		log.Fatal("Session Service 需要 NATS Connection")
	}
	if aggregationClient == nil {
		log.Fatal("Session Service 需要 AggregationServiceClient")
	}
	s := &Service{
		logger:            logger.Named("session_service"),
		repo:              repo,
		roomClient:        roomClient,
		eventClient:       eventClient,
		redisClient:       redisClient,
		natsConn:          natsConn,
		aggregationClient: aggregationClient,
		natsSubs:          make([]*nats.Subscription, 0),
	}
	return s
}

// --- 辅助函数保持不变 ---

func getSessionCacheKey(sessionID string) string {
	return fmt.Sprintf("cache:session:%s", sessionID)
}

// getLiveSessionByRoomCacheKey 生成按房间ID查询直播中会话的缓存键
func getLiveSessionByRoomCacheKey(roomID string) string {
	return fmt.Sprintf("cache:live_session_by_room:%s", roomID)
}

// getNegativeCacheKey 生成用于缓存“未找到”状态的键
func getNegativeCacheKey(key string) string {
	return fmt.Sprintf("%s:not_found", key)
}

func mapDBSessionToProto(dbSession db.Session) *sessionv1.Session {
	protoSession := &sessionv1.Session{
		SessionId:       dbSession.SessionID,
		RoomId:          dbSession.RoomID,
		OwnerUserId:     dbSession.OwnerUserID,
		StartTime:       timestamppb.New(dbSession.StartTime.Time),
		Status:          dbSession.Status,
		TotalEvents:     dbSession.TotalEvents,
		TotalDanmaku:    dbSession.TotalDanmaku,
		TotalGiftsValue: dbSession.TotalGiftsValue,
		TotalLikes:      dbSession.TotalLikes,
		TotalWatched:    dbSession.TotalWatched,
		SessionTitle:    dbSession.SessionTitle.String,
		StreamerName:    dbSession.AnchorName.String,
		RoomTitle:       dbSession.RoomTitle.String,
	}
	if dbSession.EndTime.Valid {
		protoSession.EndTime = timestamppb.New(dbSession.EndTime.Time)
	}
	return protoSession
}

func (s *Service) updateSessionCache(ctx context.Context, session *sessionv1.Session) {
	if session == nil {
		return
	}
	cacheKey := getSessionCacheKey(session.SessionId)
	sessionBytes, err := proto.Marshal(session)
	if err != nil {
		s.logger.Error("序列化 Session 到 Protobuf 失败 (缓存更新)", zap.Error(err), zap.String("session_id", session.SessionId))
		return
	}
	ttl, err := s.redisClient.TTL(ctx, cacheKey).Result()
	if err != nil || ttl <= 0 {
		if session.Status == "ended" {
			ttl = 1 * time.Hour
		} else {
			ttl = 24 * time.Hour
		}
	}
	err = s.redisClient.Set(ctx, cacheKey, sessionBytes, ttl).Err()
	if err != nil {
		s.logger.Error("更新 Redis 缓存中的 Session (Protobuf) 失败", zap.Error(err), zap.String("key", cacheKey))
	} else {
		s.logger.Debug("已更新 Redis 缓存中的 Session (Protobuf)", zap.String("key", cacheKey))
	}
}

func (s *Service) deleteSessionCache(ctx context.Context, sessionID string) {
	cacheKey := getSessionCacheKey(sessionID)
	err := s.redisClient.Del(ctx, cacheKey).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		s.logger.Error("从 Redis 缓存删除 Session 失败", zap.Error(err), zap.String("key", cacheKey))
	} else {
		s.logger.Debug("已从 Redis 缓存删除 Session (如果存在)", zap.String("key", cacheKey))
	}
}

// publishSessionStatusUpdate 发布会话状态更新到 NATS
// isLive: true 表示会话开始/活跃, false 表示会话结束
func (s *Service) publishSessionStatusUpdate(ctx context.Context, sessionID, roomID string, isLive bool, eventTime time.Time) {
	tracer := otel.Tracer("session-service-publisher")
	pubCtx, span := tracer.Start(ctx, "publishSessionStatusUpdate")
	defer span.End()
	span.SetAttributes(
		attribute.String("publish.subject", NatsSessionUpdatedSubject),
		attribute.String("publish.session_id", sessionID),
		attribute.String("publish.room_id", roomID),
		attribute.Bool("publish.is_live", isLive),
	)

	event := SessionStatusUpdateEventJSON{
		RoomID:    roomID,
		SessionID: sessionID,
		IsLive:    isLive,
		Timestamp: eventTime.UnixMilli(), // 使用毫秒时间戳
	}

	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.Error("序列化 SessionStatusUpdateEvent JSON 失败",
			zap.Error(err),
			zap.String("session_id", sessionID),
			zap.String("room_id", roomID),
		)
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "JSON marshal failed")
		return
	}

	// 注入追踪头
	traceHeadersMap := observability.InjectNATSHeaders(pubCtx)
	natsHeader := make(nats.Header)
	for k, v := range traceHeadersMap {
		natsHeader[k] = []string{v}
	}
	msg := &nats.Msg{Subject: NatsSessionUpdatedSubject, Data: payload, Header: natsHeader}

	if err := s.natsConn.PublishMsg(msg); err != nil {
		s.logger.Error("发布 SessionStatusUpdateEvent 到 NATS 失败",
			zap.Error(err),
			zap.String("subject", NatsSessionUpdatedSubject),
			zap.String("session_id", sessionID),
		)
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "NATS publish failed")
	} else {
		s.logger.Info("成功发布 SessionStatusUpdateEvent 到 NATS",
			zap.String("subject", NatsSessionUpdatedSubject),
			zap.String("session_id", sessionID),
			zap.String("room_id", roomID),
			zap.Bool("is_live", isLive),
		)
		span.SetStatus(otelCodes.Ok, "Event published")
	}
}

// --- 业务逻辑方法 ---

// CreateSession contains the business logic for creating a session.
// 注意：此方法目前主要由 HandlePlatformStreamStatusEvent 内部调用。
// 如果需要直接的 API 调用，Handler 层需要调用此方法。
// 参数已修改为接收具体值而不是 gRPC 请求对象。
func (s *Service) CreateSession(ctx context.Context, roomID, sessionTitle, roomTitle, streamerName string) (*sessionv1.Session, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "CreateSession") // Renamed span
	defer span.End()
	span.SetAttributes(
		attribute.String("biz.room_id", roomID),        // Use function argument
		attribute.String("trigger", "internal_or_api"), // Indicate potential trigger
	)

	s.logger.Info("Biz: Creating session",
		zap.String("room_id", roomID),             // Use function argument
		zap.String("session_title", sessionTitle), // Use function argument
		zap.String("room_title", roomTitle),       // Use function argument
		zap.String("streamer_name", streamerName), // Use function argument
	)

	// 1. Check for existing live session
	liveSession, err := s.repo.GetLiveSessionByRoomID(ctx, roomID) // Use function argument
	if err == nil && liveSession.SessionID != "" {
		s.logger.Warn("Biz: Room already has a live session", zap.String("room_id", roomID), zap.String("existing_session_id", liveSession.SessionID))
		span.SetStatus(otelCodes.Error, "Live session already exists")
		// Return specific business error
		return nil, fmt.Errorf("%w: 房间 %s 已有活跃会话 %s", ErrSessionAlreadyExists, roomID, liveSession.SessionID) // <-- 使用业务错误
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Error("Biz: Failed to check for existing live session", zap.Error(err), zap.String("room_id", roomID))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "DB error checking existing session")
		return nil, fmt.Errorf("检查现有会话失败: %w", err)
	}

	// 2. Get room info
	ownerUserID := ""
	roomResp, err := s.roomClient.GetRoom(ctx, &roomv1.GetRoomRequest{RoomId: roomID}) // Use function argument
	if err != nil {
		s.logger.Error("Biz: Failed to get room info, using empty OwnerUserID", zap.String("room_id", roomID), zap.Error(err))
		span.RecordError(err)
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.NotFound {
			span.SetStatus(otelCodes.Error, "Room not found")
			// Return specific business error
			return nil, fmt.Errorf("%w: 房间 ID %s 不存在", ErrRoomNotFound, roomID) // <-- 使用业务错误
		}
	} else if roomResp.GetOwner() == nil {
		s.logger.Warn("Biz: Room info retrieved but owner is nil", zap.String("room_id", roomID))
	} else {
		ownerUserID = roomResp.GetOwner().GetUserId()
	}
	span.SetAttributes(attribute.String("biz.owner_user_id", ownerUserID))

	// 3. Create session in DB
	sessionID := uuid.NewString()
	startTime := time.Now()
	params := db.CreateSessionParams{
		SessionID:    sessionID,
		RoomID:       roomID, // Use function argument
		OwnerUserID:  ownerUserID,
		StartTime:    pgtype.Timestamptz{Time: startTime, Valid: true},
		Status:       "live",
		SessionTitle: pgtype.Text{String: sessionTitle, Valid: sessionTitle != ""}, // Use function argument
		AnchorName:   pgtype.Text{String: streamerName, Valid: streamerName != ""}, // Use function argument
		RoomTitle:    pgtype.Text{String: roomTitle, Valid: roomTitle != ""},       // Use function argument
	}
	createdDBSession, err := s.repo.CreateSession(ctx, params)
	if err != nil {
		s.logger.Error("Biz: Failed to create session in DB", zap.Error(err), zap.String("room_id", roomID))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to create session in DB")
		return nil, fmt.Errorf("创建场次数据库操作失败: %w", err)
	}
	protoSession := mapDBSessionToProto(createdDBSession)
	span.SetAttributes(attribute.String("session.id", protoSession.SessionId))

	// --- 新增: 发布 Session 创建事件到 NATS (通知 Adapter 等) ---
	go s.publishSessionStatusUpdate(ctx, protoSession.SessionId, protoSession.RoomId, true, protoSession.StartTime.AsTime())
	// --- NATS 发布结束 ---

	// 4. Notify Aggregation Service
	go func(parentCtx context.Context, sess *sessionv1.Session) {
		aggCtx, aggCancel := context.WithTimeout(parentCtx, 5*time.Second)
		defer aggCancel()
		aggTracer := otel.Tracer("session-service-goroutine")
		gCtx, gSpan := aggTracer.Start(aggCtx, "notifyAggregationServiceLiveBiz")
		defer gSpan.End()
		gSpan.SetAttributes(
			attribute.String("session.id", sess.SessionId),
			attribute.Bool("event.is_live", true),
			attribute.String("trigger", "api"),
		)
		_, aggErr := s.aggregationClient.HandleStreamStatus(gCtx, &aggregationv1.StreamStatus{
			SessionId: sess.SessionId, RoomId: sess.RoomId, IsLive: true, Timestamp: sess.StartTime,
		})
		if aggErr != nil {
			s.logger.Error("Biz Goroutine: Failed to notify Aggregation (Live)", zap.String("session_id", sess.SessionId), zap.Error(aggErr))
			gSpan.RecordError(aggErr)
			gSpan.SetStatus(otelCodes.Error, "Failed to call aggregation service")
		} else {
			s.logger.Info("Biz Goroutine: Notified Aggregation (Live)", zap.String("session_id", sess.SessionId))
			gSpan.SetStatus(otelCodes.Ok, "Aggregation service notified")
		}
	}(ctx, protoSession)

	// 5. Update cache (main session cache and live session by room cache)
	s.updateSessionCache(ctx, protoSession) // Update main session cache first
	liveCacheKey := getLiveSessionByRoomCacheKey(protoSession.RoomId)
	negativeCacheKey := getNegativeCacheKey(liveCacheKey)

	// --- Ensure negative cache is removed BEFORE setting positive cache ---
	delErr := s.redisClient.Del(ctx, negativeCacheKey).Err()
	if delErr != nil && !errors.Is(delErr, redis.Nil) {
		s.logger.Warn("Biz: Failed to delete negative cache before setting positive cache on create", zap.Error(delErr), zap.String("key", negativeCacheKey))
		// Continue anyway, setting positive cache is more important
	} else if delErr == nil {
		s.logger.Debug("Biz: Deleted negative cache before setting positive cache on create (if existed)", zap.String("key", negativeCacheKey))
	}

	// Now set the positive cache
	errCache := s.redisClient.Set(ctx, liveCacheKey, protoSession.SessionId, 24*time.Hour).Err()
	if errCache != nil {
		s.logger.Error("Biz: Failed to cache live session ID by room on create", zap.Error(errCache), zap.String("key", liveCacheKey))
	} else {
		s.logger.Debug("Biz: Cached live session ID by room on create", zap.String("key", liveCacheKey), zap.String("session_id", protoSession.SessionId))
	}

	s.logger.Info("Biz: Session created successfully", zap.String("session_id", protoSession.SessionId))
	span.SetStatus(otelCodes.Ok, "Session created")
	return protoSession, nil
}

// GetSession contains the business logic for getting session details.
func (s *Service) GetSession(ctx context.Context, sessionID string) (*sessionv1.Session, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "GetSession") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.session_id", sessionID))

	s.logger.Debug("Biz: Getting session", zap.String("session_id", sessionID))

	cacheKey := getSessionCacheKey(sessionID)
	// 1. Try cache first
	cachedData, err := s.redisClient.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var cachedSession sessionv1.Session
		if err := proto.Unmarshal(cachedData, &cachedSession); err == nil {
			s.logger.Debug("Biz: Cache hit for session", zap.String("key", cacheKey))
			span.SetAttributes(attribute.Bool("cache.hit", true))
			span.SetStatus(otelCodes.Ok, "Cache hit")
			return &cachedSession, nil
		}
		s.logger.Error("Biz: Failed to unmarshal cached session, deleting cache", zap.Error(err), zap.String("key", cacheKey))
		span.RecordError(err)
		s.deleteSessionCache(context.Background(), sessionID)
	} else if !errors.Is(err, redis.Nil) {
		s.logger.Error("Biz: Failed to read session cache", zap.Error(err), zap.String("key", cacheKey))
		span.RecordError(err)
	} else {
		s.logger.Debug("Biz: Cache miss for session", zap.String("key", cacheKey))
		span.SetAttributes(attribute.Bool("cache.hit", false))
	}

	// 2. Read from DB if cache miss or error
	dbSession, err := s.repo.GetSessionByID(ctx, sessionID)
	if err != nil {
		s.logger.Warn("Biz: Failed to get session from DB", zap.Error(err), zap.String("session_id", sessionID))
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			span.SetStatus(otelCodes.Error, "Session not found in DB")
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, sessionID) // <-- 使用业务错误
		}
		span.SetStatus(otelCodes.Error, "DB error getting session")
		return nil, fmt.Errorf("获取场次数据库操作失败: %w", err)
	}

	protoSession := mapDBSessionToProto(dbSession)

	// 3. Cache backfill
	s.updateSessionCache(context.Background(), protoSession)

	s.logger.Debug("Biz: Session retrieved from DB", zap.String("session_id", protoSession.SessionId))
	span.SetStatus(otelCodes.Ok, "Retrieved from DB")
	return protoSession, nil
}

// EndSession contains the business logic for ending a session.
func (s *Service) EndSession(ctx context.Context, sessionID string) (*sessionv1.Session, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "EndSession") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.session_id", sessionID))

	s.logger.Info("Biz: Ending session", zap.String("session_id", sessionID))

	// 1. Get session to check status
	existingSession, getErr := s.repo.GetSessionByID(ctx, sessionID)
	if getErr != nil {
		span.RecordError(getErr)
		if errors.Is(getErr, pgx.ErrNoRows) {
			s.logger.Warn("Biz: Session to end not found", zap.String("session_id", sessionID))
			span.SetStatus(otelCodes.Error, "Session not found")
			return nil, fmt.Errorf("%w: 未找到要结束的场次 %s", ErrSessionNotFound, sessionID) // <-- 使用业务错误
		}
		s.logger.Error("Biz: Failed to get session before ending", zap.Error(getErr), zap.String("session_id", sessionID))
		span.SetStatus(otelCodes.Error, "DB error getting session")
		return nil, fmt.Errorf("检查场次状态失败: %w", getErr)
	}

	// Check status
	if existingSession.Status == "ended" {
		s.logger.Info("Biz: Session already ended", zap.String("session_id", sessionID))
		span.SetStatus(otelCodes.Ok, "Session already ended")
		return mapDBSessionToProto(existingSession), nil
	}
	if existingSession.Status != "live" {
		s.logger.Warn("Biz: Cannot end session in current state", zap.String("session_id", sessionID), zap.String("status", existingSession.Status))
		span.SetStatus(otelCodes.Error, "Invalid session state")
		return nil, fmt.Errorf("%w: 场次当前状态 (%s)", ErrSessionNotLive, existingSession.Status) // <-- 使用业务错误
	}

	// 2. End session in DB
	endTime := time.Now()
	params := db.EndSessionParams{
		SessionID: sessionID,
		EndTime:   pgtype.Timestamptz{Time: endTime, Valid: true},
	}
	endedDBSession, err := s.repo.EndSession(ctx, params)
	if err != nil {
		s.logger.Error("Biz: Failed to end session in DB", zap.Error(err), zap.String("session_id", sessionID))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "Failed to end session in DB")
		return nil, fmt.Errorf("结束场次数据库操作失败: %w", err)
	}
	protoSession := mapDBSessionToProto(endedDBSession)

	// --- 新增: 发布 Session 结束事件到 NATS (通知 Adapter 等) ---
	go s.publishSessionStatusUpdate(ctx, protoSession.SessionId, protoSession.RoomId, false, protoSession.EndTime.AsTime())
	// --- NATS 发布结束 ---

	// 3. Update cache (main session cache and remove live session by room cache)
	s.updateSessionCache(ctx, protoSession)
	liveCacheKey := getLiveSessionByRoomCacheKey(protoSession.RoomId)
	errCache := s.redisClient.Del(ctx, liveCacheKey).Err()
	if errCache != nil && !errors.Is(errCache, redis.Nil) {
		s.logger.Error("Biz: Failed to delete live session ID cache by room on end", zap.Error(errCache), zap.String("key", liveCacheKey))
	} else {
		s.logger.Debug("Biz: Deleted live session ID cache by room on end (if existed)", zap.String("key", liveCacheKey))
	}

	// 4. Notify Aggregation Service
	go func(parentCtx context.Context, sess *sessionv1.Session) {
		aggCtx, aggCancel := context.WithTimeout(parentCtx, 5*time.Second)
		defer aggCancel()
		aggTracer := otel.Tracer("session-service-goroutine")
		gCtx, gSpan := aggTracer.Start(aggCtx, "notifyAggregationServiceOfflineBiz")
		defer gSpan.End()
		gSpan.SetAttributes(
			attribute.String("session.id", sess.SessionId),
			attribute.Bool("event.is_live", false),
			attribute.String("trigger", "api"),
		)
		_, aggErr := s.aggregationClient.HandleStreamStatus(gCtx, &aggregationv1.StreamStatus{
			SessionId: sess.SessionId, RoomId: sess.RoomId, IsLive: false, Timestamp: sess.EndTime,
		})
		if aggErr != nil {
			s.logger.Error("Biz Goroutine: Failed to notify Aggregation (Offline)", zap.String("session_id", sess.SessionId), zap.Error(aggErr))
			gSpan.RecordError(aggErr)
			gSpan.SetStatus(otelCodes.Error, "Failed to call aggregation service")
		} else {
			s.logger.Info("Biz Goroutine: Notified Aggregation (Offline)", zap.String("session_id", sess.SessionId))
			gSpan.SetStatus(otelCodes.Ok, "Aggregation service notified")
		}
	}(ctx, protoSession)

	s.logger.Info("Biz: Session ended successfully", zap.String("session_id", protoSession.SessionId))
	span.SetStatus(otelCodes.Ok, "Session ended")
	return protoSession, nil
}

// GetLiveSessionByRoomID contains the business logic for getting the live session by room ID.
func (s *Service) GetLiveSessionByRoomID(ctx context.Context, roomID string) (*sessionv1.Session, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "GetLiveSessionByRoomID") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.room_id", roomID))

	s.logger.Debug("Biz: Getting live session by room ID", zap.String("room_id", roomID))

	cacheKey := getLiveSessionByRoomCacheKey(roomID)
	negativeCacheKey := getNegativeCacheKey(cacheKey)

	// 1. Try cache first (check for negative cache first)
	negExists, err := s.redisClient.Exists(ctx, negativeCacheKey).Result()
	if err == nil && negExists > 0 {
		s.logger.Debug("Biz: Negative cache hit for live session by room", zap.String("key", negativeCacheKey))
		span.SetAttributes(attribute.Bool("cache.hit", true), attribute.Bool("cache.negative", true))
		span.SetStatus(otelCodes.Ok, "Negative cache hit")
		return nil, fmt.Errorf("%w: room %s (cached not found)", ErrSessionNotFound, roomID)
	} else if err != nil && !errors.Is(err, redis.Nil) {
		s.logger.Error("Biz: Failed to check negative cache for live session by room", zap.Error(err), zap.String("key", negativeCacheKey))
		span.RecordError(err)
		// Proceed to check positive cache / DB
	}

	// Check positive cache
	cachedSessionID, err := s.redisClient.Get(ctx, cacheKey).Result()
	if err == nil && cachedSessionID != "" {
		s.logger.Debug("Biz: Cache hit for live session ID by room", zap.String("key", cacheKey), zap.String("cached_session_id", cachedSessionID))
		span.SetAttributes(attribute.Bool("cache.hit", true), attribute.Bool("cache.negative", false))
		// Now get the full session details (which should also be cached)
		protoSession, getErr := s.GetSession(ctx, cachedSessionID) // Use renamed GetSession
		if getErr != nil {
			s.logger.Warn("Biz: Live session ID found in room cache, but failed to get session details", zap.Error(getErr), zap.String("session_id", cachedSessionID), zap.String("room_id", roomID))
			span.RecordError(getErr)
			// Fallback to DB query for the room ID
		} else {
			span.SetStatus(otelCodes.Ok, "Cache hit (room -> session)")
			return protoSession, nil
		}
	} else if !errors.Is(err, redis.Nil) {
		s.logger.Error("Biz: Failed to read live session ID cache by room", zap.Error(err), zap.String("key", cacheKey))
		span.RecordError(err)
		// Proceed to DB query
	} else {
		s.logger.Debug("Biz: Cache miss for live session ID by room", zap.String("key", cacheKey))
		span.SetAttributes(attribute.Bool("cache.hit", false))
	}

	// 2. Read from DB if cache miss or error
	dbSession, err := s.repo.GetLiveSessionByRoomID(ctx, roomID)
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Debug("Biz: No live session found for room in DB", zap.String("room_id", roomID))
			span.SetStatus(otelCodes.Ok, "No live session found in DB")
			// --- Cache the "not found" result ---
			errCache := s.redisClient.Set(ctx, negativeCacheKey, "1", 10*time.Second).Err() // Significantly shorter TTL for negative cache (10 seconds)
			if errCache != nil {
				s.logger.Error("Biz: Failed to set negative cache for live session by room", zap.Error(errCache), zap.String("key", negativeCacheKey))
			} else {
				s.logger.Debug("Biz: Set negative cache for live session by room", zap.String("key", negativeCacheKey))
			}
			return nil, fmt.Errorf("%w: room %s", ErrSessionNotFound, roomID) // <-- 使用业务错误
		}
		s.logger.Error("Biz: Failed to get live session by room ID from DB", zap.Error(err), zap.String("room_id", roomID))
		span.SetStatus(otelCodes.Error, "DB error getting live session")
		return nil, fmt.Errorf("查询直播中场次失败: %w", err)
	}

	// 3. Cache backfill
	protoSession := mapDBSessionToProto(dbSession)

	// --- Explicitly delete negative cache AGAIN now that we confirmed a live session exists in DB ---
	// This acts as a safeguard against race conditions where negative cache wasn't deleted properly before.
	s.redisClient.Del(ctx, negativeCacheKey) // Fire-and-forget deletion is acceptable here

	// --- Ensure negative cache is removed BEFORE setting positive cache ---
	delErr := s.redisClient.Del(ctx, negativeCacheKey).Err() // Keep the original DEL attempt as well
	if delErr != nil && !errors.Is(delErr, redis.Nil) {
		s.logger.Warn("Biz: Failed to delete negative cache before setting positive cache on DB backfill", zap.Error(delErr), zap.String("key", negativeCacheKey))
		// Continue anyway
	} else if delErr == nil {
		s.logger.Debug("Biz: Deleted negative cache before setting positive cache on DB backfill (if existed)", zap.String("key", negativeCacheKey))
	}

	// Cache the live session ID by room ID (long TTL as it's live)
	errCache := s.redisClient.Set(ctx, cacheKey, protoSession.SessionId, 24*time.Hour).Err()
	if errCache != nil {
		s.logger.Error("Biz: Failed to cache live session ID by room on DB backfill", zap.Error(errCache), zap.String("key", cacheKey))
	} else {
		s.logger.Debug("Biz: Cached live session ID by room on DB backfill", zap.String("key", cacheKey), zap.String("session_id", protoSession.SessionId))
	}

	// Also update the main session cache (handled by updateSessionCache)
	s.updateSessionCache(ctx, protoSession) // Update main session cache

	s.logger.Debug("Biz: Found live session in DB", zap.String("session_id", protoSession.SessionId), zap.String("room_id", roomID))
	span.SetStatus(otelCodes.Ok, "Live session found in DB")
	return protoSession, nil
}

// UpdateSessionAggregates contains the business logic for updating session aggregates.
func (s *Service) UpdateSessionAggregates(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.Session, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "UpdateSessionAggregates") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.session_id", req.GetSessionId()))

	s.logger.Debug("Biz: Updating session aggregates", zap.String("session_id", req.GetSessionId()))

	params := db.UpdateSessionAggregatesParams{
		SessionID:       req.GetSessionId(),
		TotalEvents:     req.GetTotalEvents(),
		TotalDanmaku:    req.GetTotalDanmaku(),
		TotalGiftsValue: req.GetTotalGiftsValue(),
		TotalLikes:      req.GetTotalLikes(),
		TotalWatched:    req.GetTotalWatched(),
	}
	updatedDBSession, err := s.repo.UpdateSessionAggregates(ctx, params)
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("Biz: Attempted to update aggregates for non-existent session", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Error, "Session not found for aggregate update")
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, req.GetSessionId()) // <-- 使用业务错误
		}
		s.logger.Error("Biz: Failed to update session aggregates", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.SetStatus(otelCodes.Error, "DB error updating aggregates")
		return nil, fmt.Errorf("更新场次聚合数据失败: %w", err)
	}

	protoSession := mapDBSessionToProto(updatedDBSession)
	s.updateSessionCache(ctx, protoSession)

	s.logger.Info("Biz: Session aggregates updated", zap.String("session_id", protoSession.SessionId))
	span.SetStatus(otelCodes.Ok, "Aggregates updated")
	return protoSession, nil
}

// DeleteSession contains the business logic for deleting a session.
func (s *Service) DeleteSession(ctx context.Context, sessionID string) error {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "DeleteSession") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.session_id", sessionID))

	s.logger.Info("Biz: Deleting session", zap.String("session_id", sessionID))

	err := s.repo.DeleteSession(ctx, sessionID)
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("Biz: Session to delete not found, treated as success", zap.String("session_id", sessionID))
			span.SetStatus(otelCodes.Ok, "Session already deleted or never existed")
			s.deleteSessionCache(context.Background(), sessionID)
			return nil // Idempotent success
		}
		s.logger.Error("Biz: Failed to delete session from DB", zap.Error(err), zap.String("session_id", sessionID))
		span.SetStatus(otelCodes.Error, "DB error deleting session")
		return fmt.Errorf("删除场次数据库操作失败: %w", err)
	}

	// Get session details to find room ID before deleting caches
	// Use context.Background() as the original context might be cancelled
	sessionDetails, getErr := s.GetSession(context.Background(), sessionID) // Use renamed GetSession

	s.deleteSessionCache(context.Background(), sessionID) // Delete main session cache

	// Delete live session by room cache if we found the room ID
	// AND publish session ended event
	if getErr == nil && sessionDetails != nil && sessionDetails.RoomId != "" {
		// --- 新增: 发布 Session 结束事件到 NATS (通知 Adapter 等) ---
		// Use current time as end time since the session is being deleted now
		go s.publishSessionStatusUpdate(ctx, sessionID, sessionDetails.RoomId, false, time.Now())
		// --- NATS 发布结束 ---

		liveCacheKey := getLiveSessionByRoomCacheKey(sessionDetails.RoomId)
		errCache := s.redisClient.Del(context.Background(), liveCacheKey).Err()
		if errCache != nil && !errors.Is(errCache, redis.Nil) {
			s.logger.Error("Biz: Failed to delete live session ID cache by room on delete", zap.Error(errCache), zap.String("key", liveCacheKey))
		} else {
			s.logger.Debug("Biz: Deleted live session ID cache by room on delete (if existed)", zap.String("key", liveCacheKey))
		}
	} else if getErr != nil && !errors.Is(getErr, ErrSessionNotFound) { // Log error only if it's not 'not found'
		s.logger.Warn("Biz: Could not get session details to delete live cache on delete", zap.Error(getErr), zap.String("session_id", sessionID))
	}

	s.logger.Info("Biz: Session deleted successfully", zap.String("session_id", sessionID))
	span.SetStatus(otelCodes.Ok, "Session deleted")
	return nil
}

// HealthCheck performs the health checks for the service dependencies.
// This method is called by the gRPC handler's HealthCheck.
func (s *Service) HealthCheck(ctx context.Context) (string, error) {
	s.logger.Debug("Biz: Performing health check")
	healthStatus := "SERVING"
	var firstError error

	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("Biz HealthCheck: DB failed", zap.Error(err))
		healthStatus = "NOT_SERVING"
		firstError = fmt.Errorf("db ping failed: %w", err)
	}

	checkCtxRds, cancelRds := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRds()
	if _, err := s.redisClient.Ping(checkCtxRds).Result(); err != nil {
		s.logger.Error("Biz HealthCheck: Redis failed", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = fmt.Errorf("redis ping failed: %w", err)
		}
	}

	checkCtxRoom, cancelRoom := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRoom()
	if _, err := s.roomClient.HealthCheck(checkCtxRoom, &emptypb.Empty{}); err != nil {
		s.logger.Error("Biz HealthCheck: RoomService failed", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = fmt.Errorf("room service healthcheck failed: %w", err)
		}
	}

	if !s.natsConn.IsConnected() {
		s.logger.Error("Biz HealthCheck: NATS not connected")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("nats not connected")
		}
	}

	checkCtxAgg, cancelAgg := context.WithTimeout(ctx, 1*time.Second)
	defer cancelAgg()
	if _, err := s.aggregationClient.HealthCheck(checkCtxAgg, &emptypb.Empty{}); err != nil {
		s.logger.Error("Biz HealthCheck: AggregationService failed", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = fmt.Errorf("aggregation service healthcheck failed: %w", err)
		}
	}

	s.logger.Debug("Biz: Health check completed", zap.String("status", healthStatus), zap.Error(firstError))
	return healthStatus, firstError
}

// SendChatMessage contains the business logic for sending a chat message.
func (s *Service) SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (string, error) {
	tracer := otel.Tracer("session-service-biz")
	chatCtx, span := tracer.Start(ctx, "SendChatMessage") // Renamed span
	defer span.End()
	span.SetAttributes(
		attribute.String("biz.session_id", req.GetSessionId()),
		attribute.String("biz.user_id", req.GetUserId()),
	)

	s.logger.Debug("Biz: Sending chat message", zap.String("session_id", req.GetSessionId()), zap.String("user_id", req.GetUserId()))

	// 1. Validate session state
	session, err := s.repo.GetSessionByID(chatCtx, req.GetSessionId())
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("Biz: Session not found for chat message", zap.String("session_id", req.GetSessionId()))
			span.SetStatus(otelCodes.Error, "Session not found")
			return "", fmt.Errorf("%w: 场次 %s", ErrSessionNotFound, req.GetSessionId()) // <-- 使用业务错误
		}
		s.logger.Error("Biz: Failed to validate session for chat message", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		span.SetStatus(otelCodes.Error, "DB error getting session")
		return "", fmt.Errorf("验证场次失败: %w", err)
	}
	if session.Status != "live" {
		s.logger.Warn("Biz: Cannot send chat message to non-live session", zap.String("session_id", req.GetSessionId()), zap.String("status", session.Status))
		span.SetStatus(otelCodes.Error, "Session not live")
		return "", fmt.Errorf("%w: 场次 %s 当前状态 (%s)", ErrSessionNotLive, req.GetSessionId(), session.Status) // <-- 使用业务错误
	}

	// 2. Create and publish event
	msgID := uuid.NewString()
	ce := &eventv1.ChatMessage{
		MessageId: msgID,
		SessionId: req.GetSessionId(),
		UserId:    req.GetUserId(),
		Content:   req.GetContent(),
		Timestamp: timestamppb.Now(),
		RoomId:    session.RoomID,
	}
	payload, e := proto.Marshal(ce)
	if e != nil {
		s.logger.Error("Biz: Failed to marshal ChatMessage event", zap.Error(e), zap.String("session_id", req.GetSessionId()))
		span.RecordError(e)
		span.SetStatus(otelCodes.Error, "Failed to marshal event")
		return "", fmt.Errorf("序列化消息失败: %w", e)
	}
	subject := "events.raw.chat.message"

	traceHeadersMap := observability.InjectNATSHeaders(chatCtx)
	natsHeader := make(nats.Header)
	for k, v := range traceHeadersMap {
		natsHeader[k] = []string{v}
	}
	msg := &nats.Msg{Subject: subject, Data: payload, Header: natsHeader}

	if e := s.natsConn.PublishMsg(msg); e != nil {
		s.logger.Error("Biz: Failed to publish ChatMessage event to NATS", zap.Error(e), zap.String("session_id", req.GetSessionId()))
		span.RecordError(e)
		span.SetStatus(otelCodes.Error, "Failed to publish event")
		return "", fmt.Errorf("发布消息失败: %w", e)
	}

	s.logger.Info("Biz: ChatMessage event published", zap.String("subject", subject), zap.String("session_id", req.GetSessionId()), zap.String("message_id", msgID))
	span.SetStatus(otelCodes.Ok, "Chat message event published")
	return msgID, nil
}

// CheckSessionActive contains the business logic to check if a session is active.
func (s *Service) CheckSessionActive(ctx context.Context, roomID string) (bool, string, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "CheckSessionActive") // Renamed span
	defer span.End()
	span.SetAttributes(attribute.String("biz.room_id", roomID))

	s.logger.Debug("Biz: Checking if session is active", zap.String("room_id", roomID))

	sessionID, err := s.repo.GetLiveSessionIDByRoomID(ctx, roomID)
	isActive := false
	if err == nil && sessionID != "" {
		isActive = true
		span.SetStatus(otelCodes.Ok, "Active session found")
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Error("Biz: Failed to check active session", zap.Error(err), zap.String("room_id", roomID))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "DB error checking active session")
		return false, "", fmt.Errorf("查询活跃会话失败: %w", err)
	} else {
		span.SetStatus(otelCodes.Ok, "No active session found")
	}

	s.logger.Debug("Biz: CheckSessionActive result", zap.String("room_id", roomID), zap.Bool("is_active", isActive), zap.String("session_id", sessionID))
	return isActive, sessionID, nil
}

// ListSessions contains the business logic for listing sessions.
func (s *Service) ListSessions(ctx context.Context, req *sessionv1.ListSessionsRequest) ([]*sessionv1.Session, int32, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "ListSessions") // Renamed span
	defer span.End()
	span.SetAttributes(
		attribute.Int("page", int(req.GetPage())),
		attribute.Int("page_size", int(req.GetPageSize())),
		attribute.String("filter.status", req.GetStatus()),
		attribute.String("filter.room_id", req.GetRoomId()),
		attribute.String("filter.owner_user_id", req.GetOwnerUserId()),
	)

	s.logger.Debug("Biz: Listing sessions",
		zap.Int32("page", req.GetPage()),
		zap.Int32("page_size", req.GetPageSize()),
		zap.String("status", req.GetStatus()),
		zap.String("room_id", req.GetRoomId()),
		zap.String("owner_user_id", req.GetOwnerUserId()))

	offset := (req.GetPage() - 1) * req.GetPageSize()

	params := db.ListSessionsParams{
		Status:      req.GetStatus(),
		RoomID:      req.GetRoomId(),
		OwnerUserID: req.GetOwnerUserId(),
		Limit:       req.GetPageSize(),
		Offset:      offset,
	}
	countParams := db.GetSessionsCountParams{
		Status:      req.GetStatus(),
		RoomID:      req.GetRoomId(),
		OwnerUserID: req.GetOwnerUserId(),
	}

	total, err := s.repo.GetSessionsCount(ctx, countParams)
	if err != nil {
		s.logger.Error("Biz: Failed to get sessions count", zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "DB error getting count")
		return nil, 0, fmt.Errorf("获取场次总数失败: %w", err)
	}

	sessions, err := s.repo.ListSessions(ctx, params)
	if err != nil {
		s.logger.Error("Biz: Failed to list sessions", zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "DB error listing sessions")
		return nil, 0, fmt.Errorf("查询场次列表失败: %w", err)
	}

	protoSessions := make([]*sessionv1.Session, len(sessions))
	for i, session := range sessions {
		protoSessions[i] = mapDBSessionToProto(session)
	}

	s.logger.Debug("Biz: ListSessions successful", zap.Int("count", len(protoSessions)), zap.Int64("total", total))
	span.SetAttributes(attribute.Int("result.count", len(protoSessions)), attribute.Int64("result.total", total))
	span.SetStatus(otelCodes.Ok, "Sessions listed")
	return protoSessions, int32(total), nil
}

// GetSessionDetails reuses GetSession logic.
// Note: This method is now redundant as GetSession is the primary method.
// Consider removing this method in a future refactor if not used elsewhere.
func (s *Service) GetSessionDetails(ctx context.Context, sessionID string) (*sessionv1.Session, error) {
	return s.GetSession(ctx, sessionID) // Use renamed GetSession
}

// ListLiveSessions implements the logic for listing live session IDs.
// Note: This method's gRPC implementation should be in the handler.
// This remains as a business logic method.
func (s *Service) ListLiveSessions(ctx context.Context, req *sessionv1.ListLiveSessionsRequest) (*sessionv1.ListLiveSessionsResponse, error) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "ListLiveSessions")
	defer span.End()

	s.logger.Debug("Biz: Listing live session IDs")

	sessionIDs, err := s.repo.ListLiveSessionIDs(ctx)
	if err != nil {
		s.logger.Error("Biz: Failed to list live session IDs from DB", zap.Error(err))
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "DB error listing live sessions")
		return nil, status.Errorf(codes.Internal, "查询直播中场次 ID 失败: %v", err)
	}

	s.logger.Debug("Biz: ListLiveSessions successful", zap.Int("count", len(sessionIDs)))
	span.SetAttributes(attribute.Int("result.count", len(sessionIDs)))
	span.SetStatus(otelCodes.Ok, "Live sessions listed")

	return &sessionv1.ListLiveSessionsResponse{
		SessionIds: sessionIDs,
	}, nil
}

// --- NATS Event Handling and other internal logic remain ---

// HandlePlatformStreamStatusEvent handles incoming stream status events (called by subscriber).
func (s *Service) HandlePlatformStreamStatusEvent(ctx context.Context, event PlatformStreamStatusEventJSON) {
	tracer := otel.Tracer("session-service-biz")
	ctx, span := tracer.Start(ctx, "HandlePlatformStreamStatusEvent")
	defer span.End()
	span.SetAttributes(
		attribute.String("event.room_id", event.RoomID),
		attribute.Bool("event.is_live", event.IsLive),
		attribute.String("event.room_title", event.RoomTitle),
		attribute.String("event.streamer_name", event.StreamerName),
		attribute.String("trigger", "nats"),
	)

	s.logger.Info("Biz: Handling platform stream status event",
		zap.String("room_id", event.RoomID),
		zap.Bool("is_live", event.IsLive),
		zap.Int64("timestamp_ms", event.Timestamp))

	if event.RoomID == "" {
		s.logger.Warn("Biz: Platform stream status event missing room_id, ignoring")
		span.SetStatus(otelCodes.Error, "Missing room_id in event")
		return
	}

	if event.IsLive {
		s.logger.Info("Biz: Processing LIVE event", zap.String("room_id", event.RoomID))
		liveSession, err := s.repo.GetLiveSessionByRoomID(ctx, event.RoomID)

		// --- 修改: 如果存在活跃会话，直接使用，不创建新的 ---
		if err == nil && liveSession.SessionID != "" {
			s.logger.Info("业务: 收到 LIVE 事件，但已存在活跃会话。重用现有会话。", // <-- 翻译日志
				zap.String("room_id", event.RoomID),
				zap.String("existing_session_id", liveSession.SessionID),
			)
			span.SetAttributes(
				attribute.String("session.id", liveSession.SessionID),
				attribute.String("action", "reuse_existing_session"), // 保持英文标签
			)
			// 可选：发布更新事件以确认活跃的会话 ID
			// 如果可用，使用现有会话的开始时间，否则使用当前时间
			startTime := time.Now()
			if liveSession.StartTime.Valid {
				startTime = liveSession.StartTime.Time
			}
			go s.publishSessionStatusUpdate(ctx, liveSession.SessionID, liveSession.RoomID, true, startTime)
			span.SetStatus(otelCodes.Ok, "重用现有直播会话") // <-- 翻译状态描述
			return                                   // 不继续创建新会话
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// 处理检查现有会话时的错误（排除 ErrNoRows）
			s.logger.Error("业务: 检查现有直播会话失败 (LIVE 事件)", zap.Error(err), zap.String("room_id", event.RoomID)) // <-- 翻译日志
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to check existing session")
			return
		}

		ownerUserID := ""
		roomResp, err := s.roomClient.GetRoom(ctx, &roomv1.GetRoomRequest{RoomId: event.RoomID})
		if err != nil {
			s.logger.Error("Biz: Failed to get room info (LIVE event), using empty OwnerUserID", zap.String("room_id", event.RoomID), zap.Error(err))
			span.RecordError(err)
		} else if roomResp.GetOwner() == nil {
			s.logger.Warn("Biz: Room info retrieved but owner is nil (LIVE event)", zap.String("room_id", event.RoomID))
		} else {
			ownerUserID = roomResp.GetOwner().GetUserId()
		}
		span.SetAttributes(attribute.String("biz.owner_user_id", ownerUserID))

		sessionID := uuid.NewString()
		startTime := time.Now()
		params := db.CreateSessionParams{
			SessionID: sessionID, RoomID: event.RoomID, OwnerUserID: ownerUserID,
			StartTime: pgtype.Timestamptz{Time: startTime, Valid: true}, Status: "live",
			SessionTitle: pgtype.Text{String: event.RoomTitle, Valid: event.RoomTitle != ""},
			AnchorName:   pgtype.Text{String: event.StreamerName, Valid: event.StreamerName != ""},
			RoomTitle:    pgtype.Text{String: event.RoomTitle, Valid: event.RoomTitle != ""},
		}
		createdDBSession, err := s.repo.CreateSession(ctx, params)
		if err != nil {
			s.logger.Error("Biz: Failed to create session in DB (LIVE event)", zap.Error(err), zap.String("room_id", event.RoomID))
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to create session in DB")
			return
		}
		protoSession := mapDBSessionToProto(createdDBSession)
		span.SetAttributes(attribute.String("session.id", protoSession.SessionId))
		s.logger.Info("Biz: Session created via NATS event", zap.String("session_id", protoSession.SessionId), zap.String("room_id", event.RoomID))

		// --- 新增: 发布 Session 创建事件到 NATS (通知 Adapter 等) ---
		go s.publishSessionStatusUpdate(ctx, protoSession.SessionId, protoSession.RoomId, true, protoSession.StartTime.AsTime())
		// --- NATS 发布结束 ---

		go func(parentCtx context.Context, sess *sessionv1.Session) {
			aggCtx, aggCancel := context.WithTimeout(parentCtx, 5*time.Second)
			defer aggCancel()
			aggTracer := otel.Tracer("session-service-goroutine")
			gCtx, gSpan := aggTracer.Start(aggCtx, "notifyAggregationServiceLiveNATS")
			defer gSpan.End()
			gSpan.SetAttributes(
				attribute.String("session.id", sess.SessionId), attribute.Bool("event.is_live", true), attribute.String("trigger", "nats"),
			)
			_, aggErr := s.aggregationClient.HandleStreamStatus(gCtx, &aggregationv1.StreamStatus{
				SessionId: sess.SessionId, RoomId: sess.RoomId, IsLive: true, Timestamp: sess.StartTime,
			})
			if aggErr != nil {
				s.logger.Error("Biz Goroutine: Failed to notify Aggregation (Live/NATS)", zap.String("session_id", sess.SessionId), zap.Error(aggErr))
				gSpan.RecordError(aggErr)
				gSpan.SetStatus(otelCodes.Error, "Failed to call aggregation service")
			} else {
				s.logger.Info("Biz Goroutine: Notified Aggregation (Live/NATS)", zap.String("session_id", sess.SessionId))
				gSpan.SetStatus(otelCodes.Ok, "Aggregation service notified")
			}
		}(ctx, protoSession)

		// Update caches: main session cache, live session by room cache, remove negative cache
		s.updateSessionCache(ctx, protoSession) // Update main session cache first
		liveCacheKey := getLiveSessionByRoomCacheKey(protoSession.RoomId)
		negativeCacheKey := getNegativeCacheKey(liveCacheKey)

		// --- Ensure negative cache is removed BEFORE setting positive cache ---
		delErr := s.redisClient.Del(ctx, negativeCacheKey).Err()
		if delErr != nil && !errors.Is(delErr, redis.Nil) {
			s.logger.Warn("Biz: Failed to delete negative cache before setting positive cache on NATS create", zap.Error(delErr), zap.String("key", negativeCacheKey))
			// Continue anyway
		} else if delErr == nil {
			s.logger.Debug("Biz: Deleted negative cache before setting positive cache on NATS create (if existed)", zap.String("key", negativeCacheKey))
		}

		// Now set the positive cache
		errCache := s.redisClient.Set(ctx, liveCacheKey, protoSession.SessionId, 24*time.Hour).Err()
		if errCache != nil {
			s.logger.Error("Biz: Failed to cache live session ID by room on NATS create", zap.Error(errCache), zap.String("key", liveCacheKey))
		} else {
			s.logger.Debug("Biz: Cached live session ID by room on NATS create", zap.String("key", liveCacheKey), zap.String("session_id", protoSession.SessionId))
		}
		span.SetStatus(otelCodes.Ok, "Live event processed")

	} else {
		s.logger.Info("Biz: Processing OFFLINE event", zap.String("room_id", event.RoomID))
		liveSession, err := s.repo.GetLiveSessionByRoomID(ctx, event.RoomID)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("Biz: Received OFFLINE event, but no live session found, ignoring", zap.String("room_id", event.RoomID))
			span.SetStatus(otelCodes.Ok, "No live session found for offline event")
			return
		} else if err != nil {
			s.logger.Error("Biz: Failed to get live session (OFFLINE event)", zap.Error(err), zap.String("room_id", event.RoomID))
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to get live session")
			return
		}
		span.SetAttributes(attribute.String("session.id", liveSession.SessionID))
		s.logger.Info("Biz: Found live session to end via NATS event", zap.String("session_id", liveSession.SessionID), zap.String("room_id", event.RoomID))

		endTime := time.Now()
		params := db.EndSessionParams{
			SessionID: liveSession.SessionID, EndTime: pgtype.Timestamptz{Time: endTime, Valid: true},
		}
		endedDBSession, err := s.repo.EndSession(ctx, params)
		if err != nil {
			s.logger.Error("Biz: Failed to end session in DB (OFFLINE event)", zap.Error(err), zap.String("session_id", liveSession.SessionID))
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to end session in DB")
			return
		}
		protoSession := mapDBSessionToProto(endedDBSession)
		s.logger.Info("Biz: Session ended via NATS event", zap.String("session_id", protoSession.SessionId), zap.String("room_id", event.RoomID))

		// --- 新增: 发布 Session 结束事件到 NATS (通知 Adapter 等) ---
		go s.publishSessionStatusUpdate(ctx, protoSession.SessionId, protoSession.RoomId, false, protoSession.EndTime.AsTime())
		// --- NATS 发布结束 ---

		go func(parentCtx context.Context, sess *sessionv1.Session) {
			aggCtx, aggCancel := context.WithTimeout(parentCtx, 5*time.Second)
			defer aggCancel()
			aggTracer := otel.Tracer("session-service-goroutine")
			gCtx, gSpan := aggTracer.Start(aggCtx, "notifyAggregationServiceOfflineNATS")
			defer gSpan.End()
			gSpan.SetAttributes(
				attribute.String("session.id", sess.SessionId), attribute.Bool("event.is_live", false), attribute.String("trigger", "nats"),
			)
			_, aggErr := s.aggregationClient.HandleStreamStatus(gCtx, &aggregationv1.StreamStatus{
				SessionId: sess.SessionId, RoomId: sess.RoomId, IsLive: false, Timestamp: sess.EndTime,
			})
			if aggErr != nil {
				s.logger.Error("Biz Goroutine: Failed to notify Aggregation (Offline/NATS)", zap.String("session_id", sess.SessionId), zap.Error(aggErr))
				gSpan.RecordError(aggErr)
				gSpan.SetStatus(otelCodes.Error, "Failed to call aggregation service")
			} else {
				s.logger.Info("Biz Goroutine: Notified Aggregation (Offline/NATS)", zap.String("session_id", sess.SessionId))
				gSpan.SetStatus(otelCodes.Ok, "Aggregation service notified")
			}
		}(ctx, protoSession)

		// Update main session cache and remove live session by room cache
		s.updateSessionCache(ctx, protoSession)
		liveCacheKey := getLiveSessionByRoomCacheKey(protoSession.RoomId)
		errCache := s.redisClient.Del(ctx, liveCacheKey).Err()
		if errCache != nil && !errors.Is(errCache, redis.Nil) {
			s.logger.Error("Biz: Failed to delete live session ID cache by room on NATS end", zap.Error(errCache), zap.String("key", liveCacheKey))
		} else {
			s.logger.Debug("Biz: Deleted live session ID cache by room on NATS end (if existed)", zap.String("key", liveCacheKey))
		}
		span.SetStatus(otelCodes.Ok, "Offline event processed")
	}
}

// SignalUserPresence handles user presence signals.
func (s *Service) SignalUserPresence(ctx context.Context, userID, sessionID string, entered bool) error {
	tracer := otel.Tracer("session-service-biz")
	sigCtx, span := tracer.Start(ctx, "SignalUserPresenceBiz")
	defer span.End()
	span.SetAttributes(
		attribute.String("biz.user_id", userID),
		attribute.String("biz.session_id", sessionID),
		attribute.Bool("biz.entered", entered),
	)

	s.logger.Info("Biz: Handling user presence signal", zap.String("session_id", sessionID), zap.String("user_id", userID), zap.Bool("entered", entered))

	// 1. Validate session state
	session, err := s.repo.GetSessionByID(sigCtx, sessionID)
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("Biz: Session not found for presence signal", zap.String("session_id", sessionID), zap.String("user_id", userID))
			span.SetStatus(otelCodes.Error, "Session not found")
			return fmt.Errorf("%w: 场次 %s", ErrSessionNotFound, sessionID) // <-- 使用业务错误
		}
		s.logger.Error("Biz: Failed to validate session for presence signal", zap.Error(err), zap.String("session_id", sessionID))
		span.SetStatus(otelCodes.Error, "DB error getting session")
		return fmt.Errorf("验证场次失败: %w", err)
	}
	if session.Status != "live" {
		s.logger.Warn("Biz: Cannot signal presence for non-live session", zap.String("session_id", sessionID), zap.String("status", session.Status))
		span.SetStatus(otelCodes.Error, "Session not live")
		return fmt.Errorf("%w: 场次 %s 当前状态 (%s)", ErrSessionNotLive, sessionID, session.Status) // <-- 使用业务错误
	}

	// 2. Create and publish event
	presenceEvent := &eventv1.UserPresence{
		SessionId: sessionID, UserId: userID, Entered: entered, Timestamp: timestamppb.Now(), RoomId: session.RoomID,
	}
	payload, e := proto.Marshal(presenceEvent)
	if e != nil {
		s.logger.Error("Biz: Failed to marshal UserPresence event", zap.Error(e), zap.String("session_id", sessionID))
		span.RecordError(e)
		span.SetStatus(otelCodes.Error, "Failed to marshal event")
		return fmt.Errorf("序列化事件失败: %w", e)
	}
	subject := "events.raw.user.presence"

	traceHeadersMap := observability.InjectNATSHeaders(sigCtx)
	natsHeader := make(nats.Header)
	for k, v := range traceHeadersMap {
		natsHeader[k] = []string{v}
	}
	msg := &nats.Msg{Subject: subject, Data: payload, Header: natsHeader}

	if e := s.natsConn.PublishMsg(msg); e != nil {
		s.logger.Error("Biz: Failed to publish UserPresence event to NATS", zap.Error(e), zap.String("session_id", sessionID))
		span.RecordError(e)
		span.SetStatus(otelCodes.Error, "Failed to publish event")
		return fmt.Errorf("发布事件失败: %w", e)
	}

	s.logger.Info("Biz: UserPresence event published", zap.String("subject", subject), zap.String("session_id", sessionID), zap.String("user_id", userID))
	span.SetStatus(otelCodes.Ok, "User presence event published")
	return nil
}

// --- NATS Subscriber Management ---

func (s *Service) StartSubscribers() {
	s.logger.Info("Biz: Starting NATS subscribers (if any)...")
}

func (s *Service) StopSubscribers() {
	s.logger.Info("Biz: Stopping NATS subscribers (if any)...")
	for _, sub := range s.natsSubs {
		if err := sub.Unsubscribe(); err != nil {
			s.logger.Error("Biz: Failed to unsubscribe NATS", zap.String("subject", sub.Subject), zap.Error(err))
		} else {
			s.logger.Info("Biz: Unsubscribed NATS", zap.String("subject", sub.Subject))
		}
	}
	s.natsSubs = nil
}

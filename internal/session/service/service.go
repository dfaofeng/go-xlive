package service

import (
	"context"
	"encoding/json" // 用于缓存序列化
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"

	// !!! 替换模块路径 !!
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	roomv1 "go-xlive/gen/go/room/v1"
	sessionv1 "go-xlive/gen/go/session/v1"
	"go-xlive/internal/session/repository"
rnal/session/repository/db"

	"go.opentelemet
	db "go-xlive/internal/session/repository/db"
	"go.opentelemetry.io/otel"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionService 结构体
type SessionService struct {
	sessionv1.UnimplementedSessionServiceServer
	repo        repository.Repository
	logger      *zap.Logger
	roomClient  roomv1.RoomServiceClient
	redisClient *redis.Client // 保留 Redis 客户端实例
	natsConn    *nats.Conn
}

// NewSessionService 创建实例
func NewSessionService(logger *zap.Logger, repo repository.Repository, roomClient roomv1.RoomServiceClient, redisClient *redis.Client, natsConn *nats.Conn) *SessionService {
	if logger == nil {
		log.Fatal("SessionService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("SessionService 需要 Repository")
	}
	if roomClient == nil {
		log.Fatal("SessionService 需要 RoomServiceClient")
	}
	if redisClient == nil {
		log.Fatal("SessionService 需要 Redis Client")
	} // 保持检查
	if natsConn == nil {
		log.Fatal("SessionService 需要 NATS Connection")
	}
	return &SessionService{
		logger:      logger.Named("session_service"),
		repo:        repo,
		roomClient:  roomClient,
		redisClient: redisClient,
		natsConn:    natsConn,
	}
}

// --- Redis Key for Session Cache ---
func getSessionCacheKey(sessionID string) string {
	return fmt.Sprintf("cache:session:%s", sessionID)
}

// CreateSession 实现 (添加缓存写入和 SessionStarted 事件)
func (s *SessionService) CreateSession(ctx context.Context, req *sessionv1.CreateSessionRequest) (*sessionv1.CreateSessionResponse, error) {
	s.logger.Info("收到 CreateSession 请求", zap.String("room_id", req.GetRoomId()))
	if req.GetRoomId() == "" {
		s.logger.Warn("CreateSession 失败：房间 ID 不能为空")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}

	// 验证房间状态
	s.logger.Debug("正在验证房间状态", zap.String("room_id", req.GetRoomId()))
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	roomResp, err := s.roomClient.GetRoom(callCtx, &roomv1.GetRoomRequest{RoomId: req.GetRoomId()})
	if err != nil {
		s.logger.Error("验证房间 ID 失败", zap.String("room_id", req.GetRoomId()), zap.Error(err))
		st, _ := status.FromError(err)
		if st.Code() == codes.NotFound {
			return nil, status.Errorf(codes.InvalidArgument, "房间 ID %s 不存在", req.GetRoomId())
		}
		if st.Code() == codes.DeadlineExceeded {
			return nil, status.Error(codes.Unavailable, "验证房间超时")
		}
		return nil, status.Errorf(codes.Internal, "验证房间失败: %v", err)
	}
	if roomResp.GetRoom().GetStatus() != "active" {
		s.logger.Warn("无法创建场次：房间状态不是 active", zap.String("room_id", req.GetRoomId()), zap.String("status", roomResp.GetRoom().GetStatus()))
		return nil, status.Errorf(codes.FailedPrecondition, "房间 %s 当前状态 (%s) 不允许创建场次", req.GetRoomId(), roomResp.GetRoom().GetStatus())
	}
	s.logger.Info("房间验证成功", zap.String("room_id", req.GetRoomId()))

	sessionID := uuid.NewString()
	startTime := time.Now()
	params := db.CreateSessionParams{
		SessionID: sessionID, RoomID: req.GetRoomId(),
		StartTime: pgtype.Timestamptz{Time: startTime, Valid: true}, Status: "live",
	}
	createdDBSession, err := s.repo.CreateSession(ctx, params)
	if err != nil {
		s.logger.Error("在数据库中创建场次失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "创建场次失败: %v", err)
	}

	protoSession := &sessionv1.Session{
		SessionId: createdDBSession.SessionID, RoomId: createdDBSession.RoomID,
		StartTime: timestamppb.New(createdDBSession.StartTime.Time), Status: createdDBSession.Status,
		TotalEvents: 0, TotalDanmaku: 0, TotalGiftsValue: 0,
	}

	// --- 发布 SessionStarted 事件 (可选) ---
	// startedEvent := &eventv1.SessionStarted{ SessionId: protoSession.SessionId, RoomId: protoSession.RoomId, StartTime: protoSession.StartTime }
	// payload, err := proto.Marshal(startedEvent)
	// if err != nil { s.logger.Error("序列化 SessionStarted 事件失败", zap.Error(err)) } else {
	//     subject := fmt.Sprintf("events.session.%s.started", protoSession.SessionId)
	//     if err := s.natsConn.Publish(subject, payload); err != nil { s.logger.Error("发布 SessionStarted 事件到 NATS 失败", zap.Error(err)) } else { s.logger.Info("已发布 SessionStarted 事件到 NATS", zap.String("subject", subject)) }
	// }
	// --- NATS 事件结束 ---

	// --- 添加到 Redis 缓存 ---
	cacheKey := getSessionCacheKey(protoSession.SessionId)
	sessionJSON, err := json.Marshal(protoSession)
	if err != nil {
		s.logger.Error("序列化 Session 到 JSON 失败 (缓存)", zap.Error(err))
	} else {
		err = s.redisClient.Set(ctx, cacheKey, sessionJSON, 24*time.Hour).Err() // 24 小时过期
		if err != nil {
			s.logger.Error("写入 Session 到 Redis 缓存失败", zap.Error(err), zap.String("key", cacheKey))
		} else {
			s.logger.Info("Session 已写入 Redis 缓存", zap.String("key", cacheKey))
		}
	}
	// --- 缓存结束 ---

	s.logger.Info("场次创建成功", zap.String("session_id", protoSession.SessionId))
	return &sessionv1.CreateSessionResponse{Session: protoSession}, nil
}

// UpdateSessionAggregates 实现 (添加缓存更新)
func (s *SessionService) UpdateSessionAggregates(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.UpdateSessionAggregatesResponse, error) {
	s.logger.Info("收到 UpdateSessionAggregates 请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id")
	}

	params := db.UpdateSessionAggregatesParams{
		SessionID: req.GetSessionId(), TotalEvents: req.GetTotalEvents(),
		TotalDanmaku: req.GetTotalDanmaku(), TotalGiftsValue: req.GetTotalGiftsValue(),
	}
	updatedDBSession, err := s.repo.UpdateSessionAggregates(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("尝试更新未找到的场次的聚合数据", zap.String("sid", req.GetSessionId()))
			return nil, status.Errorf(codes.NotFound, "未找到要更新聚合数据的场次: %s", req.GetSessionId())
		}
		s.logger.Error("更新场次聚合数据失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		return nil, status.Errorf(codes.Internal, "更新聚合数据失败: %v", err)
	}

	protoSession := &sessionv1.Session{
		SessionId: updatedDBSession.SessionID, RoomId: updatedDBSession.RoomID,
		StartTime: timestamppb.New(updatedDBSession.StartTime.Time), Status: updatedDBSession.Status,
		TotalEvents: updatedDBSession.TotalEvents, TotalDanmaku: updatedDBSession.TotalDanmaku,
		TotalGiftsValue: updatedDBSession.TotalGiftsValue,
	}
	if updatedDBSession.EndTime.Valid {
		protoSession.EndTime = timestamppb.New(updatedDBSession.EndTime.Time)
	}

	// --- 更新 Redis 缓存 ---
	cacheKey := getSessionCacheKey(req.GetSessionId())
	sessionJSON, err := json.Marshal(protoSession)
	if err != nil {
		s.logger.Error("序列化更新后的 Session 到 JSON 失败 (缓存)", zap.Error(err))
	} else {
		ttl, err := s.redisClient.TTL(ctx, cacheKey).Result()
		if err != nil || ttl <= 0 {
			ttl = 24 * time.Hour
		} // 保持或设置默认 TTL
		err = s.redisClient.Set(ctx, cacheKey, sessionJSON, ttl).Err()
		if err != nil {
			s.logger.Error("更新 Redis 缓存中的 Session 失败", zap.Error(err), zap.String("key", cacheKey))
		} else {
			s.logger.Info("已更新 Redis 缓存中的 Session", zap.String("key", cacheKey))
		}
	}
	// --- 缓存结束 ---

	s.logger.Info("场次聚合数据更新成功", zap.String("session_id", req.GetSessionId()))
	return &sessionv1.UpdateSessionAggregatesResponse{UpdatedSession: protoSession}, nil
}

// GetSession 实现 (添加缓存读取)
func (s *SessionService) GetSession(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	s.logger.Info("收到 GetSession 请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "场次 ID 不能为空")
	}

	cacheKey := getSessionCacheKey(req.GetSessionId())
	// --- 尝试从 Redis 缓存读取 ---
	cachedData, err := s.redisClient.Get(ctx, cacheKey).Bytes()
	if err == nil { // 缓存命中
		var cachedSession sessionv1.Session
		if err := json.Unmarshal(cachedData, &cachedSession); err == nil {
			s.logger.Info("缓存命中: Session", zap.String("key", cacheKey))
			return &sessionv1.GetSessionResponse{Session: &cachedSession}, nil
		} else {
			s.logger.Error("解析 Redis 缓存 JSON 失败", zap.Error(err))
		}
	} else if !errors.Is(err, redis.Nil) {
		s.logger.Error("读取 Redis 缓存失败", zap.Error(err))
	} else {
		s.logger.Info("缓存未命中: Session", zap.String("key", cacheKey))
	}
	// --- 缓存逻辑结束 ---

	// --- 从数据库读取 ---
	dbSession, err := s.repo.GetSessionByID(ctx, req.GetSessionId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("场次未找到 (DB)", zap.String("session_id", req.GetSessionId()))
			return nil, status.Errorf(codes.NotFound, "未找到场次，ID: %s", req.GetSessionId())
		}
		s.logger.Error("从数据库获取场次失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		return nil, status.Errorf(codes.Internal, "获取场次信息失败: %v", err)
	}

	protoSession := &sessionv1.Session{ // 映射 (包含聚合字段)
		SessionId: dbSession.SessionID, RoomId: dbSession.RoomID,
		StartTime: timestamppb.New(dbSession.StartTime.Time), Status: dbSession.Status,
		TotalEvents: dbSession.TotalEvents, TotalDanmaku: dbSession.TotalDanmaku,
		TotalGiftsValue: dbSession.TotalGiftsValue,
	}
	if dbSession.EndTime.Valid {
		protoSession.EndTime = timestamppb.New(dbSession.EndTime.Time)
	}

	// --- 回填缓存 ---
	sessionJSON, err := json.Marshal(protoSession)
	if err != nil {
		s.logger.Error("序列化从 DB 读取的 Session 到 JSON 失败 (缓存)", zap.Error(err))
	} else {
		err = s.redisClient.Set(ctx, cacheKey, 24*time.Hour, 0).Err() // 使用 SetNX 更好? 避免覆盖新数据
		if err != nil {
			s.logger.Error("写入 Session 到 Redis 缓存失败 (回填)", zap.Error(err))
		} else {
			s.logger.Info("Session 已回填到 Redis 缓存", zap.String("key", cacheKey))
		}
	}
	// --- 缓存结束 ---

	s.logger.Info("场次信息检索成功 (来自 DB)", zap.String("session_id", protoSession.SessionId))
	return &sessionv1.GetSessionResponse{Session: protoSession}, nil
}

// EndSession 实现 (移除 Redis 清理, 使用 endedDBSession)
func (s *SessionService) EndSession(ctx context.Context, req *sessionv1.EndSessionRequest) (*sessionv1.EndSessionResponse, error) {
	s.logger.Info("收到 EndSession 请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		s.logger.Warn("...")
		return nil, status.Error(codes.InvalidArgument, "...")
	}
	endTime := time.Now()
	params := db.EndSessionParams{
		SessionID: req.GetSessionId(),
		EndTime:   pgtype.Timestamptz{Time: endTime, Valid: true},
	}

	endedDBSession, err := s.repo.EndSession(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_, getErr := s.repo.GetSessionByID(ctx, req.GetSessionId())
			if errors.Is(getErr, pgx.ErrNoRows) { /*...*/
				return nil, status.Errorf(codes.NotFound, "...")
			}
			/*...*/ return nil, status.Error(codes.FailedPrecondition, "...")
		}
		/*...*/ return nil, status.Errorf(codes.Internal, "...")
	}

	// --- 使用 endedDBSession 进行映射 ---
	protoSession := &sessionv1.Session{
		SessionId: endedDBSession.SessionID, RoomId: endedDBSession.RoomID,
		StartTime: timestamppb.New(endedDBSession.StartTime.Time), EndTime: timestamppb.New(endedDBSession.EndTime.Time),
		Status: endedDBSession.Status, TotalEvents: endedDBSession.TotalEvents,
		TotalDanmaku: endedDBSession.TotalDanmaku, TotalGiftsValue: endedDBSession.TotalGiftsValue,
	}
	// --- 映射结束 ---

	// --- 移除 Redis Key 删除逻辑 ---

	// --- 发布场次结束事件到 NATS ---
	sessionEvent := &eventv1.SessionEnded{
		SessionId: protoSession.SessionId, RoomId: protoSession.RoomId, EndTime: protoSession.EndTime,
	}
	payload, err := proto.Marshal(sessionEvent)
	if err != nil {
		s.logger.Error("...", zap.Error(err))
	} else {
		subject := fmt.Sprintf("events.session.%s.ended", protoSession.SessionId)
		if err := s.natsConn.Publish(subject, payload); err != nil {
			s.logger.Error("...", zap.Error(err))
		} else {
			s.logger.Info("...", zap.String("subject", subject))
		}
	}

	s.logger.Info("场次结束成功", zap.String("session_id", protoSession.SessionId))
	return &sessionv1.EndSessionResponse{Session: protoSession}, nil
}

// HealthCheck 实现 (添加了 DB Ping 检查)
func (s *SessionService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*sessionv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (SessionService)")
	healthStatus := "SERVING"
	var firstError error
	// 检查 DB
	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("DB健康检查失败", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}
	// 检查 Redis
	checkCtxRds, cancelRds := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRds()
	if _, err := s.redisClient.Ping(checkCtxRds).Result(); err != nil {
		s.logger.Error("Redis健康检查失败", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}
	// 检查 RoomService
	checkCtxRoom, cancelRoom := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRoom()
	if _, err := s.roomClient.HealthCheck(checkCtxRoom, &emptypb.Empty{}); err != nil {
		s.logger.Error("RoomService健康检查失败", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}
	// 检查 NATS
	if !s.natsConn.IsConnected() {
		s.logger.Error("NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}

	if firstError != nil {
		return &sessionv1.HealthCheckResponse{Status: healthStatus}, nil
	}
	return &sessionv1.HealthCheckResponse{Status: healthStatus}, nil
}

// SignalUserPresence (内部方法，用于发布 UserPresence 事件)
func (s *SessionService) SignalUserPresence(ctx context.Context, userID, sessionID string, entered bool) error {
	tracer := otel.Tracer("session-service-signal")
	sigCtx, span := tracer.Start(ctx, "SignalUserPresence")
	defer span.End()
	span.SetAttributes( /* ... */ )
	s.logger.Info("收到用户状态信号，准备发布事件", zap.String("sid", sessionID), zap.String("uid", userID), zap.Bool("entered", entered))
	// --- 验证 sessionID 是否有效且直播中 ---
	session, err := s.repo.GetSessionByID(sigCtx, sessionID) // 使用 sigCtx
	if err != nil {                                          /* ... 错误处理 ... */
		return status.Errorf(codes.Internal, "...")
	}
	if session.Status != "live" { /* ... 错误处理 ... */
		return status.Errorf(codes.FailedPrecondition, "...")
	}
	currentOnlineCount := int64(-1) // 不从此服务获取
	presenceEvent := &eventv1.UserPresence{SessionId: sessionID, UserId: userID, Entered: entered, CurrentOnlineCount: currentOnlineCount, Timestamp: timestamppb.Now()}
	payload, e := proto.Marshal(presenceEvent)
	if e != nil { /*...*/
		return fmt.Errorf("...")
	}
	subject := fmt.Sprintf("events.raw.user.presence")
	if e := s.natsConn.Publish(subject, payload); e != nil { /*...*/
		return fmt.Errorf("...")
	}
	s.logger.Info("已发布用户状态事件到 NATS", zap.String("subject", subject))
	span.SetStatus(otelCodes.Ok, "...")
	return nil
}

// SendChatMessage 实现 (用于接收客户端弹幕并发布事件)
func (s *SessionService) SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (*sessionv1.SendChatMessageResponse, error) {
	s.logger.Info("收到 SendChatMessage 请求", zap.String("sid", req.GetSessionId()), zap.String("uid", req.GetUserId()))
	if req.GetSessionId() == "" || req.GetUserId() == "" || req.GetContent() == "" { /*...*/
		return nil, status.Error(codes.InvalidArgument, "...")
	}
	// --- 验证场次状态 ---
	session, err := s.repo.GetSessionByID(ctx, req.GetSessionId())
	if err != nil { /* ... 错误处理 ... */
		return nil, status.Errorf(codes.Internal, "...")
	}
	if session.Status != "live" { /* ... 错误处理 ... */
		return nil, status.Errorf(codes.FailedPrecondition, "...")
	}
	// --- 验证结束 ---
	msgID := uuid.NewString()
	ce := &eventv1.ChatMessage{MessageId: msgID, SessionId: req.GetSessionId(), UserId: req.GetUserId(), Content: req.GetContent(), Timestamp: timestamppb.Now()}
	payload, e := proto.Marshal(ce)
	if e != nil { /*...*/
		return nil, status.Error(codes.Internal, "...")
	}
	subject := fmt.Sprintf("events.raw.chat.message")        // 发布到原始弹幕主题
	if e := s.natsConn.Publish(subject, payload); e != nil { /*...*/
		return nil, status.Error(codes.Internal, "...")
	}
	s.logger.Info("ChatMessage 事件已发布到 NATS", zap.String("subject", subject))
	return &sessionv1.SendChatMessageResponse{MessageId: msgID, Success: true}, nil
}

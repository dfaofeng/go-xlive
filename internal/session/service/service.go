package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-redis/redis/v8" // 保留 Redis Client
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	roomv1 "go-xlive/gen/go/room/v1"
	sessionv1 "go-xlive/gen/go/session/v1"
	"go-xlive/internal/session/repository"
	db "go-xlive/internal/session/repository/db"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
		logger:      logger.Named("session_service"), // 添加名字空间
		repo:        repo,
		roomClient:  roomClient,
		redisClient: redisClient,
		natsConn:    natsConn,
	}
}

// CreateSession 实现
func (s *SessionService) CreateSession(ctx context.Context, req *sessionv1.CreateSessionRequest) (*sessionv1.CreateSessionResponse, error) {
	s.logger.Info("收到 CreateSession 请求", zap.String("room_id", req.GetRoomId()))
	if req.GetRoomId() == "" {
		s.logger.Warn("...")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}

	s.logger.Debug("正在验证房间状态", zap.String("room_id", req.GetRoomId()))
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	roomResp, err := s.roomClient.GetRoom(callCtx, &roomv1.GetRoomRequest{RoomId: req.GetRoomId()})
	if err != nil { /* ... 错误处理 ... */
		s.logger.Error("...", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "...")
	}
	if roomResp.GetRoom().GetStatus() != "active" { /*...*/
		return nil, status.Errorf(codes.FailedPrecondition, "...")
	}
	s.logger.Info("房间验证成功", zap.String("room_id", req.GetRoomId()))

	sessionID := uuid.NewString()
	startTime := time.Now()
	params := db.CreateSessionParams{
		SessionID: sessionID, RoomID: req.GetRoomId(),
		StartTime: pgtype.Timestamptz{Time: startTime, Valid: true}, Status: "live",
	}
	createdDBSession, err := s.repo.CreateSession(ctx, params)
	if err != nil { /* ... 错误处理 ... */
		s.logger.Error("...", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "...")
	}

	protoSession := &sessionv1.Session{
		SessionId: createdDBSession.SessionID, RoomId: createdDBSession.RoomID,
		StartTime: timestamppb.New(createdDBSession.StartTime.Time), Status: createdDBSession.Status,
		TotalEvents: 0, TotalDanmaku: 0, TotalGiftsValue: 0, // 初始化聚合值为 0
	}

	// TODO: 发布 SessionStarted 事件到 NATS?

	s.logger.Info("场次创建成功", zap.String("session_id", protoSession.SessionId), zap.String("room_id", protoSession.RoomId))
	return &sessionv1.CreateSessionResponse{Session: protoSession}, nil
}

// UpdateSessionAggregates 实现
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
		if errors.Is(err, pgx.ErrNoRows) { /* ... 错误处理 ... */
			return nil, status.Errorf(codes.NotFound, "...")
		}
		s.logger.Error("更新场次聚合数据失败", zap.Error(err), zap.String("session_id", req.GetSessionId()))
		return nil, status.Errorf(codes.Internal, "更新聚合数据失败: %v", err)
	}

	protoSession := &sessionv1.Session{
		SessionId: updatedDBSession.SessionID, RoomId: updatedDBSession.RoomID,
		StartTime: timestamppb.New(updatedDBSession.StartTime.Time), Status: updatedDBSession.Status,
		TotalEvents: updatedDBSession.TotalEvents, TotalDanmaku: updatedDBSession.TotalDanmaku, TotalGiftsValue: updatedDBSession.TotalGiftsValue,
	}
	if updatedDBSession.EndTime.Valid {
		protoSession.EndTime = timestamppb.New(updatedDBSession.EndTime.Time)
	}

	s.logger.Info("场次聚合数据更新成功", zap.String("session_id", req.GetSessionId()))
	return &sessionv1.UpdateSessionAggregatesResponse{UpdatedSession: protoSession}, nil
}

// EndSession 实现
func (s *SessionService) EndSession(ctx context.Context, req *sessionv1.EndSessionRequest) (*sessionv1.EndSessionResponse, error) {
	s.logger.Info("收到 EndSession 请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" { /*...*/
		return nil, status.Error(codes.InvalidArgument, "...")
	}
	endTime := time.Now()
	params := db.EndSessionParams{SessionID: req.GetSessionId(), EndTime: pgtype.Timestamptz{Time: endTime, Valid: true}}
	endedDBSession, err := s.repo.EndSession(ctx, params)
	if err != nil { /* ... 错误处理 ErrNoRows 和其他错误 ... */
		return nil, status.Errorf(codes.Internal, "...")
	}

	protoSession := &sessionv1.Session{
		SessionId: endedDBSession.SessionID, RoomId: endedDBSession.RoomID,
		StartTime: timestamppb.New(endedDBSession.StartTime.Time), EndTime: timestamppb.New(endedDBSession.EndTime.Time),
		Status: endedDBSession.Status, TotalEvents: endedDBSession.TotalEvents,
		TotalDanmaku: endedDBSession.TotalDanmaku, TotalGiftsValue: endedDBSession.TotalGiftsValue,
	}

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
	// --- NATS 事件结束 ---

	s.logger.Info("场次结束成功", zap.String("session_id", protoSession.SessionId))
	return &sessionv1.EndSessionResponse{Session: protoSession}, nil
}

// GetSession 实现
func (s *SessionService) GetSession(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	s.logger.Info("收到 GetSession 请求", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" { /*...*/
		return nil, status.Error(codes.InvalidArgument, "...")
	}
	dbSession, err := s.repo.GetSessionByID(ctx, req.GetSessionId())
	if err != nil { /* ... 错误处理 ErrNoRows 和其他错误 ... */
		return nil, status.Errorf(codes.Internal, "...")
	}

	protoSession := &sessionv1.Session{
		SessionId: dbSession.SessionID, RoomId: dbSession.RoomID,
		StartTime: timestamppb.New(dbSession.StartTime.Time), Status: dbSession.Status,
		TotalEvents: dbSession.TotalEvents, TotalDanmaku: dbSession.TotalDanmaku, TotalGiftsValue: dbSession.TotalGiftsValue,
	}
	if dbSession.EndTime.Valid {
		protoSession.EndTime = timestamppb.New(dbSession.EndTime.Time)
	}
	s.logger.Info("场次信息检索成功", zap.String("session_id", protoSession.SessionId))
	return &sessionv1.GetSessionResponse{Session: protoSession}, nil
}

// HealthCheck 实现
func (s *SessionService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*sessionv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (SessionService)")
	healthStatus := "SERVING"
	var firstError error
	// 检查 Redis
	checkCtxRds, cancelRds := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRds()
	if _, err := s.redisClient.Ping(checkCtxRds).Result(); err != nil { /*...*/
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}
	// 检查 RoomService
	checkCtxRoom, cancelRoom := context.WithTimeout(ctx, 1*time.Second)
	defer cancelRoom()
	if _, err := s.roomClient.HealthCheck(checkCtxRoom, &emptypb.Empty{}); err != nil { /*...*/
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}
	// 检查 NATS
	if !s.natsConn.IsConnected() { /*...*/
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("...")
		}
	}
	// 检查 DB
	checkCtxDB, cancelDB := context.WithTimeout(ctx, 2*time.Second)
	defer cancelDB()
	if err := s.repo.Ping(checkCtxDB); err != nil { /*...*/
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	if firstError != nil {
		return &sessionv1.HealthCheckResponse{Status: healthStatus}, nil
	} // 不暴露内部错误
	return &sessionv1.HealthCheckResponse{Status: healthStatus}, nil
}

// SignalUserPresence (内部方法，用于发布 UserPresence 事件)
func (s *SessionService) SignalUserPresence(ctx context.Context, userID, sessionID string, entered bool) error {
	tracer := otel.Tracer("session-service-signal")
	sigCtx, span := tracer.Start(ctx, "SignalUserPresence")
	defer span.End()
	span.SetAttributes(attribute.String("sid", sessionID), attribute.String("uid", userID), attribute.Bool("entered", entered))
	s.logger.Info("收到用户状态信号，准备发布事件", zap.String("sid", sessionID), zap.String("uid", userID), zap.Bool("entered", entered))

	// --- 验证 sessionID 是否有效且直播中 ---
	_, err := s.repo.GetSessionByID(sigCtx, sessionID) // 使用 sigCtx
	if err != nil {                                    /* ... 错误处理 ... */
		return status.Errorf(codes.Internal, "...")
	}
	// TODO: 检查 session.Status

	currentOnlineCount := int64(-1) // 不再从此服务获取

	presenceEvent := &eventv1.UserPresence{SessionId: sessionID, UserId: userID, Entered: entered, CurrentOnlineCount: currentOnlineCount, Timestamp: timestamppb.Now()}
	payload, err := proto.Marshal(presenceEvent)
	if err != nil { /*...*/
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "...")
		return fmt.Errorf("...: %w", err)
	}

	subject := fmt.Sprintf("events.raw.user.presence")
	if err := s.natsConn.Publish(subject, payload); err != nil { /*...*/
		span.RecordError(err)
		span.SetStatus(otelCodes.Error, "...")
		return fmt.Errorf("...: %w", err)
	}

	s.logger.Info("已发布用户状态事件到 NATS", zap.String("subject", subject), zap.String("sid", sessionID), zap.String("uid", userID))
	span.SetStatus(otelCodes.Ok, "...")
	return nil
}

// SendChatMessage 实现 (用于接收客户端弹幕并发布事件)
func (s *SessionService) SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (*sessionv1.SendChatMessageResponse, error) {
	s.logger.Info("收到 SendChatMessage 请求", zap.String("sid", req.GetSessionId()), zap.String("uid", req.GetUserId()))
	if req.GetSessionId() == "" || req.GetUserId() == "" || req.GetContent() == "" { /*...*/
		return nil, status.Error(codes.InvalidArgument, "...")
	}
	// TODO: 验证场次和用户

	msgID := uuid.NewString()
	chatEvent := &eventv1.ChatMessage{MessageId: msgID, SessionId: req.GetSessionId(), UserId: req.GetUserId(), Content: req.GetContent(), Timestamp: timestamppb.Now()}
	payload, err := proto.Marshal(chatEvent)
	if err != nil { /*...*/
		return nil, status.Error(codes.Internal, "...")
	}

	subject := fmt.Sprintf("events.raw.chat.message")            // 发布到原始弹幕主题
	if err := s.natsConn.Publish(subject, payload); err != nil { /*...*/
		return nil, status.Error(codes.Internal, "...")
	}

	s.logger.Info("ChatMessage 事件已发布到 NATS", zap.String("subject", subject), zap.String("sid", req.GetSessionId()), zap.String("mid", msgID))
	return &sessionv1.SendChatMessageResponse{MessageId: msgID, Success: true}, nil
}

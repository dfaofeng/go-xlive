package service

import (
	"context"
	"errors"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype" // 导入 pgtype
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	"go-xlive/internal/event/repository"
	db "go-xlive/internal/event/repository/db" // sqlc 生成的包
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	// "go.opentelemetry.io/otel"
	// "go.opentelemetry.io/otel/attribute"
	// otelCodes "go.opentelemetry.io/otel/codes"
)

// EventService 结构体定义
type EventService struct {
	eventv1.UnimplementedEventServiceServer
	repo     repository.Repository
	logger   *zap.Logger
	natsConn *nats.Conn
	natsSubs []*nats.Subscription
	muSubs   sync.Mutex
}

// NewEventService 创建 EventService 实例
func NewEventService(logger *zap.Logger, repo repository.Repository, nc *nats.Conn) *EventService {
	if logger == nil {
		log.Fatal("EventService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("EventService 需要 Repository")
	}
	if nc == nil {
		log.Fatal("EventService 需要 NATS Connection")
	}
	return &EventService{
		logger:   logger.Named("event_service"),
		repo:     repo,
		natsConn: nc,
		natsSubs: make([]*nats.Subscription, 0),
	}
}

// StartNatsSubscription 启动 NATS 订阅以接收原始事件
func (s *EventService) StartNatsSubscription(ctx context.Context, wg *sync.WaitGroup) {
	subjectToSubscribe := "events.raw.>" // 订阅所有以 events.raw. 开头的主题
	queueGroupName := "event-service-workers"
	numSubscribers := runtime.NumCPU()
	wg.Add(numSubscribers)
	s.logger.Info("正在启动 NATS 订阅者 (EventService)...",
		zap.String("subject", subjectToSubscribe),
		zap.Int("count", numSubscribers),
		zap.String("queue_group", queueGroupName))

	natsMsgHandler := s.createNatsMsgHandler() // 获取 handler

	s.muSubs.Lock()                                            // 加锁保护 s.natsSubs
	s.natsSubs = make([]*nats.Subscription, 0, numSubscribers) // 确保 slice 被初始化
	for i := 0; i < numSubscribers; i++ {
		go func(workerID int) {
			defer wg.Done()
			// 使用 QueueSubscribe 同步订阅，加入队列组
			sub, err := s.natsConn.QueueSubscribeSync(subjectToSubscribe, queueGroupName)
			if err != nil {
				s.logger.Error("NATS 队列订阅失败 (EventService)", zap.Int("worker_id", workerID), zap.Error(err))
				return
			}

			s.muSubs.Lock() // 加锁添加订阅对象
			s.natsSubs = append(s.natsSubs, sub)
			s.muSubs.Unlock() // 解锁

			// 退出时取消订阅并清理列表
			defer func() {
				s.logger.Info("正在取消 NATS 订阅 (EventService worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
				// Drain 可能更好，但 Unsubscribe 更快
				if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
					s.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
				}
				s.muSubs.Lock()
				newSubs := make([]*nats.Subscription, 0, len(s.natsSubs)-1)
				for _, ss := range s.natsSubs {
					if ss != sub {
						newSubs = append(newSubs, ss)
					}
				}
				s.natsSubs = newSubs
				s.muSubs.Unlock()
			}()

			s.logger.Info("NATS 订阅者已启动 (EventService)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))

			for {
				// 使用传入的 ctx 来监听退出信号
				msg, err := sub.NextMsgWithContext(ctx)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
						s.logger.Info("NATS 订阅者收到退出信号或连接关闭 (EventService)，正在退出...", zap.Int("worker_id", workerID), zap.Error(err))
						return // 正常退出 Goroutine
					}
					s.logger.Error("NATS NextMsg 出错 (EventService)", zap.Int("worker_id", workerID), zap.Error(err))
					// 检查是否需要退出
					select {
					case <-ctx.Done():
						s.logger.Info("NATS 订阅者收到退出信号 (出错后) (EventService)，正在退出...", zap.Int("worker_id", workerID))
						return
					default:
						func() { natsMsgHandler(msg) }() // 调用封装后的处理方法
						time.Sleep(1 * time.Second)      // 短暂暂停后继续尝试
						continue
					}
				}
				// 检查是否需要在处理消息前退出
				select {
				case <-ctx.Done():
					s.logger.Info("NATS 订阅者收到退出信号 (处理消息前) (EventService)，正在退出...", zap.Int("worker_id", workerID))
					return
				default:
					// 安全地调用消息处理方法
					func() {
						// 可选：添加 recover 防止单个消息处理 panic
						// defer func() { ... }()
						s.handleNatsMessage(msg)
					}()
				}
			}
		}(i)
	}
	s.muSubs.Unlock() // 解锁
	// --- NATS 订阅启动结束 ---
}

// createNatsMsgHandler 返回 NATS 消息处理器
func (s *EventService) createNatsMsgHandler() func(msg *nats.Msg) {
	return s.handleNatsMessage // 直接返回方法引用
}

// handleNatsMessage 处理单条 NATS 消息 (完善解析逻辑)
func (s *EventService) handleNatsMessage(msg *nats.Msg) {
	// TODO: 添加 OpenTelemetry 追踪 Span

	s.logger.Debug("EventService 收到 NATS 消息", zap.String("subject", msg.Subject), zap.Int("data_len", len(msg.Data)))

	// 1. 解析事件信息
	eventType, sessionID, roomID, userID, eventTime := s.parseAndExtractEventInfo(msg) // 调用新的解析函数
	if eventType == eventv1.EventType_EVENT_TYPE_UNSPECIFIED || eventTime.IsZero() {
		s.logger.Warn("无法确定事件类型或时间戳，跳过存储", zap.String("subject", msg.Subject))
		// span?.SetStatus(otelCodes.Error, "Cannot determine event type or timestamp")
		return
	}

	// 2. 构造存储参数
	params := db.CreateEventParams{
		SessionID: pgtype.Text{String: sessionID, Valid: sessionID != ""},
		RoomID:    pgtype.Text{String: roomID, Valid: roomID != ""},
		EventType: eventType.String(),
		UserID:    userID, // 类型是 pgtype.Text
		EventTime: pgtype.Timestamptz{Time: eventTime, Valid: true},
		Data:      msg.Data, // 存储原始 Protobuf
	}

	// 3. 调用 Repository 存储事件
	storeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.repo.CreateEvent(storeCtx, params); err != nil {
		s.logger.Error("存储事件到数据库失败",
			zap.String("session_id", sessionID),
			zap.String("room_id", roomID),
			zap.String("event_type", eventType.String()),
			zap.String("user_id", userID.String), // .String 安全，因为 pgtype.Text{} 的 String 是 ""
			zap.Error(err))
		// span?.RecordError(err); span?.SetStatus(otelCodes.Error, "Failed to store event")
		// TODO: 错误处理，例如放入死信队列
	} else {
		s.logger.Debug("事件存储成功",
			zap.String("session_id", sessionID),
			zap.String("room_id", roomID),
			zap.String("event_type", eventType.String()))
		// span?.SetStatus(otelCodes.Ok, "Event stored")
	}
}

// parseAndExtractEventInfo 解析 NATS 消息，提取信息用于存储
// 返回: 事件类型枚举, sessionID, roomID, userID (pgtype.Text), 事件时间
func (s *EventService) parseAndExtractEventInfo(msg *nats.Msg) (
	eventType eventv1.EventType,
	sessionID, roomID string,
	userID pgtype.Text, // 使用 pgtype.Text 因为 userID 可能为空
	eventTime time.Time) {

	eventType = eventv1.EventType_EVENT_TYPE_UNSPECIFIED // 默认值

	// 尝试按已知事件类型解析 Protobuf
	var userPresence eventv1.UserPresence
	if err := proto.Unmarshal(msg.Data, &userPresence); err == nil && userPresence.GetSessionId() != "" && userPresence.GetTimestamp().IsValid() {
		eventType = eventv1.EventType_EVENT_TYPE_USER_PRESENCE
		sessionID = userPresence.GetSessionId()
		userID = pgtype.Text{String: userPresence.GetUserId(), Valid: userPresence.GetUserId() != ""}
		eventTime = userPresence.GetTimestamp().AsTime()
		// TODO: 尝试从 sessionID 或其他地方获取 roomID
		s.logger.Debug("成功解析 UserPresence", zap.String("session_id", sessionID), zap.String("user_id", userID.String))
		return
	}

	var chatMessage eventv1.ChatMessage
	if err := proto.Unmarshal(msg.Data, &chatMessage); err == nil && chatMessage.GetSessionId() != "" && chatMessage.GetTimestamp().IsValid() {
		eventType = eventv1.EventType_EVENT_TYPE_CHAT_MESSAGE
		sessionID = chatMessage.GetSessionId()
		userID = pgtype.Text{String: chatMessage.GetUserId(), Valid: chatMessage.GetUserId() != ""}
		eventTime = chatMessage.GetTimestamp().AsTime()
		// TODO: 获取 RoomID
		s.logger.Debug("成功解析 ChatMessage", zap.String("session_id", sessionID), zap.String("user_id", userID.String))
		return
	}

	var giftSent eventv1.GiftSent
	if err := proto.Unmarshal(msg.Data, &giftSent); err == nil && giftSent.GetSessionId() != "" && giftSent.GetTimestamp().IsValid() {
		eventType = eventv1.EventType_EVENT_TYPE_GIFT
		sessionID = giftSent.GetSessionId()
		userID = pgtype.Text{String: giftSent.GetFromUserId(), Valid: giftSent.GetFromUserId() != ""} // 发送者
		eventTime = giftSent.GetTimestamp().AsTime()
		// TODO: 获取 RoomID
		s.logger.Debug("成功解析 GiftSent", zap.String("session_id", sessionID), zap.String("from_user_id", userID.String))
		return
	}

	var roomStatus eventv1.RoomStatusChanged
	if err := proto.Unmarshal(msg.Data, &roomStatus); err == nil && roomStatus.GetRoomId() != "" && roomStatus.GetTimestamp().IsValid() {
		eventType = eventv1.EventType_EVENT_TYPE_ROOM_STATUS
		roomID = roomStatus.GetRoomId()
		// sessionID 通常为空
		// userID 通常为空
		eventTime = roomStatus.GetTimestamp().AsTime()
		s.logger.Debug("成功解析 RoomStatusChanged", zap.String("room_id", roomID), zap.String("status", roomStatus.NewStatus))
		return
	}

	// 如果所有已知类型都解析失败，则尝试从主题推断（可能不太可靠）
	s.logger.Warn("无法通过 Protobuf 解析事件，尝试从主题推断", zap.String("subject", msg.Subject))
	subjectParts := strings.Split(msg.Subject, ".")
	if len(subjectParts) >= 4 && subjectParts[0] == "events" && subjectParts[1] == "raw" {
		domain := subjectParts[2]
		action := strings.Join(subjectParts[3:], ".")
		typeStr := domain + "." + action // 构造类型字符串
		if val, ok := eventv1.EventType_value[strings.ToUpper(strings.ReplaceAll(typeStr, ".", "_"))]; ok {
			eventType = eventv1.EventType(val)
			eventTime = time.Now() // 使用当前时间作为近似值
			s.logger.Warn("从主题推断出事件类型，但无法解析详细信息", zap.String("subject", msg.Subject), zap.Stringer("inferred_type", eventType))
			// 尝试从主题提取 ID (非常不推荐，仅作示例)
			// if domain == "session" && len(subjectParts) >= 5 { sessionID = subjectParts[2] }
			// if domain == "room" && len(subjectParts) >= 5 { roomID = subjectParts[2] }
			return // 返回推断的类型和当前时间
		}
	}

	s.logger.Error("完全无法解析 NATS 消息", zap.String("subject", msg.Subject))
	return // 返回零值
}

// QueryEvents 实现查询接口 (完善分页和时间过滤)
func (s *EventService) QueryEvents(ctx context.Context, req *eventv1.QueryEventsRequest) (*eventv1.QueryEventsResponse, error) {
	s.logger.Info("收到 QueryEvents 请求",
		zap.String("session_id", req.GetSessionId()),
		zap.Stringer("type", req.GetEventType()),
		zap.Int32("limit", req.GetLimit()),
		zap.Int32("offset", req.GetOffset()),
		zap.Bool("start_time_valid", req.GetStartTime().IsValid()),
		zap.Bool("end_time_valid", req.GetEndTime().IsValid()),
	)
	if req.GetSessionId() == "" && req.GetEventType() != eventv1.EventType_EVENT_TYPE_ROOM_STATUS {
		// 只有房间状态事件允许 session_id 为空 (假设按 room_id 查)
		// 其他事件类型必须提供 session_id
		return nil, status.Error(codes.InvalidArgument, "非房间状态查询必须提供 session_id")
	}

	var dbEvents []db.Event
	var totalCount int64
	var err error

	// 处理分页和时间戳参数
	limit := req.GetLimit()
	if limit <= 0 || limit > 1000 {
		limit = 100
	} // 添加最大 limit 限制
	offset := req.GetOffset()
	if offset < 0 {
		offset = 0
	}

	startTime := pgtype.Timestamptz{Valid: false}
	if req.GetStartTime().IsValid() {
		startTime = pgtype.Timestamptz{Time: req.GetStartTime().AsTime(), Valid: true}
	}
	endTime := pgtype.Timestamptz{Valid: false}
	if req.GetEndTime().IsValid() {
		endTime = pgtype.Timestamptz{Time: req.GetEndTime().AsTime(), Valid: true}
	}
	sessionIDPgType := pgtype.Text{String: req.GetSessionId(), Valid: req.GetSessionId() != ""}
	eventTypeStr := req.GetEventType().String()
	_, typeOk := eventv1.EventType_value[eventTypeStr]
	isValidTypeFilter := req.GetEventType() != eventv1.EventType_EVENT_TYPE_UNSPECIFIED && typeOk

	// 先查询总数
	countParams := db.CountEventsBySessionIDParams{
		SessionID:       sessionIDPgType,
		StartTime:       startTime,
		EndTime:         endTime,
		EventTypeFilter: eventTypeStr,
	}
	totalCount, err = s.repo.CountEventsBySessionID(ctx, countParams)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Error("查询事件总数失败", zap.Error(err))
	} else {
		err = nil
	}

	// 再查询当页数据
	if !isValidTypeFilter {
		queryP := db.QueryEventsBySessionIDParams{SessionID: sessionIDPgType, StartTime: startTime, EndTime: endTime, Lim: limit, Offs: offset}
		dbEvents, err = s.repo.QueryEventsBySessionID(ctx, queryP)
	} else {
		queryP := db.QueryEventsBySessionIDAndTypeParams{SessionID: sessionIDPgType, EventType: eventTypeStr, StartTime: startTime, EndTime: endTime, Lim: limit, Offs: offset}
		dbEvents, err = s.repo.QueryEventsBySessionIDAndType(ctx, queryP)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &eventv1.QueryEventsResponse{Events: []*eventv1.StoredEvent{}, TotalCount: int32(totalCount)}, nil
		}
		s.logger.Error("查询事件列表失败", zap.Error(err))
		return nil, status.Error(codes.Internal, "...")
	}

	// 映射结果
	protoEvents := make([]*eventv1.StoredEvent, 0, len(dbEvents))
	for _, dbEv := range dbEvents {
		ete, ok := eventv1.EventType_value[dbEv.EventType]
		if !ok {
			ete = 0
		}
		var uidStr string
		if dbEv.UserID.Valid {
			uidStr = dbEv.UserID.String
		}
		var ridStr string
		if dbEv.RoomID.Valid {
			ridStr = dbEv.RoomID.String
		}
		var eidStr string
		if dbEv.EventID.Valid {
			eidUUID, e := uuid.FromBytes(dbEv.EventID.Bytes[:])
			if e == nil {
				eidStr = eidUUID.String()
			} else {
				s.logger.Warn("...", zap.Error(e))
			}
		}
		pev := &eventv1.StoredEvent{EventId: eidStr, SessionId: dbEv.SessionID.String, RoomId: ridStr, EventType: eventv1.EventType(ete), Timestamp: timestamppb.New(dbEv.EventTime.Time), UserId: uidStr, Data: dbEv.Data}
		protoEvents = append(protoEvents, pev)
	}
	s.logger.Info("事件查询成功", zap.Int("count", len(protoEvents)), zap.Int64("total", totalCount))
	return &eventv1.QueryEventsResponse{Events: protoEvents, TotalCount: int32(totalCount)}, nil
}

// HealthCheck 实现 (添加 DB Ping)
func (s *EventService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*eventv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (EventService)")
	healthStatus := "SERVING"
	var firstError error
	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("DB 健康检查失败", zap.Error(err))
		healthStatus = "NOT_SERVING"
		firstError = err
	}
	if !s.natsConn.IsConnected() {
		s.logger.Error("NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}
	if firstError != nil {
		return &eventv1.HealthCheckResponse{Status: healthStatus}, nil
	}
	return &eventv1.HealthCheckResponse{Status: healthStatus}, nil
}

// StopNatsSubscription (保持不变)
func (s *EventService) StopNatsSubscription() {
	s.logger.Info("正在取消 EventService NATS 订阅...")
	s.muSubs.Lock()
	defer s.muSubs.Unlock()
	for i, sub := range s.natsSubs {
		if sub != nil && sub.IsValid() {
			s.logger.Debug("...", zap.Int("idx", i))
			if e := sub.Unsubscribe(); e != nil {
				s.logger.Warn("...", zap.Error(e))
			}
		}
	}
	s.natsSubs = nil
	s.logger.Info("EventService NATS 订阅已取消")
}

// --- 移除 parseEventTypeFromSubject，逻辑合并到 parseAndExtractEventInfo ---

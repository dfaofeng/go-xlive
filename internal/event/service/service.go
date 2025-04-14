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
	"github.com/jackc/pgx/v5/pgtype"
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
	// "go.opentelemetry.io/otel" // 如果需要在 handler 中创建 span
	// "go.opentelemetry.io/otel/attribute"
	// otelCodes "go.opentelemetry.io/otel/codes"
)

// EventService 结构体定义
type EventService struct {
	eventv1.UnimplementedEventServiceServer
	repo     repository.Repository
	logger   *zap.Logger
	natsConn *nats.Conn
	natsSubs []*nats.Subscription // 保存 NATS 订阅对象，用于优雅关闭
	muSubs   sync.Mutex           // 保护 natsSubs 的并发访问
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

	// --- 启动订阅者 Goroutine ---
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

			// 退出时取消订阅并清理
			defer func() {
				s.logger.Info("正在取消 NATS 订阅 (EventService worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
				if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
					s.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
				}
				s.muSubs.Lock()
				newSubs := make([]*nats.Subscription, 0, len(s.natsSubs)-1)
				for _, s := range s.natsSubs {
					if s != sub {
						newSubs = append(newSubs, s)
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
						time.Sleep(1 * time.Second) // 短暂暂停后继续尝试
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
						// 可选：添加 recover 防止单个消息处理 panic 导致 worker 退出
						// defer func() {
						//     if r := recover(); r != nil {
						//         s.logger.Error("处理 NATS 消息时发生 Panic", zap.Any("panic", r), zap.Stack("stacktrace"))
						//     }
						// }()
						s.handleNatsMessage(msg) // <--- 调用封装后的处理方法
					}()
				}
			}
		}(i)
	}
	s.muSubs.Unlock() // 解锁
	// --- NATS 订阅启动结束 ---
}

// handleNatsMessage 处理从 NATS 接收到的单条消息
func (s *EventService) handleNatsMessage(msg *nats.Msg) {
	// TODO: 添加 OpenTelemetry 追踪 Span
	// tracer := otel.Tracer("nats-event-handler")
	// msgCtx, span := tracer.Start(context.Background(), "handleRawEvent")
	// defer span.End()
	// span.SetAttributes(...)

	s.logger.Debug("EventService 处理 NATS 消息", zap.String("subject", msg.Subject), zap.Int("data_len", len(msg.Data)))

	// 1. 解析事件信息
	eventType, sessionID, roomID, userID, eventTime := s.parseEventInfo(msg) // 调用解析函数
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
		Data:      msg.Data,
	}

	// 3. 调用 Repository 存储事件
	storeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // 使用后台 Context + 超时
	// 如果上面创建了 msgCtx，应该用 msgCtx: context.WithTimeout(msgCtx, ...)
	defer cancel()
	if err := s.repo.CreateEvent(storeCtx, params); err != nil {
		s.logger.Error("存储事件到数据库失败",
			zap.String("session_id", sessionID),
			zap.String("room_id", roomID),
			zap.String("event_type", eventType.String()),
			zap.String("user_id", userID.String),
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

// QueryEvents 实现查询接口 (修正了 UUID 和 Nullable 映射)
func (s *EventService) QueryEvents(ctx context.Context, req *eventv1.QueryEventsRequest) (*eventv1.QueryEventsResponse, error) {
	s.logger.Info("收到 QueryEvents 请求",
		zap.String("session_id", req.GetSessionId()),
		zap.Stringer("type", req.GetEventType()),
		zap.Int32("limit", req.GetLimit()),
		zap.Int32("offset", req.GetOffset()),
	)
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id")
	}

	var dbEvents []db.Event
	var err error

	// TODO: 实现更灵活的查询，结合时间范围、分页等
	eventTypeStr := req.GetEventType().String()
	_, typeOk := eventv1.EventType_value[eventTypeStr]
	isValidTypeFilter := req.GetEventType() != eventv1.EventType_EVENT_TYPE_UNSPECIFIED && typeOk

	// --- 修正 SessionID 类型转换 ---
	sessionIDPgType := pgtype.Text{String: req.GetSessionId(), Valid: req.GetSessionId() != ""}

	if !isValidTypeFilter {
		// 调用 QueryEventsBySessionID 时传递 pgtype.Text
		dbEvents, err = s.repo.QueryEventsBySessionID(ctx, sessionIDPgType)
	} else {
		params := db.QueryEventsBySessionIDAndTypeParams{
			SessionID: sessionIDPgType, // 类型匹配
			EventType: eventTypeStr,
			// TODO: 添加 Limit/Offset/Time 到 Params
		}
		dbEvents, err = s.repo.QueryEventsBySessionIDAndType(ctx, params)
	}

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("未查询到相关事件", zap.String("session_id", req.GetSessionId()), zap.Stringer("type", req.GetEventType()))
			return &eventv1.QueryEventsResponse{Events: []*eventv1.StoredEvent{}}, nil
		}
		s.logger.Error("查询事件失败", zap.String("session_id", req.GetSessionId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "查询事件失败")
	}

	// 将数据库事件映射为 Protobuf 事件
	protoEvents := make([]*eventv1.StoredEvent, 0, len(dbEvents))
	for _, dbEv := range dbEvents {
		eventTypeEnum, ok := eventv1.EventType_value[dbEv.EventType]
		if !ok {
			eventTypeEnum = int32(eventv1.EventType_EVENT_TYPE_UNSPECIFIED)
		}

		var userIDStr string
		if dbEv.UserID.Valid {
			userIDStr = dbEv.UserID.String
		}
		var roomIDStr string
		if dbEv.RoomID.Valid {
			roomIDStr = dbEv.RoomID.String
		}
		var eventIDStr string
		if dbEv.EventID.Valid {
			eventUUID, e := uuid.FromBytes(dbEv.EventID.Bytes[:])
			if e == nil {
				eventIDStr = eventUUID.String()
			} else {
				s.logger.Warn("无法转换数据库 EventID", zap.Error(e))
			}
		}

		protoEv := &eventv1.StoredEvent{
			EventId:   eventIDStr,
			SessionId: dbEv.SessionID.String, // 假设 SessionID 也是 pgtype.Text
			RoomId:    roomIDStr,
			EventType: eventv1.EventType(eventTypeEnum),
			Timestamp: timestamppb.New(dbEv.EventTime.Time),
			UserId:    userIDStr,
			Data:      dbEv.Data,
		}
		protoEvents = append(protoEvents, protoEv)
	}

	s.logger.Info("事件查询成功", zap.String("session_id", req.GetSessionId()), zap.Int("count", len(protoEvents)))
	// TODO: 返回 total_count
	return &eventv1.QueryEventsResponse{Events: protoEvents}, nil
}

// HealthCheck 实现
func (s *EventService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*eventv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (EventService)")
	healthStatus := "SERVING"
	var firstError error

	// 1. 检查数据库连接
	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("依赖健康检查失败: Database", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// 2. 检查 NATS 连接
	if !s.natsConn.IsConnected() {
		s.logger.Error("依赖健康检查失败: NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}

	if firstError != nil {
		// 对于健康检查，即使依赖有问题，服务本身可能还能响应 HealthCheck 请求
		// 所以通常返回最终的 healthStatus，而不直接返回 error
		return &eventv1.HealthCheckResponse{Status: healthStatus}, nil
	}
	return &eventv1.HealthCheckResponse{Status: healthStatus}, nil
}

// StopNatsSubscription 优雅地停止所有 NATS 订阅
func (s *EventService) StopNatsSubscription() {
	s.logger.Info("正在取消 EventService NATS 订阅...")
	s.muSubs.Lock() // 加锁访问
	defer s.muSubs.Unlock()
	for i, sub := range s.natsSubs {
		if sub != nil && sub.IsValid() {
			s.logger.Debug("正在取消订阅", zap.Int("index", i), zap.String("subject", sub.Subject))
			// 使用 Drain 可能更好，等待已接收消息处理完，但 Unsubscribe 更快
			if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
				s.logger.Warn("取消订阅失败", zap.Error(err), zap.String("subject", sub.Subject))
			}
		}
	}
	s.natsSubs = nil // 清空列表
	s.logger.Info("EventService NATS 订阅已尝试取消")
}

// parseEventInfo 解析事件信息 (内部辅助函数)
// 返回: 事件类型枚举, sessionID, roomID, userID (pgtype.Text), 事件时间
func (s *EventService) parseEventInfo(msg *nats.Msg) (eventType eventv1.EventType, sessionID, roomID string, userID pgtype.Text, eventTime time.Time) {
	eventType = eventv1.EventType_EVENT_TYPE_UNSPECIFIED // 默认值
	subjectParts := strings.Split(msg.Subject, ".")
	// 假设主题格式: events.raw.<domain>.<action>[.<sub_action>...]
	if len(subjectParts) >= 4 && subjectParts[0] == "events" && subjectParts[1] == "raw" {
		domain := subjectParts[2]
		action := strings.Join(subjectParts[3:], ".")

		switch domain {
		case "user":
			if action == "presence" {
				eventType = eventv1.EventType_EVENT_TYPE_USER_PRESENCE
				var p eventv1.UserPresence
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					sessionID = p.GetSessionId()
					userID = pgtype.Text{String: p.GetUserId(), Valid: p.GetUserId() != ""}
					eventTime = p.GetTimestamp().AsTime()
					// roomID = "" // 通常 UserPresence 不直接关联 RoomID
				} else {
					s.logger.Error("无法解析 UserPresence 事件数据", zap.Error(err), zap.String("subject", msg.Subject))
				}
			} else {
				s.logger.Warn("未知的 user action", zap.String("action", action), zap.String("subject", msg.Subject))
			}
		case "chat":
			if action == "message" {
				eventType = eventv1.EventType_EVENT_TYPE_CHAT_MESSAGE
				var p eventv1.ChatMessage
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					sessionID = p.GetSessionId()
					userID = pgtype.Text{String: p.GetUserId(), Valid: p.GetUserId() != ""}
					eventTime = p.GetTimestamp().AsTime()
					// roomID = "" // 需要从 Session 或其他地方获取 RoomID?
				} else {
					s.logger.Error("无法解析 ChatMessage 事件数据", zap.Error(err), zap.String("subject", msg.Subject))
				}
			} else {
				s.logger.Warn("未知的 chat action", zap.String("action", action), zap.String("subject", msg.Subject))
			}
		case "gift":
			if action == "sent" {
				eventType = eventv1.EventType_EVENT_TYPE_GIFT
				var p eventv1.GiftSent
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					sessionID = p.GetSessionId()
					userID = pgtype.Text{String: p.GetFromUserId(), Valid: p.GetFromUserId() != ""} // 发送者
					eventTime = p.GetTimestamp().AsTime()
					// roomID = ""
				} else {
					s.logger.Error("无法解析 GiftSent 事件数据", zap.Error(err), zap.String("subject", msg.Subject))
				}
			} else {
				s.logger.Warn("未知的 gift action", zap.String("action", action), zap.String("subject", msg.Subject))
			}
		case "room":
			if strings.HasPrefix(action, "status.") {
				eventType = eventv1.EventType_EVENT_TYPE_ROOM_STATUS
				var p eventv1.RoomStatusChanged
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					roomID = p.GetRoomId()
					// sessionID = "" // 房间事件通常无 SessionID
					// userID = pgtype.Text{} // 房间事件通常无 UserID
					eventTime = p.GetTimestamp().AsTime()
				} else {
					s.logger.Error("无法解析 RoomStatusChanged 事件数据", zap.Error(err), zap.String("subject", msg.Subject))
				}
			} else {
				s.logger.Warn("未知的 room action", zap.String("action", action), zap.String("subject", msg.Subject))
			}
		default:
			s.logger.Warn("未知事件域", zap.String("domain", domain), zap.String("subject", msg.Subject))
		}
	} else {
		s.logger.Warn("无法从主题解析标准事件格式", zap.String("subject", msg.Subject))
	}
	return // 返回解析出的值（可能是零值）
}

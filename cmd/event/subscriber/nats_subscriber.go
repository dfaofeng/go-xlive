// cmd/event/subscriber/nats_subscriber.go
package subscriber

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	"go-xlive/internal/event/repository" // 导入 repository 包
	db "go-xlive/internal/event/repository/db"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	// OTEL imports if needed inside handler
	// "go.opentelemetry.io/otel"
	// "go.opentelemetry.io/otel/attribute"
	// otelCodes "go.opentelemetry.io/otel/codes"
)

// NatsSubscriber handles subscribing to NATS topics and processing messages
type NatsSubscriber struct {
	natsConn *nats.Conn
	repo     repository.Repository
	logger   *zap.Logger
	wg       *sync.WaitGroup      // WaitGroup for graceful shutdown
	cancel   context.CancelFunc   // Context cancel function for shutdown
	subs     []*nats.Subscription // Active subscriptions
	muSubs   sync.Mutex           // Mutex for subscriptions slice
}

// NewNatsSubscriber creates a new NatsSubscriber
func NewNatsSubscriber(logger *zap.Logger, nc *nats.Conn, repo repository.Repository) *NatsSubscriber {
	return &NatsSubscriber{
		natsConn: nc,
		repo:     repo,
		logger:   logger.Named("nats_subscriber"), // Add namespace to logger
		subs:     make([]*nats.Subscription, 0),
	}
}

// Start starts the NATS subscriptions
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.wg = wg
	// 创建一个可取消的内部 context，用于通知 goroutine 退出
	internalCtx, internalCancel := context.WithCancel(ctx)
	ns.cancel = internalCancel // 保存 cancel 函数以便 Shutdown 时调用

	subjectToSubscribe := "events.raw.>"
	queueGroupName := "event-service-workers"
	numSubscribers := runtime.NumCPU()

	ns.logger.Info("正在启动 NATS 订阅者 (EventService)...",
		zap.String("subject", subjectToSubscribe),
		zap.Int("count", numSubscribers),
		zap.String("queue_group", queueGroupName))

	natsMsgHandler := ns.createNatsMsgHandler() // 获取消息处理函数

	ns.muSubs.Lock()                                        // 加锁保护 subs slice
	ns.subs = make([]*nats.Subscription, 0, numSubscribers) // 初始化
	for i := 0; i < numSubscribers; i++ {
		ns.wg.Add(1) // 为每个 goroutine 增加 WaitGroup 计数
		go func(workerID int) {
			defer ns.wg.Done() // Goroutine 退出时减少计数
			sub, err := ns.natsConn.QueueSubscribeSync(subjectToSubscribe, queueGroupName)
			if err != nil {
				ns.logger.Error("...", zap.Int("id", workerID), zap.Error(err))
				return
			}

			ns.muSubs.Lock() // 加锁添加订阅对象
			ns.subs = append(ns.subs, sub)
			ns.muSubs.Unlock()

			// 退出时取消订阅
			defer func() {
				ns.logger.Info("正在取消 NATS 订阅 (EventService worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
				// Drain 会等待消息处理完，但 Unsubscribe 更快，适合强制关闭
				if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
					ns.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
				}
				// 从列表中移除 (可选，如果 Shutdown 会清空的话)
				ns.muSubs.Lock()
				newSubs := make([]*nats.Subscription, 0, len(ns.subs)-1)
				for _, s := range ns.subs {
					if s != sub {
						newSubs = append(newSubs, s)
					}
				}
				ns.subs = newSubs
				ns.muSubs.Unlock()
			}()

			ns.logger.Info("NATS 订阅者已启动 (EventService)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))

			for {
				// 使用 internalCtx 监听退出信号
				msg, err := sub.NextMsgWithContext(internalCtx)
				if err != nil {
					// 如果 context 被取消，说明是正常的退出流程
					if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
						ns.logger.Info("NATS 订阅者收到退出信号或连接关闭 (EventService)，正在退出...", zap.Int("worker_id", workerID), zap.Error(err))
						return // 退出 goroutine
					}
					// 其他错误
					ns.logger.Error("NATS NextMsg 出错 (EventService)", zap.Int("worker_id", workerID), zap.Error(err))
					// 检查是否需要退出
					select {
					case <-internalCtx.Done():
						ns.logger.Info("NATS 订阅者收到退出信号 (出错后) (EventService)，正在退出...", zap.Int("worker_id", workerID))
						return
					default:
						time.Sleep(1 * time.Second) // 短暂暂停后继续尝试
						continue
					}
				}
				// 检查是否需要在处理消息前退出
				select {
				case <-internalCtx.Done():
					ns.logger.Info("NATS 订阅者收到退出信号 (处理消息前) (EventService)，正在退出...", zap.Int("worker_id", workerID))
					return
				default:
					// 安全地处理消息
					func() {
						// 可以添加 recover 防止单个消息处理 panic
						// defer func() { ... }()
						natsMsgHandler(msg)
					}()
				}
			}
		}(i)
	}
	ns.muSubs.Unlock()
}

// Shutdown 优雅地停止所有 NATS 订阅者
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("正在通知 NATS 订阅者退出 (EventService)...")
	if ns.cancel != nil {
		ns.cancel() // 取消 context，通知所有 goroutine 退出
	}
	// StopNatsSubscription(ns) // 可以选择在这里调用显式取消订阅
}

// createNatsMsgHandler 创建 NATS 消息处理函数
func (ns *NatsSubscriber) createNatsMsgHandler() func(msg *nats.Msg) {
	return func(msg *nats.Msg) {
		// TODO: 添加 OpenTelemetry 追踪 Span
		// tracer := otel.Tracer("nats-event-handler")
		// msgCtx, span := tracer.Start(context.Background(), "handleRawEvent")
		// defer span.End()
		// span.SetAttributes(...)

		ns.logger.Debug("EventService 收到 NATS 消息", zap.String("subject", msg.Subject), zap.Int("data_len", len(msg.Data)))

		// 解析事件信息
		eventType, sessionID, roomID, userID, eventTime := ns.parseEventInfo(msg)
		if eventType == eventv1.EventType_EVENT_TYPE_UNSPECIFIED || eventTime.IsZero() {
			ns.logger.Warn("无法确定事件类型或时间戳，跳过存储", zap.String("subject", msg.Subject))
			// span?.SetStatus(otelCodes.Error, "...")
			return
		}

		// 构造存储参数
		params := db.CreateEventParams{
			SessionID: pgtype.Text{String: sessionID, Valid: sessionID != ""},
			RoomID:    pgtype.Text{String: roomID, Valid: roomID != ""},
			EventType: eventType.String(),
			UserID:    userID,
			EventTime: pgtype.Timestamptz{Time: eventTime, Valid: true},
			Data:      msg.Data,
		}

		// 调用 Repository 存储事件
		storeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // 使用后台 Context
		// 如果上面创建了 msgCtx，应该用 msgCtx: context.WithTimeout(msgCtx, ...)
		defer cancel()
		if err := ns.repo.CreateEvent(storeCtx, params); err != nil {
			ns.logger.Error("存储事件到数据库失败" /* ... zap fields ... */, zap.Error(err))
			// span?.RecordError(err); span?.SetStatus(otelCodes.Error, "...")
			// TODO: 错误处理
		} else {
			ns.logger.Debug("事件存储成功" /* ... zap fields ... */)
			// span?.SetStatus(otelCodes.Ok, "...")
		}
	}
}

// parseEventInfo 解析事件信息 (内部辅助函数)
func (ns *NatsSubscriber) parseEventInfo(msg *nats.Msg) (eventType eventv1.EventType, sessionID, roomID string, userID pgtype.Text, eventTime time.Time) {
	eventType = eventv1.EventType_EVENT_TYPE_UNSPECIFIED // 默认值
	subjectParts := strings.Split(msg.Subject, ".")
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
				} else {
					ns.logger.Error("...", zap.Error(err))
				}
			} else {
				ns.logger.Warn("...", zap.String("action", action))
			}
		case "chat":
			if action == "message" {
				eventType = eventv1.EventType_EVENT_TYPE_CHAT_MESSAGE
				var p eventv1.ChatMessage
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					sessionID = p.GetSessionId()
					userID = pgtype.Text{String: p.GetUserId(), Valid: p.GetUserId() != ""}
					eventTime = p.GetTimestamp().AsTime()
				} else {
					ns.logger.Error("...", zap.Error(err))
				}
			} else {
				ns.logger.Warn("...", zap.String("action", action))
			}
		case "gift":
			if action == "sent" {
				eventType = eventv1.EventType_EVENT_TYPE_GIFT
				var p eventv1.GiftSent
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					sessionID = p.GetSessionId()
					userID = pgtype.Text{String: p.GetFromUserId(), Valid: p.GetFromUserId() != ""}
					eventTime = p.GetTimestamp().AsTime()
				} else {
					ns.logger.Error("...", zap.Error(err))
				}
			} else {
				ns.logger.Warn("...", zap.String("action", action))
			}
		case "room":
			if strings.HasPrefix(action, "status.") {
				eventType = eventv1.EventType_EVENT_TYPE_ROOM_STATUS
				var p eventv1.RoomStatusChanged
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					roomID = p.GetRoomId()
					eventTime = p.GetTimestamp().AsTime()
				} else {
					ns.logger.Error("...", zap.Error(err))
				}
			} else {
				ns.logger.Warn("...", zap.String("action", action))
			}
		default:
			ns.logger.Warn("未知事件域", zap.String("domain", domain))
		}
	} else {
		ns.logger.Warn("无法解析事件主题", zap.String("subject", msg.Subject))
	}
	return
}

// StopNatsSubscription 优雅地停止所有 NATS 订阅
func (ns *NatsSubscriber) StopNatsSubscription() {
	ns.logger.Info("正在取消 EventService NATS 订阅...")
	// 1. 取消 context，通知所有 goroutine 退出循环
	if ns.cancel != nil {
		ns.cancel()
	}
	// 2. (可选) 明确取消 NATS 订阅对象 (goroutine 退出时也会自动取消)
	ns.muSubs.Lock()
	defer ns.muSubs.Unlock()
	for i, sub := range ns.subs {
		if sub != nil && sub.IsValid() {
			ns.logger.Debug("正在显式取消订阅", zap.Int("index", i), zap.String("subject", sub.Subject))
			// 使用 Drain 更好，它会等待已接收消息处理完，但 Unsubscribe 更快
			if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
				ns.logger.Warn("取消订阅失败", zap.Error(err), zap.String("subject", sub.Subject))
			}
		}
	}
	ns.subs = nil // 清空列表
	ns.logger.Info("EventService NATS 订阅已尝试取消")
}

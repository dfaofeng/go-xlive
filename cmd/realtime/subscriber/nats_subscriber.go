package subscriber

import (
	"context"
	"errors"
	"go-xlive/pkg/observability" // <-- 新增: 导入 observability
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1" // 需要导入 realtime proto

	// --- !!! 修改: 需要导入 service 包以获取 Hub 类型定义 !!! ---
	realtimeservice "go-xlive/internal/realtime/service"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

// NatsSubscriber handles NATS subscriptions for the Realtime service
type NatsSubscriber struct {
	natsConn *nats.Conn
	// --- !!! 修改: 依赖 *realtimeservice.SubscriptionHub !!! ---
	hub *realtimeservice.SubscriptionHub // 依赖 Hub 进行广播
	// --- 修改结束 ---
	logger *zap.Logger
	wg     *sync.WaitGroup
	cancel context.CancelFunc   // Context cancel function for shutdown
	subs   []*nats.Subscription // Active subscriptions
	muSubs sync.Mutex           // Mutex for subscriptions slice
}

// NewNatsSubscriber creates a new NatsSubscriber
// --- !!! 修改: 接收 *realtimeservice.SubscriptionHub !!! ---
func NewNatsSubscriber(logger *zap.Logger, nc *nats.Conn, hub *realtimeservice.SubscriptionHub) *NatsSubscriber {
	return &NatsSubscriber{
		natsConn: nc,
		hub:      hub, // <--- 保存 Hub 实例
		logger:   logger.Named("nats_subscriber"),
		subs:     make([]*nats.Subscription, 0),
	}
}

// --- 修改结束 ---

// Start starts the NATS subscriptions
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.wg = wg
	internalCtx, internalCancel := context.WithCancel(ctx)
	ns.cancel = internalCancel

	subjectsToSubscribe := []string{
		"events.raw.user.presence",
		"events.raw.chat.message",
		"events.raw.gift.sent",
		// 可以添加更多需要实时广播的事件主题
	}
	queueGroupName := "realtime-service-workers"
	numSubscribersPerSubject := 2 // 可以根据负载调整

	ns.logger.Info("正在启动 NATS 订阅者 (RealtimeService)...",
		zap.Strings("subjects", subjectsToSubscribe),
		zap.Int("subs_per_subject", numSubscribersPerSubject),
		zap.String("queue_group", queueGroupName))

	natsMsgHandler := ns.createNatsMsgHandler() // 获取消息处理函数

	ns.muSubs.Lock()
	ns.subs = make([]*nats.Subscription, 0, len(subjectsToSubscribe)*numSubscribersPerSubject)
	for _, subject := range subjectsToSubscribe {
		for i := 0; i < numSubscribersPerSubject; i++ {
			ns.wg.Add(1)
			go func(subj string, workerID int) {
				// defer ns.wg.Done() // 移动到订阅成功之后
				sub, err := ns.natsConn.QueueSubscribeSync(subj, queueGroupName)
				if err != nil {
					ns.logger.Error("NATS 队列订阅失败 (Realtime)", zap.String("subj", subj), zap.Int("id", workerID), zap.Error(err))
					ns.wg.Done()
					return
				}
				defer ns.wg.Done()

				ns.muSubs.Lock()
				ns.subs = append(ns.subs, sub)
				ns.muSubs.Unlock() // 使用锁添加

				defer func() { // 退出时取消订阅并清理
					ns.logger.Info("正在取消 NATS 订阅 (Realtime worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
					if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
						ns.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
					}
					ns.muSubs.Lock()
					ns.subs = removeSubscription(ns.subs, sub)
					ns.muSubs.Unlock() // 使用锁移除
				}()
				ns.logger.Info("NATS 订阅者已启动 (RealtimeService)", zap.String("subject", subj), zap.Int("worker_id", workerID))

				for {
					msg, err := sub.NextMsgWithContext(internalCtx) // 使用 internalCtx
					if err != nil {
						if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
							ns.logger.Info("NATS 订阅者收到退出信号或连接关闭 (Realtime)，正在退出...", zap.String("subject", subj), zap.Int("worker_id", workerID), zap.Error(err))
							return // 正常退出 Goroutine
						}
						ns.logger.Error("NATS NextMsg 出错 (Realtime)", zap.String("subject", subj), zap.Int("worker_id", workerID), zap.Error(err))
						select {
						case <-internalCtx.Done():
							ns.logger.Info("NATS 订阅者收到退出信号 (出错后) (Realtime)，正在退出...", zap.Int("worker_id", workerID))
							return
						default:
							time.Sleep(1 * time.Second) // 短暂暂停后继续尝试
							continue
						}
					}
					// 检查是否需要在处理消息前退出
					select {
					case <-internalCtx.Done():
						ns.logger.Info("NATS 订阅者收到退出信号 (处理消息前) (Realtime)，正在退出...", zap.Int("worker_id", workerID))
						return
					default:
						// 安全地调用消息处理方法
						func() {
							// 可以添加 recover 防止单个消息处理 panic
							// defer func() { ... }()
							natsMsgHandler(msg)
						}()
					}
				}
			}(subject, i)
		}
	}
	ns.muSubs.Unlock()
}

// Shutdown 优雅地停止所有 NATS 订阅者
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("正在通知 NATS 订阅者退出 (RealtimeService)...")
	if ns.cancel != nil {
		ns.cancel() // 取消 context，通知所有 goroutine 退出循环
	}
	// ns.StopNatsSubscription() // 移除显式取消调用，依赖 context 和 defer
}

// createNatsMsgHandler 创建 NATS 消息处理闭包 (调用 hub.Broadcast)
// --- 修改: 提取 NATS 追踪上下文 ---
func (ns *NatsSubscriber) createNatsMsgHandler() func(msg *nats.Msg) {
	return func(msg *nats.Msg) {
		// --- 提取追踪上下文 ---
		natsHeader := msg.Header
		headersMap := make(map[string]string)
		if natsHeader != nil {
			for k, v := range natsHeader {
				if len(v) > 0 {
					headersMap[k] = v[0] // 取第一个值
				}
			}
		}
		natsCtx := observability.ExtractNATSHeaders(context.Background(), headersMap)
		// --- 提取结束 ---

		// --- 启动新的 Span ---
		tracer := otel.Tracer("realtime-service-nats")
		// 使用 natsCtx 启动 Span，而不是 context.Background()
		ctx, span := tracer.Start(natsCtx, "handleNatsMessageForRealtime")
		defer span.End()

		// 添加 NATS 相关属性
		span.SetAttributes(attribute.String("nats.subject", msg.Subject))
		// 添加提取的 Headers 到 Span 属性 (用于调试)
		headerAttributes := make([]attribute.KeyValue, 0, len(headersMap))
		for k, v := range headersMap {
			headerAttributes = append(headerAttributes, attribute.String("nats.header."+k, v))
		}
		span.SetAttributes(headerAttributes...)
		// --- Span 启动结束 ---

		// 使用带有追踪信息的 ctx 记录日志
		logger := ns.logger.With(zap.Any("trace_id", span.SpanContext().TraceID()), zap.Any("span_id", span.SpanContext().SpanID()))
		logger.Debug("RealtimeService 收到 NATS 消息",
			zap.String("subject", msg.Subject),
			zap.Any("headers", headersMap), // 记录提取的 Headers
		)

		var sessionEvent *realtimev1.SessionEvent
		var targetSessionID string

		// --- 重构: 根据主题解析事件 ---
		subjectParts := strings.Split(msg.Subject, ".")
		if len(subjectParts) < 4 || subjectParts[0] != "events" || subjectParts[1] != "raw" {
			logger.Warn("无法解析的 NATS 主题 (Realtime)", zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Error, "Invalid NATS subject format")
			return
		}
		domain := subjectParts[2]
		action := subjectParts[3] // 简化，假设 action 是第四部分
		span.SetAttributes(
			attribute.String("event.domain", domain),
			attribute.String("event.action", action),
		)

		switch domain {
		case "user":
			if action == "presence" {
				var p eventv1.UserPresence
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					targetSessionID = p.GetSessionId()
					sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_UserPresence{UserPresence: &p}}
					span.SetAttributes(attribute.String("event.type", "UserPresence"))
				} else {
					logger.Error("解析 UserPresence 事件失败 (Realtime)", zap.Error(err), zap.String("subject", msg.Subject))
					span.RecordError(err)
					span.SetStatus(otelCodes.Error, "Unmarshal UserPresence failed")
					return
				}
			}
		case "chat":
			if action == "message" {
				var p eventv1.ChatMessage
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					targetSessionID = p.GetSessionId()
					sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_ChatMessage{ChatMessage: &p}}
					span.SetAttributes(attribute.String("event.type", "ChatMessage"))
				} else {
					logger.Error("解析 ChatMessage 事件失败 (Realtime)", zap.Error(err), zap.String("subject", msg.Subject))
					span.RecordError(err)
					span.SetStatus(otelCodes.Error, "Unmarshal ChatMessage failed")
					return
				}
			}
		case "gift":
			if action == "sent" {
				var p eventv1.GiftSent
				if err := proto.Unmarshal(msg.Data, &p); err == nil {
					targetSessionID = p.GetSessionId()
					sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_GiftSent{GiftSent: &p}}
					span.SetAttributes(attribute.String("event.type", "GiftSent"))
				} else {
					logger.Error("解析 GiftSent 事件失败 (Realtime)", zap.Error(err), zap.String("subject", msg.Subject))
					span.RecordError(err)
					span.SetStatus(otelCodes.Error, "Unmarshal GiftSent failed")
					return
				}
			}
		default:
			// 忽略其他域的事件
			logger.Debug("RealtimeService 忽略 NATS 消息 (未知域)", zap.String("domain", domain), zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Ok, "Event domain ignored by realtime")
			return
		}
		// --- 解析结束 ---

		if targetSessionID != "" && sessionEvent != nil {
			span.SetAttributes(attribute.String("target_session_id", targetSessionID))
			// Broadcast 不接受 context，但其内部操作（如查找连接、发送消息）应考虑追踪
			// 暂时我们只追踪到广播调用本身
			// 移除未使用的 broadcastCtx
			_, broadcastSpan := tracer.Start(ctx, "broadcastSessionEvent") // 使用从 NATS 继承的 ctx
			ns.hub.Broadcast(targetSessionID, sessionEvent)                // <--- 调用 Hub 的方法
			broadcastSpan.SetAttributes(attribute.String("session.id", targetSessionID))
			broadcastSpan.SetStatus(otelCodes.Ok, "Broadcast initiated")
			broadcastSpan.End()

			span.SetStatus(otelCodes.Ok, "Event broadcasted")
		} else {
			logger.Warn("无法从事件中确定 SessionID 或事件为空 (Realtime)", zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Error, "Missing session ID or event data")
		}
	}
}

// StopNatsSubscription is removed as shutdown relies on context cancellation and defer.

// removeSubscription 辅助函数 (需要在包内定义或移到公共包)
func removeSubscription(subs []*nats.Subscription, sub *nats.Subscription) []*nats.Subscription {
	L := len(subs)
	if L == 0 {
		return subs // 如果已经是空 slice，直接返回
	}
	// 创建一个容量为 L-1 (或 L，如果 L=1) 的新 slice
	// 这样可以避免 len(subs)-1 为负数
	newCap := L - 1
	if newCap < 0 {
		newCap = 0 // 容量不能是负数
	}
	newSubs := make([]*nats.Subscription, 0, newCap)
	for _, s := range subs {
		if s != sub { // 只将不是要移除的元素添加到新 slice
			newSubs = append(newSubs, s)
		}
	}
	return newSubs
}

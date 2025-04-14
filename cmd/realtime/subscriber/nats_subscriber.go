// cmd/realtime/subscriber/nats_subscriber.go
package subscriber

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1"
	// 需要 internal/realtime/service 中的 Hub 定义 (或者将 Hub 移到公共包)
	// 为了避免循环依赖，这里假设 Hub 的 Broadcast 方法被抽象成接口或直接操作 Hub 实例
	realtimeservice "go-xlive/internal/realtime/service"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

// NatsSubscriber handles NATS subscriptions for the Realtime service
type NatsSubscriber struct {
	natsConn *nats.Conn
	hub      *realtimeservice.RealtimeService // 依赖 Hub 进行广播
	logger   *zap.Logger
	wg       *sync.WaitGroup
	cancel   context.CancelFunc
	subs     []*nats.Subscription
	muSubs   sync.Mutex
}

// NewNatsSubscriber creates a new NatsSubscriber
func NewNatsSubscriber(logger *zap.Logger, nc *nats.Conn, hub *realtimeservice.RealtimeService) *NatsSubscriber {
	return &NatsSubscriber{
		natsConn: nc,
		hub:      hub,
		logger:   logger.Named("nats_subscriber"),
		subs:     make([]*nats.Subscription, 0),
	}
}

// Start starts the NATS subscriptions
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.wg = wg
	internalCtx, internalCancel := context.WithCancel(ctx)
	ns.cancel = internalCancel

	// 订阅需要实时推送的事件
	subjectsToSubscribe := []string{
		"events.raw.user.presence",
		"events.raw.chat.message",
		"events.raw.gift.sent",
		// 可以添加其他主题，如 "events.room.*.status.>"
	}
	queueGroupName := "realtime-service-workers" // 队列组
	numSubscribersPerSubject := 2                // 每个主题的订阅者数量

	ns.logger.Info("正在启动 NATS 订阅者 (RealtimeService)...",
		zap.Strings("subjects", subjectsToSubscribe),
		zap.Int("subs_per_subject", numSubscribersPerSubject))

	natsMsgHandler := ns.createNatsMsgHandler()

	ns.muSubs.Lock()
	ns.subs = make([]*nats.Subscription, 0, len(subjectsToSubscribe)*numSubscribersPerSubject)
	for _, subject := range subjectsToSubscribe {
		for i := 0; i < numSubscribersPerSubject; i++ {
			ns.wg.Add(1)
			go func(subj string, workerID int) {
				defer ns.wg.Done()
				sub, err := ns.natsConn.QueueSubscribeSync(subj, queueGroupName)
				if err != nil {
					ns.logger.Error("...", zap.String("subj", subj), zap.Int("id", workerID), zap.Error(err))
					return
				}
				ns.muSubs.Lock()
				ns.subs = append(ns.subs, sub)
				ns.muSubs.Unlock()
				defer func() { /* ... 取消订阅并移除 ... */
					ns.logger.Info("...", zap.Int("id", workerID))
					sub.Unsubscribe()
					ns.muSubs.Lock()
					ns.subs = removeSubscription(ns.subs, sub)
					ns.muSubs.Unlock()
				}()
				ns.logger.Info("NATS 订阅者已启动 (RealtimeService)", zap.String("subject", subj), zap.Int("worker_id", workerID))
				for {
					msg, err := sub.NextMsgWithContext(internalCtx)
					if err != nil { /* ... 错误处理和退出逻辑 ... */
						if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
							ns.logger.Info("...", zap.String("subj", subj), zap.Int("id", workerID), zap.Error(err))
							return
						}
						ns.logger.Error("...", zap.String("subj", subj), zap.Int("id", workerID), zap.Error(err))
						select {
						case <-internalCtx.Done():
							return
						default:
							time.Sleep(1 * time.Second)
							continue
						}
					}
					select {
					case <-internalCtx.Done():
						return
					default:
						func() { /* recover */ natsMsgHandler(msg) }()
					}
				}
			}(subject, i)
		}
	}
	ns.muSubs.Unlock()
}

// Shutdown gracefully stops the NATS subscriptions
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("正在通知 NATS 订阅者退出 (RealtimeService)...")
	if ns.cancel != nil {
		ns.cancel()
	}
	ns.StopNatsSubscription() // 尝试显式取消
}

// createNatsMsgHandler creates the message handler closure
func (ns *NatsSubscriber) createNatsMsgHandler() func(msg *nats.Msg) {
	return func(msg *nats.Msg) {
		tracer := otel.Tracer("nats-realtime-handler")
		_, span := tracer.Start(context.Background(), "handleNatsMessageForRealtime")
		defer span.End()
		span.SetAttributes(attribute.String("nats.subject", msg.Subject))
		ns.logger.Debug("RealtimeService 收到 NATS 消息", zap.String("subject", msg.Subject))

		var sessionEvent *realtimev1.SessionEvent
		var targetSessionID string

		// --- 根据主题和内容解析事件 ---
		// 示例：解析 UserPresence
		var presenceEvent eventv1.UserPresence
		if err := proto.Unmarshal(msg.Data, &presenceEvent); err == nil && strings.Contains(msg.Subject, "user.presence") {
			targetSessionID = presenceEvent.GetSessionId()
			sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_UserPresence{UserPresence: &presenceEvent}}
			span.SetAttributes(attribute.String("event.type", "UserPresence"))
		} else {
			// 示例：解析 ChatMessage
			var chatEvent eventv1.ChatMessage
			if err := proto.Unmarshal(msg.Data, &chatEvent); err == nil && strings.Contains(msg.Subject, "chat.message") {
				targetSessionID = chatEvent.GetSessionId()
				sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_ChatMessage{ChatMessage: &chatEvent}}
				span.SetAttributes(attribute.String("event.type", "ChatMessage"))
			} else {
				// 示例：解析 GiftSent
				var giftEvent eventv1.GiftSent
				if err := proto.Unmarshal(msg.Data, &giftEvent); err == nil && strings.Contains(msg.Subject, "gift.sent") {
					targetSessionID = giftEvent.GetSessionId()
					sessionEvent = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_GiftSent{GiftSent: &giftEvent}}
					span.SetAttributes(attribute.String("event.type", "GiftSent"))
				} else {
					// 忽略其他不关心的事件类型
					ns.logger.Debug("RealtimeService 忽略 NATS 消息", zap.String("subject", msg.Subject))
					span.SetStatus(otelCodes.Ok, "Event type ignored")
					return
				}
			}
		}

		if targetSessionID != "" && sessionEvent != nil {
			span.SetAttributes(attribute.String("target_session_id", targetSessionID))
			ns.hub.BroadcastEvent(targetSessionID, sessionEvent) // 调用 Hub 的广播方法
			span.SetStatus(otelCodes.Ok, "Event broadcasted")
		} else {
			ns.logger.Warn("无法从事件中确定 SessionID 或事件为空", zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Error, "Missing session ID or event data")
		}
	}
}

// StopNatsSubscription explicitly unsubscribes
func (ns *NatsSubscriber) StopNatsSubscription() {
	ns.logger.Info("正在显式取消 RealtimeService NATS 订阅...")
	ns.muSubs.Lock()
	defer ns.muSubs.Unlock()
	for i, sub := range ns.subs {
		if sub != nil && sub.IsValid() {
			ns.logger.Debug("...", zap.Int("idx", i), zap.String("subj", sub.Subject))
			if err := sub.Unsubscribe(); err != nil {
				ns.logger.Warn("...", zap.Error(err))
			}
		}
	}
	ns.subs = nil
	ns.logger.Info("RealtimeService NATS 订阅已尝试取消")
}

// removeSubscription helper function
func removeSubscription(subs []*nats.Subscription, sub *nats.Subscription) []*nats.Subscription {
	newSubs := make([]*nats.Subscription, 0, len(subs)-1)
	for _, s := range subs {
		if s != sub {
			newSubs = append(newSubs, s)
		}
	}
	return newSubs
}

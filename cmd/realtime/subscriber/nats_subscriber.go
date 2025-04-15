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
	cancel context.CancelFunc
	subs   []*nats.Subscription
	muSubs sync.Mutex
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
	}
	queueGroupName := "realtime-service-workers"
	numSubscribersPerSubject := 2

	ns.logger.Info("正在启动 NATS 订阅者 (RealtimeService)..." /* ... */)

	natsMsgHandler := ns.createNatsMsgHandler() // 获取消息处理函数

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
					sub.Unsubscribe() // Drain maybe better?
					ns.muSubs.Lock()
					ns.subs = removeSubscription(ns.subs, sub)
					ns.muSubs.Unlock()
				}()
				ns.logger.Info("NATS 订阅者已启动 (RealtimeService)", zap.String("subject", subj), zap.Int("worker_id", workerID))
				for {
					msg, err := sub.NextMsgWithContext(internalCtx) // 使用 internalCtx
					if err != nil {                                 /* ... 错误处理和退出 ... */
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

// Shutdown (保持不变)
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("...")
	if ns.cancel != nil {
		ns.cancel()
	}
	ns.StopNatsSubscription()
}

// createNatsMsgHandler 创建消息处理闭包 (调用 hub.Broadcast)
func (ns *NatsSubscriber) createNatsMsgHandler() func(msg *nats.Msg) {
	return func(msg *nats.Msg) {
		tracer := otel.Tracer("nats-realtime-handler")
		msgCtx, span := tracer.Start(context.Background(), "handleNatsMessageForRealtime")
		defer span.End()
		span.SetAttributes(attribute.String("nats.subject", msg.Subject))
		ns.logger.Debug("RealtimeService 收到 NATS 消息", zap.String("subject", msg.Subject))

		var sessionEvent *realtimev1.SessionEvent
		var targetSessionID string

		// --- 解析事件 (逻辑不变) ---
		// ... (解析 UserPresence, ChatMessage, GiftSent) ...
		var pres eventv1.UserPresence
		if e := proto.Unmarshal(msg.Data, &pres); e == nil && strings.Contains(msg.Subject, "user.presence") {
			sid := pres.GetSessionId()
			se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_UserPresence{UserPresence: &pres}}
			span.SetAttributes(attribute.String("evt", "UserPresence"))
		} else {
			var chat eventv1.ChatMessage
			if e := proto.Unmarshal(msg.Data, &chat); e == nil && strings.Contains(msg.Subject, "chat.message") {
				sid = chat.GetSessionId()
				se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_ChatMessage{ChatMessage: &chat}}
				span.SetAttributes(attribute.String("evt", "ChatMessage"))
			} else {
				var gift eventv1.GiftSent
				if e := proto.Unmarshal(msg.Data, &gift); e == nil && strings.Contains(msg.Subject, "gift.sent") {
					sid = gift.GetSessionId()
					se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_GiftSent{GiftSent: &gift}}
					span.SetAttributes(attribute.String("evt", "GiftSent"))
				} else {
					ns.logger.Debug("...", zap.String("subj", msg.Subject))
					span.SetStatus(otelCodes.Ok, "...")
					return
				}
			}
		}

		if targetSessionID != "" && sessionEvent != nil {
			span.SetAttributes(attribute.String("target_session_id", targetSessionID))
			// --- !!! 修改: 直接调用 hub 的 Broadcast 方法 !!! ---
			ns.hub.Broadcast(targetSessionID, sessionEvent) // <--- 调用 Hub 的方法
			// --- 修改结束 ---
			span.SetStatus(otelCodes.Ok, "Event broadcasted")
		} else {
			ns.logger.Warn("无法从事件中确定 SessionID 或事件为空 (Realtime)", zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Error, "Missing session ID or event data")
		}
	}
}

// StopNatsSubscription (保持不变)
func (ns *NatsSubscriber) StopNatsSubscription() {
	ns.logger.Info("正在显式取消 RealtimeService NATS 订阅...")
	ns.muSubs.Lock()
	defer ns.muSubs.Unlock()
	for i, sub := range ns.subs {
		if sub != nil && sub.IsValid() {
			ns.logger.Debug("...", zap.Int("idx", i))
			if e := sub.Unsubscribe(); e != nil {
				ns.logger.Warn("...", zap.Error(e))
			}
		}
	}
	ns.subs = nil
	ns.logger.Info("RealtimeService NATS 订阅已尝试取消")
}

// removeSubscription helper (保持不变)
func removeSubscription(subs []*nats.Subscription, sub *nats.Subscription) []*nats.Subscription {
	newSubs := make([]*nats.Subscription, 0, len(subs)-1)
	for _, s := range subs {
		if s != sub {
			newSubs = append(newSubs, s)
		}
	}
	return newSubs
}

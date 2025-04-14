package subscriber

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	aggregationservice "go-xlive/internal/aggregation/service" // 需要导入 aggregation service
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

// NatsSubscriber handles NATS subscriptions for the Aggregation service
type NatsSubscriber struct {
	natsConn *nats.Conn
	aggSvc   *aggregationservice.AggregationService // 聚合服务实例
	logger   *zap.Logger
	wg       *sync.WaitGroup
	cancel   context.CancelFunc   // Context cancel function for shutdown
	subs     []*nats.Subscription // Active subscriptions
	muSubs   sync.Mutex           // Mutex for subscriptions slice
}

// NewNatsSubscriber creates a new NatsSubscriber
func NewNatsSubscriber(logger *zap.Logger, nc *nats.Conn, aggSvc *aggregationservice.AggregationService) *NatsSubscriber {
	return &NatsSubscriber{
		natsConn: nc,
		aggSvc:   aggSvc, // 依赖 AggregationService
		logger:   logger.Named("nats_subscriber"),
		subs:     make([]*nats.Subscription, 0),
	}
}

// Start starts the NATS subscriptions for SessionEnded events
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.wg = wg
	internalCtx, internalCancel := context.WithCancel(ctx)
	ns.cancel = internalCancel

	sessionEndedSubject := "events.session.*.ended" // 只订阅场次结束事件
	queueGroupName := "aggregation-service-workers"
	numSubscribers := runtime.NumCPU() // 可以根据负载调整

	ns.logger.Info("正在启动 NATS 订阅者 (AggregationService)...",
		zap.String("subject", sessionEndedSubject),
		zap.Int("count", numSubscribers),
		zap.String("queue_group", queueGroupName))

	natsMsgHandler := ns.createNatsMsgHandler() // 获取消息处理函数

	ns.muSubs.Lock()
	ns.subs = make([]*nats.Subscription, 0, numSubscribers)
	for i := 0; i < numSubscribers; i++ {
		ns.wg.Add(1)
		go func(workerID int) {
			defer ns.wg.Done()
			sub, err := ns.natsConn.QueueSubscribeSync(sessionEndedSubject, queueGroupName)
			if err != nil {
				ns.logger.Error("NATS 队列订阅失败 (Aggregation)", zap.Int("worker_id", workerID), zap.Error(err))
				return
			}

			ns.muSubs.Lock()
			ns.subs = append(ns.subs, sub)
			ns.muSubs.Unlock()
			defer func() { // 确保退出时取消订阅并清理列表
				ns.logger.Info("正在取消 NATS 订阅 (Aggregation worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
				if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
					ns.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
				}
				ns.muSubs.Lock()
				ns.subs = removeSubscription(ns.subs, sub)
				ns.muSubs.Unlock()
			}()
			ns.logger.Info("NATS 订阅者已启动 (AggregationService)", zap.Int("worker_id", workerID))

			for {
				// 使用 internalCtx 监听退出信号
				msg, err := sub.NextMsgWithContext(internalCtx)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
						ns.logger.Info("NATS 订阅者收到退出信号或连接关闭 (Aggregation)，正在退出...", zap.Int("worker_id", workerID), zap.Error(err))
						return // 正常退出 Goroutine
					}
					ns.logger.Error("NATS NextMsg 出错 (Aggregation)", zap.Int("worker_id", workerID), zap.Error(err))
					// 检查是否需要退出
					select {
					case <-internalCtx.Done():
						ns.logger.Info("NATS 订阅者收到退出信号 (出错后) (Aggregation)，正在退出...", zap.Int("worker_id", workerID))
						return
					default:
						time.Sleep(1 * time.Second) // 短暂暂停后继续尝试
						continue
					}
				}
				// 检查是否需要在处理消息前退出
				select {
				case <-internalCtx.Done():
					ns.logger.Info("NATS 订阅者收到退出信号 (处理消息前) (Aggregation)，正在退出...", zap.Int("worker_id", workerID))
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
	ns.logger.Info("正在通知 NATS 订阅者退出 (AggregationService)...")
	if ns.cancel != nil {
		ns.cancel() // 取消 context，通知所有 goroutine 退出循环
	}
	// StopNatsSubscription(ns) // 调用显式取消 (可选)
}

// createNatsMsgHandler 创建 NATS 消息处理闭包
func (ns *NatsSubscriber) createNatsMsgHandler() func(msg *nats.Msg) {
	return func(msg *nats.Msg) {
		tracer := otel.Tracer("nats-aggregator")
		// 使用后台 Context 作为父级，因为 NATS 消息通常不携带 Trace Context
		msgCtx, span := tracer.Start(context.Background(), "handleSessionEndedEvent")
		defer span.End()
		span.SetAttributes(attribute.String("nats.subject", msg.Subject))

		var sessionEvent eventv1.SessionEnded
		if err := proto.Unmarshal(msg.Data, &sessionEvent); err == nil {
			sessionID := sessionEvent.GetSessionId()
			span.SetAttributes(attribute.String("session_id", sessionID))
			ns.logger.Info("收到场次结束事件，准备聚合", zap.String("session_id", sessionID))

			// 调用 AggregationService 的核心处理方法，传递带有新 Span 的 Context
			_, err := ns.aggSvc.ProcessAggregation(msgCtx, sessionID) // 调用 service 中的方法
			if err != nil {
				ns.logger.Error("处理场次聚合失败", zap.String("session_id", sessionID), zap.Error(err))
				span.RecordError(err)
				span.SetStatus(otelCodes.Error, "Aggregation processing failed")
			} else {
				ns.logger.Info("场次聚合处理完成 (由 NATS 触发)", zap.String("session_id", sessionID))
				span.SetStatus(otelCodes.Ok, "Aggregation processed successfully")
			}
		} else {
			ns.logger.Warn("无法将 NATS 消息解析为 SessionEnded 事件", zap.Error(err), zap.String("subject", msg.Subject))
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to unmarshal NATS message")
		}
	}
}

// StopNatsSubscription 显式取消订阅 (可以被 Shutdown 调用)
func (ns *NatsSubscriber) StopNatsSubscription() {
	ns.logger.Info("正在显式取消 AggregationService NATS 订阅...")
	ns.muSubs.Lock()
	defer ns.muSubs.Unlock()
	for i, sub := range ns.subs {
		if sub != nil && sub.IsValid() {
			ns.logger.Debug("正在取消订阅", zap.Int("index", i), zap.String("subject", sub.Subject))
			if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
				ns.logger.Warn("取消订阅失败", zap.Error(err), zap.String("subject", sub.Subject))
			}
		}
	}
	ns.subs = nil // 清空列表
	ns.logger.Info("AggregationService NATS 订阅已尝试取消")
}

// removeSubscription 辅助函数 (需要在包内定义或移到公共包)
func removeSubscription(subs []*nats.Subscription, sub *nats.Subscription) []*nats.Subscription {
	newSubs := make([]*nats.Subscription, 0, len(subs)-1)
	for _, s := range subs {
		if s != sub {
			newSubs = append(newSubs, s)
		}
	}
	return newSubs
}

package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	realtimev1 "go-xlive/gen/go/realtime/v1" // 导入 realtime proto

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// streamClient 代表一个订阅了事件流的 gRPC 客户端连接
type streamClient struct {
	stream realtimev1.RealtimeService_SubscribeSessionEventsServer // gRPC 服务器流
	done   chan struct{}                                           // 用于通知 goroutine 结束
}

// SubscriptionHub 管理所有活跃的事件流订阅 (导出类型，以便 subscriber 包能使用)
type SubscriptionHub struct {
	// 使用读写锁保护并发访问
	mu            sync.RWMutex
	subscriptions map[string]map[*streamClient]bool // sessionID -> map[streamClient指针] -> bool (使用 map 实现 set)
	logger        *zap.Logger
}

// newSubscriptionHub 创建一个新的 Hub 实例 (保持私有)
func newSubscriptionHub(logger *zap.Logger) *SubscriptionHub { // 返回导出类型
	return &SubscriptionHub{
		subscriptions: make(map[string]map[*streamClient]bool),
		logger:        logger.Named("sub_hub"), // 给 Hub 的 logger 加个名字
	}
}

// addSubscription 添加一个新的订阅者
func (h *SubscriptionHub) addSubscription(sessionID string, client *streamClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscriptions[sessionID]; !ok {
		h.subscriptions[sessionID] = make(map[*streamClient]bool)
	}
	h.subscriptions[sessionID][client] = true
	h.logger.Info("添加事件流订阅", zap.String("session_id", sessionID), zap.Int("current_subscribers", len(h.subscriptions[sessionID])))
}

// removeSubscription 移除一个订阅者
func (h *SubscriptionHub) removeSubscription(sessionID string, client *streamClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if clients, ok := h.subscriptions[sessionID]; ok {
		if _, clientExists := clients[client]; clientExists {
			delete(clients, client)
			// 尝试关闭 done channel (如果还没关闭的话)
			select {
			case <-client.done: // 已关闭
			default:
				close(client.done)
			}
			if len(clients) == 0 {
				delete(h.subscriptions, sessionID) // 如果没有订阅者了，清理 sessionID 条目
				h.logger.Info("移除最后一个事件流订阅", zap.String("session_id", sessionID))
			}
			h.logger.Info("移除事件流订阅", zap.String("session_id", sessionID), zap.Int("remaining_subscribers", len(clients)))
		}
	}
}

// Broadcast 将事件广播给特定 sessionID 的所有订阅者 (导出方法)
func (h *SubscriptionHub) Broadcast(sessionID string, event *realtimev1.SessionEvent) {
	h.mu.RLock() // 使用读锁允许多个广播并发
	subscribers, ok := h.subscriptions[sessionID]
	if !ok || len(subscribers) == 0 {
		h.mu.RUnlock()
		return
	}

	// 复制一份订阅者列表以避免长时间持有读锁
	clientsToSend := make([]*streamClient, 0, len(subscribers))
	for client := range subscribers {
		clientsToSend = append(clientsToSend, client)
	}
	h.mu.RUnlock() // 释放读锁

	h.logger.Debug("准备广播事件",
		zap.String("session_id", sessionID),
		zap.Int("target_count", len(clientsToSend)),
		zap.Any("event_type", fmt.Sprintf("%T", event.GetEvent())),
	)

	// 向复制出来的列表中的客户端发送事件
	for _, client := range clientsToSend {
		// 使用 goroutine 异步发送，避免一个慢客户端阻塞其他客户端
		go func(c *streamClient, ev *realtimev1.SessionEvent) {
			// 为发送操作创建一个带超时的 Context
			sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer sendCancel()

			// 使用 Select 检查 client.done 是否已关闭，避免向已关闭的流发送
			select {
			case <-c.done:
				h.logger.Debug("广播时发现客户端已关闭 (done channel)", zap.String("session_id", sessionID))
				return
			case <-sendCtx.Done():
				h.logger.Warn("广播发送超时", zap.String("session_id", sessionID), zap.Error(sendCtx.Err()))
				h.removeSubscription(sessionID, c) // 超时也移除
				return
			default:
				// 尝试发送
				if err := c.stream.Send(ev); err != nil {
					h.logger.Error("向 gRPC 流发送事件失败，移除订阅", zap.Error(err), zap.String("session_id", sessionID))
					h.removeSubscription(sessionID, c) // 发送失败则移除
				}
			}
		}(client, event)
	}
}

// RealtimeService 结构体定义
type RealtimeService struct {
	realtimev1.UnimplementedRealtimeServiceServer
	logger   *zap.Logger
	natsConn *nats.Conn
	hub      *SubscriptionHub // <--- 类型改为导出的 SubscriptionHub
	natsSubs []*nats.Subscription
	muSubs   sync.Mutex
}

// NewRealtimeService 创建实例
func NewRealtimeService(logger *zap.Logger, nc *nats.Conn) *RealtimeService {
	if logger == nil {
		log.Fatal("RealtimeService 需要 Logger")
	}
	if nc == nil {
		log.Fatal("RealtimeService 需要 NATS Connection")
	}
	return &RealtimeService{
		logger:   logger.Named("realtime_svc"),
		natsConn: nc,
		hub:      newSubscriptionHub(logger), // 初始化 Hub
		natsSubs: make([]*nats.Subscription, 0),
	}
}

// --- 添加回 GetHub 方法，返回导出类型 ---
// GetHub 返回内部的 SubscriptionHub 实例，供 NATS 订阅者使用
func (s *RealtimeService) GetHub() *SubscriptionHub {
	return s.hub
}

// StartNatsSubscription (接收 msgHandler 作为参数)
func (s *RealtimeService) StartNatsSubscription(ctx context.Context, wg *sync.WaitGroup, msgHandler func(*nats.Msg)) {
	subjectsToSubscribe := []string{"events.raw.user.presence", "events.raw.chat.message", "events.raw.gift.sent"}
	group := "realtime-workers"
	num := 2
	s.logger.Info("正在启动 NATS 订阅者 (RealtimeService)...", zap.Strings("subjs", subjectsToSubscribe), zap.Int("num", num))

	s.muSubs.Lock()
	s.natsSubs = make([]*nats.Subscription, 0, len(subjectsToSubscribe)*num)
	for _, subject := range subjectsToSubscribe {
		for i := 0; i < num; i++ {
			wg.Add(1)
			go func(sbj string, id int) {
				defer wg.Done()
				sub, e := s.natsConn.QueueSubscribeSync(sbj, group)
				if e != nil {
					s.logger.Error("NATS 队列订阅失败 (RealtimeService)", zap.String("subject", sbj), zap.Int("worker_id", id), zap.Error(e))
					return
				}
				s.muSubs.Lock()
				s.natsSubs = append(s.natsSubs, sub)
				s.muSubs.Unlock()
				defer func() { /* ... unsubscribe & remove ... */
					s.logger.Info("正在取消 NATS 订阅 (Realtime worker)", zap.Int("worker_id", id), zap.String("subject", sub.Subject))
					if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
						s.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", id), zap.Error(err))
					}
					s.muSubs.Lock()
					newSubs := []*nats.Subscription{}
					for _, ss := range s.natsSubs {
						if ss != sub {
							newSubs = append(newSubs, ss)
						}
					}
					s.natsSubs = newSubs
					s.muSubs.Unlock()
				}()
				s.logger.Info("NATS 订阅者已启动 (RealtimeService)", zap.String("subj", sbj), zap.Int("id", id))
				for {
					msg, e := sub.NextMsgWithContext(ctx)
					if e != nil {
						if errors.Is(e, context.Canceled) || errors.Is(e, nats.ErrConnectionClosed) || errors.Is(e, nats.ErrBadSubscription) {
							s.logger.Info("NATS 订阅者收到退出信号或连接关闭 (RealtimeService)，正在退出...", zap.String("subject", sbj), zap.Int("worker_id", id), zap.Error(e))
							return
						}
						s.logger.Error("NATS NextMsg 出错 (RealtimeService)", zap.String("subject", sbj), zap.Int("worker_id", id), zap.Error(e))
						select {
						case <-ctx.Done():
							s.logger.Info("NATS 订阅者收到退出信号 (出错后) (RealtimeService)，正在退出...", zap.Int("worker_id", id))
							return
						default:
							time.Sleep(1 * time.Second)
							continue
						}
					}
					select {
					case <-ctx.Done():
						return
					default:
						func() { /* recover */ msgHandler(msg) }()
					} // 调用传入的 handler
				}
			}(subject, i)
		}
	}
	s.muSubs.Unlock()
}

// SubscribeSessionEvents (实现不变)
func (s *RealtimeService) SubscribeSessionEvents(req *realtimev1.SubscribeSessionEventsRequest, stream realtimev1.RealtimeService_SubscribeSessionEventsServer) error {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id 不能为空")
	}
	s.logger.Info("收到新的事件流订阅请求", zap.String("session_id", sessionID))
	client := &streamClient{stream: stream, done: make(chan struct{})}
	s.hub.addSubscription(sessionID, client)
	defer s.hub.removeSubscription(sessionID, client)

	// --- (可选) 发送快照 ---
	// s.sendInitialSnapshot(stream.Context(), sessionID, stream)

	ctx := stream.Context()
	for {
		select {
		case <-client.done: // Hub 通知关闭
			s.logger.Info("Hub 通知关闭事件流", zap.String("sid", sessionID))
			return status.Error(codes.Aborted, "服务器端关闭流")
		case <-ctx.Done(): // 客户端断开
			s.logger.Info("客户端断开事件流连接", zap.String("sid", sessionID), zap.Error(ctx.Err()))
			return ctx.Err()
		}
	}
}

// HealthCheck (实现不变)
func (s *RealtimeService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*realtimev1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (RealtimeService)")
	healthStatus := "SERVING"
	var firstError error
	if !s.natsConn.IsConnected() {
		s.logger.Error("NATS 未连接")
		healthStatus = "NOT_SERVING"
		// 对于第一个检查，firstError 肯定是 nil，直接赋值
		firstError = errors.New("NATS not connected")
	}
	if firstError != nil {
		return &realtimev1.HealthCheckResponse{Status: healthStatus}, nil
	}
	return &realtimev1.HealthCheckResponse{Status: healthStatus}, nil
}

// StopNatsSubscription (实现不变)
func (s *RealtimeService) StopNatsSubscription() {
	s.logger.Info("正在取消 RealtimeService NATS 订阅...")
	s.muSubs.Lock()
	defer s.muSubs.Unlock()
	for i, sub := range s.natsSubs {
		if sub != nil && sub.IsValid() {
			s.logger.Debug("正在取消订阅", zap.Int("index", i), zap.String("subject", sub.Subject))
			if e := sub.Unsubscribe(); e != nil && !errors.Is(e, nats.ErrConnectionClosed) && !errors.Is(e, nats.ErrBadSubscription) {
				s.logger.Warn("取消 NATS 订阅时出错", zap.Int("index", i), zap.String("subject", sub.Subject), zap.Error(e))
			}
		}
	}
	s.natsSubs = nil
	s.logger.Info("RealtimeService NATS 订阅已取消")
}

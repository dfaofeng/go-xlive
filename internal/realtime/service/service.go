package service

import (
	"context"
	"errors"
	"github.com/nats-io/nats.go"
	eventv1 "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"log"
	"strings"
	"sync"
	"time"
) // !!! 替换路径 !!!

type streamClient struct {
	stream realtimev1.RealtimeService_SubscribeSessionEventsServer
	done   chan struct{}
}
type subscriptionHub struct {
	mu            sync.RWMutex
	subscriptions map[string]map[*streamClient]bool
	logger        *zap.Logger
}

func newSubscriptionHub(l *zap.Logger) *subscriptionHub {
	return &subscriptionHub{subscriptions: make(map[string]map[*streamClient]bool), logger: l.Named("sub_hub")}
}
func (h *subscriptionHub) addSubscription(sid string, c *streamClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscriptions[sid]; !ok {
		h.subscriptions[sid] = make(map[*streamClient]bool)
	}
	h.subscriptions[sid][c] = true
	h.logger.Info("...", zap.String("sid", sid), zap.Int("subs", len(h.subscriptions[sid])))
}
func (h *subscriptionHub) removeSubscription(sid string, c *streamClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cls, ok := h.subscriptions[sid]; ok {
		if _, ok := cls[c]; ok {
			delete(cls, c)
			close(c.done)
			if len(cls) == 0 {
				delete(h.subscriptions, sid)
				h.logger.Info("...", zap.String("sid", sid))
			}
			h.logger.Info("...", zap.String("sid", sid), zap.Int("rem_subs", len(cls)))
		}
	}
}
func (h *subscriptionHub) broadcast(sid string, evt *realtimev1.SessionEvent) {
	h.mu.RLock()
	subs, ok := h.subscriptions[sid]
	if !ok || len(subs) == 0 {
		h.mu.RUnlock() /*h.logger.Debug("...", zap.String("sid", sid))*/
		return
	}
	cls := make([]*streamClient, 0, len(subs))
	for c := range subs {
		cls = append(cls, c)
	}
	h.mu.RUnlock()
	h.logger.Debug("...", zap.String("sid", sid), zap.Int("targets", len(cls)))
	for _, c := range cls {
		if e := c.stream.Send(evt); e != nil {
			h.logger.Error("...", zap.Error(e), zap.String("sid", sid))
			go h.removeSubscription(sid, c)
		}
	}
} // 异步移除慢客户端

type RealtimeService struct {
	realtimev1.UnimplementedRealtimeServiceServer
	logger   *zap.Logger
	natsConn *nats.Conn
	hub      *subscriptionHub
	natsSubs []*nats.Subscription
}

func NewRealtimeService(l *zap.Logger, nc *nats.Conn) *RealtimeService {
	if l == nil {
		log.Fatal("...")
	}
	if nc == nil {
		log.Fatal("...")
	}
	return &RealtimeService{logger: l, natsConn: nc, hub: newSubscriptionHub(l)}
}

// --- !!! 新增 BroadcastEvent 方法 !!! ---
// BroadcastEvent 是一个导出的方法，供 NATS 订阅者或其他内部逻辑调用，以触发对 Hub 的广播
func (s *RealtimeService) BroadcastEvent(sessionID string, event *realtimev1.SessionEvent) {
	// 直接调用内部未导出的 broadcast 方法
	s.hub.broadcast(sessionID, event)
}

// --- 新增结束 ---
func (s *RealtimeService) StartNatsSubscription(ctx context.Context, wg *sync.WaitGroup) {
	subs := []string{"events.raw.user.presence", "events.raw.chat.message", "events.raw.gift.sent"}
	group := "realtime-service-workers"
	num := 2
	s.logger.Info("...", zap.Strings("subjects", subs), zap.Int("subs_per", num))
	handler := func(m *nats.Msg) {
		tracer := otel.Tracer("nats-realtime")
		msgCtx, span := tracer.Start(context.Background(), "handleNatsMsgRealtime")
		defer span.End()
		span.SetAttributes(attribute.String("subj", m.Subject))
		// --- 【修正】日志记录应该关联 msgCtx (形式上) ---
		s.logger.Debug("RealtimeService 收到 NATS 消息",
			//zap.String("subject", msg.Subject),
			zap.String("trace_id", trace.SpanContextFromContext(msgCtx).TraceID().String()), // 可选
		)
		s.logger.Debug("...", zap.String("subj", m.Subject))
		var se *realtimev1.SessionEvent
		var sid string
		var pres eventv1.UserPresence
		if e := proto.Unmarshal(m.Data, &pres); e == nil && strings.Contains(m.Subject, "user.presence") {
			sid = pres.GetSessionId()
			se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_UserPresence{UserPresence: &pres}}
			span.SetAttributes(attribute.String("evt", "UserPresence"))
		} else {
			var chat eventv1.ChatMessage
			if e := proto.Unmarshal(m.Data, &chat); e == nil && strings.Contains(m.Subject, "chat.message") {
				sid = chat.GetSessionId()
				se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_ChatMessage{ChatMessage: &chat}}
				span.SetAttributes(attribute.String("evt", "ChatMessage"))
			} else {
				var gift eventv1.GiftSent
				if e := proto.Unmarshal(m.Data, &gift); e == nil && strings.Contains(m.Subject, "gift.sent") {
					sid = gift.GetSessionId()
					se = &realtimev1.SessionEvent{Event: &realtimev1.SessionEvent_GiftSent{GiftSent: &gift}}
					span.SetAttributes(attribute.String("evt", "GiftSent"))
				} else {
					s.logger.Warn("...", zap.String("subj", m.Subject), zap.Error(e))
					span.SetStatus(otelCodes.Error, "...")
					return
				}
			}
		}
		if sid != "" && se != nil {
			span.SetAttributes(attribute.String("sid", sid))
			s.hub.broadcast(sid, se)
			span.SetStatus(otelCodes.Ok, "...")
		} else {
			s.logger.Warn("...", zap.String("subj", m.Subject))
			span.SetStatus(otelCodes.Error, "...")
		}
	}
	s.natsSubs = make([]*nats.Subscription, 0, len(subs)*num)
	for _, subject := range subs {
		for i := 0; i < num; i++ {
			wg.Add(1)
			go func(sbj string, id int) {
				defer wg.Done()
				sub, e := s.natsConn.QueueSubscribeSync(sbj, group)
				if e != nil {
					s.logger.Error("...", zap.String("subj", sbj), zap.Int("id", id), zap.Error(e))
					return
				}
				s.natsSubs = append(s.natsSubs, sub)
				defer sub.Unsubscribe()
				s.logger.Info("...", zap.String("subj", sbj), zap.Int("id", id))
				for {
					msg, e := sub.NextMsgWithContext(ctx)
					if e != nil {
						if errors.Is(e, context.Canceled) || errors.Is(e, nats.ErrConnectionClosed) || errors.Is(e, nats.ErrBadSubscription) {
							s.logger.Info("...", zap.String("subj", sbj), zap.Int("id", id), zap.Error(e))
							return
						}
						s.logger.Error("...", zap.String("subj", sbj), zap.Int("id", id), zap.Error(e))
						select {
						case <-ctx.Done():
							s.logger.Info("...", zap.Int("id", id))
							return
						default:
							time.Sleep(1 * time.Second)
							continue
						}
					}
					select {
					case <-ctx.Done():
						s.logger.Info("...", zap.Int("id", id))
						return
					default:
						handler(msg)
					}
				}
			}(subject, i)
		}
	}
}
func (s *RealtimeService) SubscribeSessionEvents(q *realtimev1.SubscribeSessionEventsRequest, stm realtimev1.RealtimeService_SubscribeSessionEventsServer) error {
	sid := q.GetSessionId()
	if sid == "" {
		return status.Error(codes.InvalidArgument, "...")
	}
	s.logger.Info("...", zap.String("sid", sid))
	c := &streamClient{stream: stm, done: make(chan struct{})}
	s.hub.addSubscription(sid, c)
	ctx := stm.Context()
	for {
		select {
		case <-c.done:
			s.logger.Info("...", zap.String("sid", sid))
			return status.Error(codes.Aborted, "...")
		case <-ctx.Done():
			s.logger.Info("...", zap.String("sid", sid), zap.Error(ctx.Err()))
			s.hub.removeSubscription(sid, c)
			return ctx.Err()
		}
	}
}
func (s *RealtimeService) HealthCheck(c context.Context, q *emptypb.Empty) (*realtimev1.HealthCheckResponse, error) {
	s.logger.Debug("...")
	hs := "SERVING"
	var fe error
	if !s.natsConn.IsConnected() {
		s.logger.Error("...")
		hs = "NOT_SERVING"
		if fe == nil {
			fe = errors.New("...")
		}
	}
	if fe != nil {
		return &realtimev1.HealthCheckResponse{Status: hs}, fe
	}
	return &realtimev1.HealthCheckResponse{Status: hs}, nil
}
func (s *RealtimeService) StopNatsSubscription() {
	s.logger.Info("...")
	for _, sub := range s.natsSubs {
		if sub != nil && sub.IsValid() {
			if e := sub.Unsubscribe(); e != nil {
				s.logger.Warn("...", zap.Error(e))
			}
		}
	}
	s.natsSubs = nil
	s.logger.Info("...")
}

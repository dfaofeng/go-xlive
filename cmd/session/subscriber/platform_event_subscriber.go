package subscriber

import (
	"context"
	"encoding/json"
	"go-xlive/pkg/observability"
	"time"

	// sessionv1 "go-xlive/gen/go/session/v1" // No longer needed here
	sessionservice "go-xlive/internal/session/service" // Import the service package

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	// "google.golang.org/grpc/codes" // No longer needed here
	// "google.golang.org/grpc/status" // No longer needed here
)

const (
	platformStreamStatusSubject = "platform.event.stream.status"
	subscriptionQueueGroup      = "session_service_stream_status_processor"
)

// PlatformStreamStatusEventJSON structure (should match the one in service)
type PlatformStreamStatusEventJSON struct {
	RoomID       string `json:"room_id"`
	SessionID    string `json:"session_id,omitempty"`
	IsLive       bool   `json:"is_live"`
	Timestamp    int64  `json:"timestamp"`
	RoomTitle    string `json:"room_title"`
	StreamerName string `json:"streamer_name"`
}

// PlatformEventSubscriber listens for platform stream status events.
type PlatformEventSubscriber struct {
	logger     *zap.Logger
	natsConn   *nats.Conn
	sessionSvc *sessionservice.Service // <-- 修改: 类型为 *sessionservice.Service
}

// NewPlatformEventSubscriber creates a PlatformEventSubscriber instance.
func NewPlatformEventSubscriber(logger *zap.Logger, nc *nats.Conn, svc *sessionservice.Service) *PlatformEventSubscriber { // <-- 修改: 参数类型为 *sessionservice.Service
	if nc == nil {
		panic("NATS connection cannot be nil")
	}
	if svc == nil {
		panic("Session Service cannot be nil") // <-- 修改: 错误消息
	}
	return &PlatformEventSubscriber{
		logger:     logger.Named("platform_event_subscriber"),
		natsConn:   nc,
		sessionSvc: svc,
	}
}

// Start begins subscribing to and processing platform stream status events.
func (s *PlatformEventSubscriber) Start(ctx context.Context) error {
	s.logger.Info("开始订阅平台流状态事件 (JSON)", zap.String("subject", platformStreamStatusSubject))

	msgChan := make(chan *nats.Msg, 64)

	sub, err := s.natsConn.QueueSubscribe(platformStreamStatusSubject, subscriptionQueueGroup, func(msg *nats.Msg) {
		msgChan <- msg
	})
	if err != nil {
		s.logger.Error("订阅 NATS 主题失败", zap.String("subject", platformStreamStatusSubject), zap.Error(err))
		return err
	}
	defer func() {
		s.logger.Info("取消 NATS 订阅...")
		if err := sub.Unsubscribe(); err != nil {
			s.logger.Error("取消 NATS 订阅失败", zap.Error(err))
		}
		close(msgChan)
	}()

	s.logger.Info("成功订阅 NATS 主题", zap.String("subject", platformStreamStatusSubject), zap.String("queue_group", subscriptionQueueGroup))

	for {
		select {
		case msg := <-msgChan:
			// Extract trace context from NATS headers
			natsHeaders := make(map[string]string)
			if msg.Header != nil {
				for k, v := range msg.Header {
					if len(v) > 0 {
						natsHeaders[k] = v[0]
					}
				}
			}
			msgCtx := observability.ExtractNATSHeaders(context.Background(), natsHeaders)

			// Process each message in a goroutine with the extracted context
			go s.handlePlatformStreamStatusEvent(msgCtx, msg) // Pass extracted context
		case <-ctx.Done():
			s.logger.Info("收到外部关闭信号，停止订阅...")
			return ctx.Err()
		}
	}
}

// handlePlatformStreamStatusEvent processes a received PlatformStreamStatus event (JSON format).
// It now simply parses the message and calls the business logic method in the service layer.
func (s *PlatformEventSubscriber) handlePlatformStreamStatusEvent(ctx context.Context, msg *nats.Msg) {
	var event sessionservice.PlatformStreamStatusEventJSON // <-- 使用 service 包中定义的结构体
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		s.logger.Error("解析 PlatformStreamStatus JSON 消息失败",
			zap.Error(err),
			zap.String("subject", msg.Subject),
			zap.Int("data_len", len(msg.Data)),
			zap.ByteString("raw_data", msg.Data),
		)
		return // Cannot parse, discard message
	}

	// Create a context with timeout for processing this specific event
	processCtx, processCancel := context.WithTimeout(ctx, 25*time.Second) // Use the parent context (msgCtx)
	defer processCancel()

	logger := s.logger.With(
		zap.String("internal_room_id", event.RoomID),
		zap.Bool("is_live", event.IsLive),
		zap.String("room_title", event.RoomTitle),
		zap.String("streamer_name", event.StreamerName),
	)

	logger.Info("收到平台流状态更新事件 (JSON)，调用业务逻辑处理...")

	// --- 直接调用业务逻辑层的方法 ---
	// The service layer's HandlePlatformStreamStatusEvent now contains all the logic
	// for checking existing sessions, creating/ending sessions, and notifying aggregation.
	s.sessionSvc.HandlePlatformStreamStatusEvent(processCtx, event) // <-- 调用 Service 的方法

	// --- 移除订阅者中重复的业务逻辑 ---
	// logger.Info("检查当前是否有活跃 Session...") // REMOVED
	// liveSessionResp, getErr := s.sessionSvc.GetLiveSessionByRoomID(...) // REMOVED
	// ... rest of the logic checking isLive, hasActiveSession, calling Create/EndSession ... // REMOVED

	logger.Info("平台流状态事件处理完成") // Log completion
}

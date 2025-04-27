package service

import (

	// fmt removed - unused import
	"context"
	"github.com/tidwall/gjson"
	"log"

	// Import reflect for type name
	// "github.com/google/uuid" // UUID is now generated in ChatMessage proto
	// "github.com/jackc/pgx/v5" // Removed unused import

	eventv1 "go-xlive/gen/go/event/v1"
	"go-xlive/internal/event/repository"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	// "google.golang.org/grpc/codes" // No longer needed for QueryEvents/HealthCheck
	// "google.golang.org/grpc/status" // No longer needed for QueryEvents/HealthCheck
	// "google.golang.org/protobuf/types/known/emptypb" // No longer needed for HealthCheck
	// "google.golang.org/protobuf/types/known/timestamppb" // Timestamps are handled directly
)

// EventService 结构体定义
type EventService struct {
	eventv1.UnimplementedEventServiceServer
	repo   repository.Repository
	logger *zap.Logger
	// natsConn *nats.Conn // Removed: NATS handling moved to subscriber package
	// natsSubs []*nats.Subscription // Removed
	// muSubs   sync.Mutex // Removed
}

// NewEventService 创建 EventService 实例
// --- 移除 nc *nats.Conn 参数 ---
func NewEventService(logger *zap.Logger, repo repository.Repository) *EventService {
	if logger == nil {
		log.Fatal("EventService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("EventService 需要 Repository")
	}
	// --- 移除 NATS 连接检查 ---
	// if nc == nil {
	// 	log.Fatal("EventService 需要 NATS Connection")
	// }
	return &EventService{
		logger: logger.Named("event_service"),
		repo:   repo,
		// --- 移除 NATS 相关字段初始化 ---
		// natsConn: nc,
		// natsSubs: make([]*nats.Subscription, 0),
	}
}

// GetAllSessionEvents 获取指定会话的所有事件
func (s *EventService) GetAllSessionEvents(ctx context.Context, req *eventv1.GetAllSessionEventsRequest) (*eventv1.GetAllSessionEventsResponse, error) {
	s.logger.Info("获取会话事件",
		zap.String("session_id", req.GetSessionId()),
	)
	events, err := s.repo.GetAllSessionEvents(ctx, req.GetSessionId())
	if err != nil {
		s.logger.Error("获取会话事件失败",
			zap.String("session_id", req.GetSessionId()),
			zap.Error(err),
		)
		return nil, err
	}

	// 初始化响应
	response := &eventv1.GetAllSessionEventsResponse{}
	// 收集各类事件
	for _, event := range events {
		switch event.EventType {
		case "chat", "chat_message":
			chatMsg := &eventv1.ChatMessage{
				MessageId:    event.EventID,
				SessionId:    event.SessionID,
				RoomId:       event.RoomID,
				UserId:       event.UserID,
				Username:     event.Username,
				Content:      gjson.GetBytes(event.EventData, "content").String(),
				UserLevel:    int32(gjson.GetBytes(event.EventData, "userlever").Int()),
				GuardLevel:   int32(gjson.GetBytes(event.EventData, "guardlevel").Int()),
				Admin:        gjson.GetBytes(event.EventData, "admin").Bool(),
				MobileVerify: gjson.GetBytes(event.EventData, "mobileverify").Bool(),
				Medal: &eventv1.Medal{
					MedalName: gjson.GetBytes(event.EventData, "medal_name").String(),
					//MedalLevel:    event.MedalLevel,
					//MedalColor:    event.MedalColor,
					MedalUpname: gjson.GetBytes(event.EventData, "medal_upname").String(),
					//MedalUproomid: event.MedalUproomid,
					//MedalUpuid:    event.MedalUpuid,
				},
				Timestamp: timestamppb.New(event.Timestamp.Time),
			}
			response.ChatMessages = append(response.ChatMessages, chatMsg)
			//case "gift":
			//	giftSent := &eventv1.GiftSent{
			//		GiftId:    event.EventID,
			//		SessionId: event.SessionID,
			//		RoomId:    event.RoomID,
			//		UserId:    event.UserID,
			//		Username:  event.Username,
			//		Timestamp: timestamppb.New(event.Timestamp.Time),
			//	}
			//	response.GiftSents = append(response.GiftSents, giftSent)

		}
	}

	// 记录事件统计信息
	chatCount := len(response.ChatMessages)

	s.logger.Info("获取会话事件成功",
		zap.String("session_id", req.GetSessionId()),
		zap.Int("total_events", len(events)),
		zap.Int("chat_messages", chatCount),
	)

	return response, nil
}

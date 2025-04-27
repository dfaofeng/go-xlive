// cmd/event/subscriber/nats_subscriber.go
package subscriber

import (
	"context"
	"errors"
	"fmt"                        // Import fmt for Redis key generation
	"go-xlive/pkg/observability" // <-- 新增: 导入 observability
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8" // Import Redis client
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"                 // <-- 新增: 导入 otel
	"go.opentelemetry.io/otel/attribute"       // <-- 新增: 导入 attribute
	otelCodes "go.opentelemetry.io/otel/codes" // <-- 新增: 导入 otelCodes

	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
	"go-xlive/internal/event/repository" // 需要导入 repository
	db "go-xlive/internal/event/repository/db"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const (
	// Redis key expiration time for live stats
	liveStatsTTL = 24 * time.Hour
)

// NatsSubscriber handles NATS subscriptions for the Event service
type NatsSubscriber struct {
	natsConn    *nats.Conn
	repo        repository.Repository // 依赖 Repository 存储事件
	redisClient *redis.Client         // Add Redis client
	logger      *zap.Logger
	wg          *sync.WaitGroup
	cancel      context.CancelFunc
	subs        []*nats.Subscription
	muSubs      sync.Mutex
}

// NewNatsSubscriber creates a new NatsSubscriber
func NewNatsSubscriber(logger *zap.Logger, nc *nats.Conn, repo repository.Repository, redisClient *redis.Client) *NatsSubscriber {
	if redisClient == nil {
		logger.Fatal("NatsSubscriber requires a Redis client")
	}
	return &NatsSubscriber{
		natsConn:    nc,
		repo:        repo,
		redisClient: redisClient,
		logger:      logger.Named("nats_subscriber"),
		subs:        make([]*nats.Subscription, 0),
	}
}

// getLiveStatsKey generates the Redis key for live session statistics.
func getLiveStatsKey(sessionID string) string {
	return fmt.Sprintf("live:stats:%s", sessionID)
}

// Start starts the NATS subscriptions
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.wg = wg
	internalCtx, internalCancel := context.WithCancel(ctx)
	ns.cancel = internalCancel

	subjectToSubscribe := "events.raw.>" // 订阅所有原始事件
	queueGroupName := "event-service-workers"
	numSubscribers := runtime.NumCPU()

	ns.logger.Info("正在启动 NATS 订阅者 (EventService)...",
		zap.String("subject", subjectToSubscribe),
		zap.Int("count", numSubscribers),
		zap.String("queue_group", queueGroupName))

	natsMsgHandler := ns.createNatsMsgHandler() // 获取消息处理函数

	ns.muSubs.Lock()
	// Initialize subs slice with capacity for the subscribers
	ns.subs = make([]*nats.Subscription, 0, numSubscribers)
	for i := 0; i < numSubscribers; i++ {
		ns.wg.Add(1)
		go func(workerID int) {
			defer ns.wg.Done()
			sub, err := ns.natsConn.QueueSubscribeSync(subjectToSubscribe, queueGroupName)
			if err != nil {
				ns.logger.Error("NATS 队列订阅失败 (EventService)", zap.Int("worker_id", workerID), zap.Error(err))
				return
			}

			ns.muSubs.Lock()
			ns.subs = append(ns.subs, sub)
			ns.muSubs.Unlock()

			defer func() {
				ns.logger.Info("正在取消 NATS 订阅并清理 (Event worker)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))
				if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrBadSubscription) {
					ns.logger.Error("取消 NATS 订阅失败", zap.Int("worker_id", workerID), zap.Error(err))
				}
				ns.muSubs.Lock()
				ns.subs = removeSubscription(ns.subs, sub)
				ns.muSubs.Unlock()
			}()

			ns.logger.Info("NATS 订阅者已启动 (EventService)", zap.Int("worker_id", workerID), zap.String("subject", sub.Subject))

			for { // 订阅循环
				msg, err := sub.NextMsgWithContext(internalCtx)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
						ns.logger.Info("NATS 订阅者收到退出信号或连接关闭 (EventService)，正在退出...", zap.Int("worker_id", workerID), zap.Error(err))
						return
					}
					ns.logger.Error("NATS NextMsg 出错 (EventService)", zap.Int("worker_id", workerID), zap.Error(err))
					select {
					case <-internalCtx.Done():
						ns.logger.Info("NATS 订阅者收到退出信号 (出错后) (EventService)，正在退出...", zap.Int("worker_id", workerID))
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
					// 使用匿名函数确保单个消息处理失败不影响整个循环
					func() {
						natsMsgHandler(msg) // 直接传递消息
					}()
				}
			}
		}(i)
	}
	ns.muSubs.Unlock()
}

// Shutdown gracefully stops the NATS subscriptions
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("正在通知 NATS 订阅者退出 (EventService)...")
	if ns.cancel != nil {
		ns.cancel()
	}
}

// createNatsMsgHandler 创建消息处理闭包
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
		tracer := otel.Tracer("event-service-nats")
		ctx, span := tracer.Start(natsCtx, "handleRawEvent") // <-- 使用 natsCtx
		defer span.End()

		// 添加 NATS 相关属性
		span.SetAttributes(
			attribute.String("nats.subject", msg.Subject),
			attribute.Int("nats.data_len", len(msg.Data)),
		)
		// 添加提取的 Headers 到 Span 属性 (用于调试)
		headerAttributes := make([]attribute.KeyValue, 0, len(headersMap))
		for k, v := range headersMap {
			headerAttributes = append(headerAttributes, attribute.String("nats.header."+k, v))
		}
		span.SetAttributes(headerAttributes...)
		// --- Span 启动结束 ---

		ns.logger.Debug("EventService 收到 NATS 消息",
			zap.String("subject", msg.Subject),
			zap.Int("data_len", len(msg.Data)),
			zap.Any("headers", headersMap), // 记录提取的 Headers
		)

		// 1. 解析主题获取事件类型
		subjectParts := strings.Split(msg.Subject, ".")
		if len(subjectParts) < 3 {
			ns.logger.Warn("无效的 NATS 主题格式，无法确定事件类型", zap.String("subject", msg.Subject))
			span.SetStatus(otelCodes.Error, "Invalid NATS subject format")
			return
		}
		eventTypeKey := strings.Join(subjectParts[2:], ".")
		span.SetAttributes(attribute.String("event.type_key", eventTypeKey))

		// 2. 根据事件类型反序列化
		var parsedEvent proto.Message
		var err error
		var sessionID string // Session ID might be empty for non-live events
		var roomID string    // Room ID should always be present
		var eventTime time.Time
		isDuringLiveSession := true // Default to true, specific events will override

		switch eventTypeKey {
		case "chat.message":
			var chat eventv1.ChatMessage
			err = proto.Unmarshal(msg.Data, &chat)
			if err == nil && chat.GetTimestamp().IsValid() {
				parsedEvent = &chat
				sessionID = chat.GetSessionId()
				roomID = chat.GetRoomId() // Get RoomID
				eventTime = chat.GetTimestamp().AsTime()
				isDuringLiveSession = chat.GetIsDuringLiveSession()
			}
		case "user.presence":
			var presence eventv1.UserPresence
			err = proto.Unmarshal(msg.Data, &presence)
			if err == nil && presence.GetTimestamp().IsValid() {
				parsedEvent = &presence
				sessionID = presence.GetSessionId()
				roomID = presence.GetRoomId() // Get RoomID
				eventTime = presence.GetTimestamp().AsTime()
				isDuringLiveSession = presence.GetIsDuringLiveSession()
			}
		case "stream.status":
			var status eventv1.StreamStatus
			err = proto.Unmarshal(msg.Data, &status)
			if err == nil && status.GetTimestamp().IsValid() {
				parsedEvent = &status
				sessionID = status.GetSessionId()
				roomID = status.GetRoomId() // Get RoomID
				eventTime = status.GetTimestamp().AsTime()
				// StreamStatus inherently defines live state, always store in live table
				isDuringLiveSession = true
			}
		case "gift.sent":
			var gift eventv1.GiftSent
			err = proto.Unmarshal(msg.Data, &gift)
			if err == nil && gift.GetTimestamp().IsValid() {
				parsedEvent = &gift
				sessionID = gift.GetSessionId()
				roomID = gift.GetRoomId() // Get RoomID
				eventTime = gift.GetTimestamp().AsTime()
				isDuringLiveSession = gift.GetIsDuringLiveSession()
			}
		case "guard.purchase":
			var guard eventv1.GuardPurchase
			err = proto.Unmarshal(msg.Data, &guard)
			if err == nil && guard.GetTimestamp().IsValid() {
				parsedEvent = &guard
				sessionID = guard.GetSessionId()
				roomID = guard.GetRoomId() // Get RoomID
				eventTime = guard.GetTimestamp().AsTime()
				isDuringLiveSession = guard.GetIsDuringLiveSession()
			}
		case "superchat.message":
			var sc eventv1.SuperChatMessage
			err = proto.Unmarshal(msg.Data, &sc)
			if err == nil && sc.GetTimestamp().IsValid() {
				parsedEvent = &sc
				sessionID = sc.GetSessionId()
				roomID = sc.GetRoomId() // Get RoomID
				eventTime = sc.GetTimestamp().AsTime()
				isDuringLiveSession = sc.GetIsDuringLiveSession()
			}
		case "watched.update":
			var watched eventv1.WatchedCountUpdate
			err = proto.Unmarshal(msg.Data, &watched)
			if err == nil && watched.GetTimestamp().IsValid() {
				parsedEvent = &watched
				sessionID = watched.GetSessionId()
				roomID = watched.GetRoomId() // Get RoomID
				eventTime = watched.GetTimestamp().AsTime()
				isDuringLiveSession = watched.GetIsDuringLiveSession()
			}
		case "like.update":
			var like eventv1.LikeCountUpdate
			err = proto.Unmarshal(msg.Data, &like)
			if err == nil && like.GetTimestamp().IsValid() {
				parsedEvent = &like
				sessionID = like.GetSessionId()
				roomID = like.GetRoomId() // Get RoomID
				eventTime = like.GetTimestamp().AsTime()
				isDuringLiveSession = like.GetIsDuringLiveSession()
			}
		case "rank.update":
			var rank eventv1.OnlineRankUpdate
			err = proto.Unmarshal(msg.Data, &rank)
			if err == nil && rank.GetTimestamp().IsValid() {
				parsedEvent = &rank
				sessionID = rank.GetSessionId()
				roomID = rank.GetRoomId() // Get RoomID
				eventTime = rank.GetTimestamp().AsTime()
				isDuringLiveSession = rank.GetIsDuringLiveSession()
			}
		case "user.interaction":
			var interaction eventv1.UserInteraction
			err = proto.Unmarshal(msg.Data, &interaction)
			if err == nil && interaction.GetTimestamp().IsValid() {
				parsedEvent = &interaction
				sessionID = interaction.GetSessionId()
				roomID = interaction.GetRoomId() // Get RoomID
				eventTime = interaction.GetTimestamp().AsTime()
				isDuringLiveSession = interaction.GetIsDuringLiveSession()
			}
		// case "rank.count": // This case is likely obsolete now
		// 	var rank eventv1.OnlineRankCount
		// 	err = proto.Unmarshal(msg.Data, &rank)
		// 	if err == nil && rank.GetCount() > 0 {
		// 		parsedEvent = &rank
		// 		// Need RoomID and IsDuringLiveSession for this event if kept
		// 	}
		case "online_rank_count.updated":
			var onlineRank eventv1.OnlineRankCountUpdatedEvent
			err = proto.Unmarshal(msg.Data, &onlineRank)
			if err == nil && onlineRank.GetTimestamp().IsValid() {
				parsedEvent = &onlineRank
				sessionID = onlineRank.GetSessionId()
				roomID = onlineRank.GetRoomId() // Get RoomID
				eventTime = onlineRank.GetTimestamp().AsTime()
				isDuringLiveSession = onlineRank.GetIsDuringLiveSession()
			}
		default:
			ns.logger.Debug("收到未配置处理的 NATS 事件类型", zap.String("subject", msg.Subject), zap.String("event_type_key", eventTypeKey))
			span.SetStatus(otelCodes.Unset, "Unhandled event type")
			return
		}

		if err != nil || parsedEvent == nil || eventTime.IsZero() || roomID == "" { // Ensure RoomID is present
			ns.logger.Error("无法通过 Protobuf 解析 NATS 消息、时间戳无效或 RoomID 为空",
				zap.String("subject", msg.Subject),
				zap.String("event_type_key", eventTypeKey),
				zap.String("room_id", roomID),
				zap.Error(err))
			span.RecordError(err)
			span.SetStatus(otelCodes.Error, "Failed to unmarshal protobuf, invalid timestamp, or missing RoomID")
			return
		}

		// 添加 Session ID, Room ID 和直播状态到 Span
		if sessionID != "" {
			span.SetAttributes(attribute.String("session.id", sessionID))
		}
		span.SetAttributes(
			attribute.String("room.id", roomID),
			attribute.Bool("event.is_during_live_session", isDuringLiveSession),
		)

		// 3. 存储结构化事件 (根据 isDuringLiveSession 决定存储位置)
		var storeErr error
		processedStructured := true
		storageTarget := "live" // Default target
		if !isDuringLiveSession {
			storageTarget = "non-live"
		}
		span.SetAttributes(attribute.String("db.storage_target", storageTarget))

		switch event := parsedEvent.(type) {
		case *eventv1.ChatMessage:
			if isDuringLiveSession {
				params := db.InsertChatMessageParams{
					MessageID:     event.GetMessageId(),
					SessionID:     sessionID, // Should be valid here
					RoomID:        roomID,
					UserID:        event.GetUserId(),
					Username:      event.GetUsername(),
					Content:       event.GetContent(),
					Guardlevel:    event.GuardLevel,
					Userlever:     event.UserLevel,
					Mobileverify:  event.MobileVerify,
					Admin:         event.Admin,
					MedalName:     event.Medal.MedalName,
					MedalLevel:    pgtype.Int4{Int32: event.Medal.MedalLevel, Valid: true},
					MedalColor:    pgtype.Int4{Int32: event.Medal.MedalColor, Valid: true},
					MedalUpname:   event.Medal.MedalUpname,
					MedalUproomid: pgtype.Int4{Int32: event.Medal.MedalUproomid, Valid: true},
					MedalUpuid:    pgtype.Int4{Int32: event.Medal.MedalUpuid, Valid: true},
					Timestamp:     pgtype.Timestamptz{Time: eventTime, Valid: true},
					RawMsg:        event.GetRawMsg(),
				}
				storeErr = ns.repo.InsertChatMessage(ctx, params)
			} else {
				params := db.InsertNonLiveChatMessageParams{
					MessageID:     event.GetMessageId(),
					RoomID:        roomID, // Use RoomID
					UserID:        event.GetUserId(),
					Username:      event.GetUsername(),
					Content:       event.GetContent(),
					Guardlevel:    event.GuardLevel,
					Userlever:     event.UserLevel,
					Mobileverify:  event.MobileVerify,
					Admin:         event.Admin,
					MedalName:     event.Medal.MedalName,
					MedalLevel:    pgtype.Int4{Int32: event.Medal.MedalLevel, Valid: true},
					MedalColor:    pgtype.Int4{Int32: event.Medal.MedalColor, Valid: true},
					MedalUpname:   event.Medal.MedalUpname,
					MedalUproomid: pgtype.Int4{Int32: event.Medal.MedalUproomid, Valid: true},
					MedalUpuid:    pgtype.Int4{Int32: event.Medal.MedalUpuid, Valid: true},
					Timestamp:     pgtype.Timestamptz{Time: eventTime, Valid: true},
					RawMsg:        event.GetRawMsg(),
				}
				storeErr = ns.repo.InsertNonLiveChatMessage(ctx, params)
			}
		case *eventv1.GiftSent:
			if isDuringLiveSession {
				params := db.InsertGiftSentParams{
					EventID:   event.GetEventId(),
					SessionID: sessionID,
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					GiftID:    event.GetGiftId(),
					GiftName:  event.GetGiftName(),
					GiftCount: event.GetGiftCount(),
					TotalCoin: event.GetTotalCoin(),
					CoinType:  event.GetCoinType(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertGiftSent(ctx, params)
			} else {
				params := db.InsertNonLiveGiftSentParams{
					EventID:   event.GetEventId(),
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					GiftID:    event.GetGiftId(),
					GiftName:  event.GetGiftName(),
					GiftCount: event.GetGiftCount(),
					TotalCoin: event.GetTotalCoin(),
					CoinType:  event.GetCoinType(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveGiftSent(ctx, params)
			}
		case *eventv1.GuardPurchase:
			if isDuringLiveSession {
				params := db.InsertGuardPurchaseParams{
					EventID:    event.GetEventId(),
					SessionID:  sessionID,
					RoomID:     roomID,
					UserID:     event.GetUserId(),
					Username:   event.GetUsername(),
					GuardLevel: event.GetGuardLevel(),
					GuardName:  event.GetGuardName(),
					Count:      event.GetCount(),
					Price:      event.GetPrice(),
					Timestamp:  pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertGuardPurchase(ctx, params)
			} else {
				params := db.InsertNonLiveGuardPurchaseParams{
					EventID:    event.GetEventId(),
					RoomID:     roomID,
					UserID:     event.GetUserId(),
					Username:   event.GetUsername(),
					GuardLevel: event.GetGuardLevel(),
					GuardName:  event.GetGuardName(),
					Count:      event.GetCount(),
					Price:      event.GetPrice(),
					Timestamp:  pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveGuardPurchase(ctx, params)
			}
		case *eventv1.SuperChatMessage:
			if isDuringLiveSession {
				params := db.InsertSuperChatMessageParams{
					MessageID: event.GetMessageId(),
					SessionID: sessionID,
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					Content:   event.GetContent(),
					Price:     event.GetPrice(),
					Duration:  event.GetDuration(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertSuperChatMessage(ctx, params)
			} else {
				params := db.InsertNonLiveSuperChatMessageParams{
					MessageID: event.GetMessageId(),
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					Content:   event.GetContent(),
					Price:     event.GetPrice(),
					Duration:  event.GetDuration(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveSuperChatMessage(ctx, params)
			}
		case *eventv1.UserInteraction:
			// Only store FOLLOW interactions for now
			if event.GetType() == eventv1.UserInteraction_FOLLOW {
				if isDuringLiveSession {
					params := db.InsertUserInteractionParams{
						EventID:         event.GetEventId(),
						SessionID:       sessionID,
						RoomID:          roomID,
						UserID:          event.GetUserId(),
						Username:        event.GetUsername(),
						InteractionType: int32(event.GetType()),
						Timestamp:       pgtype.Timestamptz{Time: eventTime, Valid: true},
					}
					storeErr = ns.repo.InsertUserInteraction(ctx, params)
				} else {
					params := db.InsertNonLiveUserInteractionParams{
						EventID:         event.GetEventId(),
						RoomID:          roomID,
						UserID:          event.GetUserId(),
						Username:        event.GetUsername(),
						InteractionType: int32(event.GetType()),
						Timestamp:       pgtype.Timestamptz{Time: eventTime, Valid: true},
					}
					storeErr = ns.repo.InsertNonLiveUserInteraction(ctx, params)
				}
			} else {
				processedStructured = false // Skip non-follow interactions
			}
		case *eventv1.UserPresence:
			if isDuringLiveSession {
				params := db.InsertUserPresenceParams{
					SessionID: sessionID,
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					Entered:   event.GetEntered(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertUserPresence(ctx, params)
			} else {
				params := db.InsertNonLiveUserPresenceParams{
					RoomID:    roomID,
					UserID:    event.GetUserId(),
					Username:  event.GetUsername(),
					Entered:   event.GetEntered(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveUserPresence(ctx, params)
			}
		case *eventv1.StreamStatus:
			// StreamStatus always relates to a live session
			params := db.InsertStreamStatusParams{
				SessionID: sessionID, // Should always be valid for StreamStatus
				RoomID:    roomID,
				IsLive:    event.GetIsLive(),
				Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
			}
			storeErr = ns.repo.InsertStreamStatus(ctx, params)
		case *eventv1.WatchedCountUpdate:
			if isDuringLiveSession {
				params := db.InsertWatchedCountUpdateParams{
					SessionID: sessionID,
					RoomID:    roomID,
					Count:     event.GetCount(),
					TextLarge: pgtype.Text{String: event.GetTextLarge(), Valid: event.GetTextLarge() != ""},
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertWatchedCountUpdate(ctx, params)
			} else {
				params := db.InsertNonLiveWatchedCountUpdateParams{
					RoomID:    roomID,
					Count:     event.GetCount(),
					TextLarge: pgtype.Text{String: event.GetTextLarge(), Valid: event.GetTextLarge() != ""},
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveWatchedCountUpdate(ctx, params)
			}
		case *eventv1.LikeCountUpdate:
			if isDuringLiveSession {
				params := db.InsertLikeCountUpdateParams{
					SessionID: sessionID,
					RoomID:    roomID,
					Count:     event.GetCount(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertLikeCountUpdate(ctx, params)
			} else {
				params := db.InsertNonLiveLikeCountUpdateParams{
					RoomID:    roomID,
					Count:     event.GetCount(),
					Timestamp: pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveLikeCountUpdate(ctx, params)
			}
		case *eventv1.OnlineRankUpdate:
			if isDuringLiveSession {
				repoUsers := make([]repository.OnlineRankUser, 0, len(event.GetTopUsers()))
				for _, u := range event.GetTopUsers() {
					repoUsers = append(repoUsers, repository.OnlineRankUser{
						UserID:   u.GetUserId(),
						Username: u.GetUsername(),
						Rank:     u.GetRank(),
						Score:    u.GetScore(),
						FaceURL:  u.GetFaceUrl(),
					})
				}
				updateParams := db.InsertOnlineRankUpdateParams{
					SessionID:  sessionID,
					RoomID:     roomID,
					TotalCount: pgtype.Int8{Valid: false}, // Assuming total_count is nullable
					Timestamp:  pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertOnlineRankUpdate(ctx, updateParams, repoUsers)
			} else {
				repoUsers := make([]repository.NonLiveOnlineRankUser, 0, len(event.GetTopUsers()))
				for _, u := range event.GetTopUsers() {
					repoUsers = append(repoUsers, repository.NonLiveOnlineRankUser{
						UserID:   u.GetUserId(),
						Username: u.GetUsername(),
						Rank:     u.GetRank(),
						Score:    u.GetScore(),
						FaceURL:  u.GetFaceUrl(),
					})
				}
				updateParams := db.InsertNonLiveOnlineRankUpdateParams{
					RoomID:     roomID,
					TotalCount: pgtype.Int8{Valid: false}, // Assuming total_count is nullable
					Timestamp:  pgtype.Timestamptz{Time: eventTime, Valid: true},
				}
				storeErr = ns.repo.InsertNonLiveOnlineRankUpdate(ctx, updateParams, repoUsers)
			}
		case *eventv1.OnlineRankCountUpdatedEvent:
			// This event only updates Redis, no DB storage needed here.
			processedStructured = false
		default:
			processedStructured = false
			ns.logger.Debug("未处理的 Protobuf 事件类型，无法存入结构化表", zap.String("event_type", eventTypeKey))
		}

		if processedStructured && storeErr != nil {
			ns.logger.Error("存储结构化事件到 PostgreSQL 失败",
				zap.String("target", storageTarget),
				zap.String("session_id", sessionID), // Log sessionID even if target is non-live for context
				zap.String("room_id", roomID),
				zap.String("event_type", eventTypeKey),
				zap.Error(storeErr))
			span.RecordError(storeErr)
			span.SetStatus(otelCodes.Error, fmt.Sprintf("Failed to store structured event in DB (%s)", storageTarget))
			// Decide if we should proceed to Redis update on DB error
		} else if processedStructured {
			ns.logger.Debug("结构化事件存储到 PostgreSQL 成功",
				zap.String("target", storageTarget),
				zap.String("session_id", sessionID),
				zap.String("room_id", roomID),
				zap.String("event_type", eventTypeKey))
		}

		// 4. 更新 Redis 计数 (使用新的 ctx) - 仅当在直播期间且 sessionID 有效时
		if isDuringLiveSession && sessionID != "" {
			redisKey := getLiveStatsKey(sessionID)
			var redisErr error
			processedInRedis := true // Flag to track if Redis logic was attempted

			switch event := parsedEvent.(type) {
			case *eventv1.ChatMessage:
				_, redisErr = ns.redisClient.HIncrBy(ctx, redisKey, "danmaku", 1).Result()
			case *eventv1.LikeCountUpdate:
				redisErr = ns.redisClient.HSet(ctx, redisKey, "likes", event.GetCount()).Err()
			case *eventv1.WatchedCountUpdate:
				redisErr = ns.redisClient.HSet(ctx, redisKey, "watched", event.GetCount()).Err()
			case *eventv1.GiftSent: // <-- 新增: 处理 GiftSent 事件
				giftValue := event.GetTotalCoin() // 获取礼物价值
				if giftValue > 0 {
					_, redisErr = ns.redisClient.HIncrBy(ctx, redisKey, "gift_value", giftValue).Result()
				} else {
					processedInRedis = false // Don't process gifts with 0 value in Redis
				}
			case *eventv1.OnlineRankCountUpdatedEvent: // Handle the specific event for Redis
				redisErr = ns.redisClient.HSet(ctx, redisKey, "online_rank_count", event.GetCount()).Err()
			default:
				processedInRedis = false
			}

			// Set TTL only if an operation was performed and successful
			if processedInRedis && redisErr == nil {
				ttlErr := ns.redisClient.Expire(ctx, redisKey, liveStatsTTL).Err()
				if ttlErr != nil {
					ns.logger.Warn("设置 Redis TTL 失败", zap.String("redis_key", redisKey), zap.Error(ttlErr))
					span.RecordError(ttlErr) // Record TTL error but don't mark span as failed
				}
			}

			if processedInRedis && redisErr != nil {
				ns.logger.Error("更新 Redis 实时统计失败",
					zap.String("session_id", sessionID),
					zap.String("redis_key", redisKey),
					zap.String("event_type", eventTypeKey),
					zap.Error(redisErr))
				span.RecordError(redisErr)
				// Don't set span status to Error here, as DB operation might have succeeded
			} else if processedInRedis {
				ns.logger.Debug("更新 Redis 实时统计成功",
					zap.String("session_id", sessionID),
					zap.String("redis_key", redisKey),
					zap.String("event_type", eventTypeKey))
			}
		} else if sessionID != "" { // Log if Redis update was skipped
			ns.logger.Debug("跳过 Redis 更新，因为事件不在活跃会话期间或 SessionID 为空",
				zap.String("session_id", sessionID),
				zap.String("room_id", roomID),
				zap.String("event_type", eventTypeKey),
				zap.Bool("is_during_live", isDuringLiveSession),
			)
			span.SetAttributes(attribute.Bool("redis.update_skipped", true))
		}

		// Set final span status based on whether critical operations succeeded
		if storeErr != nil {
			// DB error is considered critical
			span.SetStatus(otelCodes.Error, fmt.Sprintf("Event processing failed (DB error - %s)", storageTarget))
		} else {
			span.SetStatus(otelCodes.Ok, "Event processed successfully")
		}
	}
}

// removeSubscription 辅助函数 (与 realtime/subscriber 中的相同)
func removeSubscription(subs []*nats.Subscription, subToRemove *nats.Subscription) []*nats.Subscription {
	L := len(subs)
	if L == 0 {
		return subs
	}
	newCap := L - 1
	if newCap < 0 {
		newCap = 0
	}
	newSubs := make([]*nats.Subscription, 0, newCap)
	for _, s := range subs {
		if s != subToRemove {
			newSubs = append(newSubs, s)
		}
	}
	return newSubs
}

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http" // <-- 新增: 导入 net/http
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	// OpenTelemetry HTTP instrumentation
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp" // <-- 新增: 导入 otelhttp

	roomv1 "go-xlive/gen/go/room/v1"
	userv1 "go-xlive/gen/go/user/v1"
)

// Bilibili WebSocket 协议常量
const (
	bilibiliPlatform                   = "bilibili"
	natsRoomSubjectPrefix              = "rooms.event."
	natsRoomSubject                    = "rooms.event.*"
	NatsRoomDeletedSubject             = "rooms.event.deleted" // <-- 新增: Room 删除事件主题
	NatsPlatformRoomInfoUpdatedSubject = "platform.event.room.info.updated"
	NatsSessionUpdatedSubject          = "platform.event.session.updated" // <-- 新增: Session 更新主题
	// Bilibili API Endpoint
	bilibiliRoomInfoAPI = "https://api.live.bilibili.com/room/v1/Room/get_info?room_id=%d" // RESTORED: Constant is actually used
)

// SessionStatusUpdateEventJSON 定义 NATS 会话状态更新事件消息结构 (JSON)
// 期望由 session 服务发布
type SessionStatusUpdateEventJSON struct {
	RoomID    string `json:"room_id"` // Internal Room ID
	SessionID string `json:"session_id"`
	IsLive    bool   `json:"is_live"` // true for created/active, false for ended
	Timestamp int64  `json:"timestamp"`
}

// BilibiliAdapterService 结构体
type BilibiliAdapterService struct {
	logger   *zap.Logger
	natsConn *nats.Conn
	// sessionClient sessionv1.SessionServiceClient // REMOVED: No longer calling session service via gRPC
	roomClient  roomv1.RoomServiceClient
	userClient  userv1.UserServiceClient
	listenerMgr *listenerManager
	httpClient  *http.Client  // <-- 新增: HTTP 客户端
	redisClient *redis.Client // <-- 新增: Redis 客户端

	platformToInternalRoomID map[int]string
	roomMapMutex             sync.RWMutex
	sessionIDCache           map[string]string // key: internalRoomID, value: active sessionID
	sessionIDCacheMutex      sync.RWMutex      // Mutex for sessionIDCache
}
type RoomEventData struct {
	RoomID         string `json:"room_id"`
	Platform       string `json:"platform"`
	PlatformRoomID string `json:"platform_room_id"`
	Status         string `json:"status"`
	OwnerUserID    string `json:"owner_user_id"`
	EventType      string `json:"event_type"` // "created", "updated", "deleted"
	Timestamp      int64  `json:"timestamp"`
}

// NewBilibiliAdapterService 创建 BilibiliAdapterService 实例
// REMOVED: sc sessionv1.SessionServiceClient parameter
func NewBilibiliAdapterService(logger *zap.Logger, nc *nats.Conn, rc roomv1.RoomServiceClient, uc userv1.UserServiceClient) *BilibiliAdapterService {
	if rc == nil {
		panic("BilibiliAdapterService requires a non-nil RoomServiceClient")
	}
	if uc == nil {
		panic("BilibiliAdapterService requires a non-nil UserServiceClient")
	}

	// --- 新增: 初始化带 OTel 的 HTTP Client ---
	otelTransport := otelhttp.NewTransport(http.DefaultTransport)
	httpClient := &http.Client{
		Transport: otelTransport,
		Timeout:   10 * time.Second, // 设置合理的超时
	}
	// --- 初始化结束 ---

	adapter := &BilibiliAdapterService{
		logger:   logger.Named("adapter_bilibili_service"),
		natsConn: nc,
		// sessionClient:            sc, // REMOVED
		roomClient:               rc,
		userClient:               uc,
		httpClient:               httpClient, // <-- 存储 HTTP 客户端
		platformToInternalRoomID: make(map[int]string),
		sessionIDCache:           make(map[string]string), // Initialize the cache
	}
	adapter.listenerMgr = newListenerManager(adapter)

	// --- 新增: 订阅 Session 更新事件 ---
	if err := adapter.subscribeToSessionUpdates(); err != nil {
		// 在实际应用中，这里可能需要更健壮的错误处理，例如重试或 panic
		adapter.logger.Fatal("无法订阅 NATS Session 更新事件", zap.Error(err))
	}
	// --- Session 更新订阅结束 ---

	return adapter
}

// subscribeToSessionUpdates 订阅 NATS 主题以接收会话状态更新
func (s *BilibiliAdapterService) subscribeToSessionUpdates() error {
	_, err := s.natsConn.Subscribe(NatsSessionUpdatedSubject, func(msg *nats.Msg) {
		// 在 goroutine 中处理，避免阻塞 NATS 客户端的回调
		go s.handleSessionUpdate(msg)
	})
	if err != nil {
		s.logger.Error("订阅 NATS Session 更新主题失败", zap.String("subject", NatsSessionUpdatedSubject), zap.Error(err))
		return fmt.Errorf("subscribing to %s failed: %w", NatsSessionUpdatedSubject, err)
	}
	s.logger.Info("成功订阅 NATS Session 更新主题", zap.String("subject", NatsSessionUpdatedSubject))
	return nil
}

// handleSessionUpdate 处理来自 NATS 的会话状态更新消息
func (s *BilibiliAdapterService) handleSessionUpdate(msg *nats.Msg) {
	s.logger.Debug("收到 NATS Session 更新消息", zap.String("subject", msg.Subject))
	var event SessionStatusUpdateEventJSON
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		s.logger.Error("解析 NATS Session 更新事件 JSON 失败", zap.Error(err), zap.String("subject", msg.Subject))
		return
	}

	if event.RoomID == "" {
		s.logger.Warn("收到的 Session 更新事件缺少 RoomID", zap.Any("event", event))
		return
	}

	s.sessionIDCacheMutex.Lock()
	defer s.sessionIDCacheMutex.Unlock()

	if event.IsLive && event.SessionID != "" {
		// 会话开始或更新
		s.sessionIDCache[event.RoomID] = event.SessionID
		s.logger.Info("更新本地 SessionID 缓存 (会话开始/活跃)",
			zap.String("internal_room_id", event.RoomID),
			zap.String("session_id", event.SessionID),
		)
	} else {
		// 会话结束
		delete(s.sessionIDCache, event.RoomID)
		s.logger.Info("从本地 SessionID 缓存移除 (会话结束)",
			zap.String("internal_room_id", event.RoomID),
		)
	}
}

// StartManagingListeners 启动适配器的主管理循环
func (s *BilibiliAdapterService) StartManagingListeners(ctx context.Context) error {
	s.logger.Info("开始管理 Bilibili 监听器...")
	// 1. 初始加载活跃房间
	s.logger.Info("正在从 Room Service 加载初始活跃房间列表...")
	listCtx, listCancel := context.WithTimeout(ctx, 15*time.Second)
	listReq := &roomv1.ListRoomsRequest{
		PlatformFilter: bilibiliPlatform,
		StatusFilter:   "active",
	}
	listResp, err := s.roomClient.ListRooms(listCtx, listReq)
	listCancel()
	if err != nil {
		s.logger.Error("从 Room Service 加载初始房间列表失败", zap.Error(err))
		return fmt.Errorf("loading initial rooms failed: %w", err)
	}

	initialListenCount := 0
	if listResp != nil {
		s.roomMapMutex.Lock()
		for _, room := range listResp.Rooms {
			if room.Platform == bilibiliPlatform && room.PlatformRoomId != "" {
				biliRoomID, parseErr := strconv.Atoi(room.PlatformRoomId)
				if parseErr != nil {
					s.logger.Error("无法解析初始列表中的 Bilibili 房间 ID",
						zap.String("platform_room_id", room.PlatformRoomId),
						zap.String("internal_room_id", room.RoomId),
						zap.Error(parseErr),
					)
					continue
				}
				s.platformToInternalRoomID[biliRoomID] = room.RoomId
				s.listenerMgr.startListener(biliRoomID)
				initialListenCount++
			}
		}
		s.roomMapMutex.Unlock()
	}
	s.logger.Info("初始房间加载完成", zap.Int("started_listeners", initialListenCount))

	// 2. 订阅 NATS 房间事件 (包括创建、更新、删除)
	msgChan := make(chan *nats.Msg, 64) // Channel for all room events
	// 订阅通配符主题，接收 created 和 updated 事件
	subWildcard, err := s.natsConn.ChanSubscribe(natsRoomSubject, msgChan)
	if err != nil {
		s.logger.Error("订阅 NATS 房间通配符事件失败", zap.String("subject", natsRoomSubject), zap.Error(err))
		s.listenerMgr.stopAllListeners()
		return fmt.Errorf("subscribing to NATS subject %s failed: %w", natsRoomSubject, err)
	}
	s.logger.Info("成功订阅 NATS 房间通配符事件", zap.String("subject", natsRoomSubject))

	// 显式订阅删除事件，也发送到同一个 channel
	subDeleted, err := s.natsConn.ChanSubscribe(NatsRoomDeletedSubject, msgChan)
	if err != nil {
		s.logger.Error("订阅 NATS 房间删除事件失败", zap.String("subject", NatsRoomDeletedSubject), zap.Error(err))
		_ = subWildcard.Unsubscribe() // Clean up the wildcard subscription
		s.listenerMgr.stopAllListeners()
		return fmt.Errorf("subscribing to NATS subject %s failed: %w", NatsRoomDeletedSubject, err)
	}
	s.logger.Info("成功订阅 NATS 房间删除事件", zap.String("subject", NatsRoomDeletedSubject))

	// --- 将 defer 移到这里，在订阅成功之后，循环开始之前 ---
	defer func() {
		s.logger.Info("取消 NATS 订阅...")
		if err := subWildcard.Unsubscribe(); err != nil {
			s.logger.Error("取消 NATS 通配符订阅失败", zap.Error(err))
		}
		if err := subDeleted.Unsubscribe(); err != nil {
			s.logger.Error("取消 NATS 删除订阅失败", zap.Error(err))
		}
		close(msgChan)
	}()
	// --- defer 结束 ---

	// 3. 事件处理循环
	s.logger.Info("进入事件处理循环...")
	for {
		select {
		case msg := <-msgChan:
			s.handleRoomEvent(msg)
		case <-ctx.Done():
			s.logger.Info("收到外部关闭信号，停止管理监听器...")
			s.listenerMgr.stopAllListeners()
			s.roomMapMutex.Lock()
			s.platformToInternalRoomID = make(map[int]string)
			s.roomMapMutex.Unlock()
			return ctx.Err()
		}
	}
}

// handleRoomEvent 处理来自 NATS 的房间事件消息
func (s *BilibiliAdapterService) handleRoomEvent(msg *nats.Msg) {
	s.logger.Debug("收到 NATS 消息", zap.String("subject", msg.Subject), zap.Int("data_len", len(msg.Data)))
	eventType := ""
	if strings.HasPrefix(msg.Subject, natsRoomSubjectPrefix) {
		parts := strings.Split(msg.Subject, ".")
		if len(parts) > 2 {
			eventType = parts[2]
		}
	}
	if eventType == "" {
		s.logger.Warn("无法从 NATS 主题中提取事件类型", zap.String("subject", msg.Subject))
		return
	}
	var room RoomEventData
	if err := json.Unmarshal(msg.Data, &room); err != nil {
		s.logger.Error("解析 NATS 房间事件 JSON 失败", zap.Error(err), zap.String("subject", msg.Subject))
		return
	}
	if room.Platform != bilibiliPlatform {
		s.logger.Debug("忽略非 Bilibili 平台的房间事件", zap.String("platform", room.Platform), zap.String("room_id", room.RoomID))
		return
	}

	biliRoomID, parseErr := strconv.Atoi(room.PlatformRoomID)
	if parseErr != nil {
		s.logger.Error("无法解析事件中的 Bilibili 平台房间 ID",
			zap.String("platform_room_id", room.PlatformRoomID),
			zap.String("internal_room_id", room.RoomID),
			zap.Error(parseErr),
		)
		return
	}

	s.logger.Info("处理房间事件",
		zap.String("event_type", eventType),
		zap.String("status", room.Status),
		zap.Int("bili_room_id", biliRoomID),
		zap.String("internal_room_id", room.RoomID),
	)

	// --- 修改: 显式处理 deleted 事件 ---
	if eventType == "deleted" {
		s.logger.Info("事件触发：房间被删除，停止监听 (如果正在监听)", zap.Int("bili_room_id", biliRoomID))
		s.roomMapMutex.Lock()
		delete(s.platformToInternalRoomID, biliRoomID)
		s.roomMapMutex.Unlock()
		s.listenerMgr.stopListener(biliRoomID) // 确保停止监听
		return                                 // 删除事件处理完毕
	}
	// --- deleted 事件处理结束 ---

	shouldBeListening := (eventType == "created" || eventType == "updated") && room.Status == "active"
	isCurrentlyListening := s.listenerMgr.isListening(biliRoomID)

	if shouldBeListening && !isCurrentlyListening {
		s.logger.Info("事件触发：启动监听 (created/updated active)", zap.Int("bili_room_id", biliRoomID))
		s.roomMapMutex.Lock()
		s.platformToInternalRoomID[biliRoomID] = room.RoomID
		s.roomMapMutex.Unlock()
		s.listenerMgr.startListener(biliRoomID)
	} else if !shouldBeListening && isCurrentlyListening {
		s.logger.Info("事件触发：停止监听", zap.Int("bili_room_id", biliRoomID))
		s.roomMapMutex.Lock()
		delete(s.platformToInternalRoomID, biliRoomID)
		s.roomMapMutex.Unlock()
		s.listenerMgr.stopListener(biliRoomID)
	} else {
		if shouldBeListening {
			s.roomMapMutex.Lock()
			s.platformToInternalRoomID[biliRoomID] = room.RoomID
			s.roomMapMutex.Unlock()
		}
		s.logger.Debug("事件未导致监听状态改变", zap.Int("bili_room_id", biliRoomID), zap.Bool("should_listen", shouldBeListening), zap.Bool("is_listening", isCurrentlyListening))
	}
}

// --- REMOVED: Unused function getBilibiliRoomInfo ---
// func (s *BilibiliAdapterService) getBilibiliRoomInfo(ctx context.Context, platformRoomID int) (title string, streamerName string, err error) {
// ... (function body removed) ...
// }

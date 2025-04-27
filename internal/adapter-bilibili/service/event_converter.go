package service

import (
	"context"
	"encoding/json"
	"fmt"
	"go-xlive/pkg/observability" // <-- 新增: 导入 observability
	"strconv"
	"strings" // <-- 新增: 导入 strings
	"time"

	"github.com/nats-io/nats.go" // <-- 新增: 导入 nats.go
	"github.com/tidwall/gjson"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1 "go-xlive/gen/go/event/v1"
)

// --- 新增: NATS 主题常量 ---
const (
	NatsPlatformStreamStatusSubject = "platform.event.stream.status" // JSON 格式
)

// --- 修改: PlatformStreamStatusEventJSON 结构体, 添加 RoomTitle 和 StreamerName ---
type PlatformStreamStatusEventJSON struct {
	RoomID       string `json:"room_id"`              // 内部 Room ID
	SessionID    string `json:"session_id,omitempty"` // 会话 ID (如果平台提供)
	IsLive       bool   `json:"is_live"`              // true: 开播, false: 下播
	Timestamp    int64  `json:"timestamp"`            // 事件发生时间戳 (Unix 毫秒)
	RoomTitle    string `json:"room_title"`           // <-- 新增: Bilibili 房间标题
	StreamerName string `json:"streamer_name"`        // <-- 新增: Bilibili 主播名称
}

// convertBilibiliEvent - 转换 Bilibili 事件为内部事件或触发动作
// 返回值: (内部 Protobuf 事件, NATS 主题) 或 (nil, "")
// --- 修改: 添加 isDuringLiveSession 参数 ---
func (s *BilibiliAdapterService) convertBilibiliEvent(ctx context.Context, baseEvent BilibiliBaseEvent, listenerRoomID int, internalRoomID, sessionID string, cookieStr string, isDuringLiveSession bool) (proto.Message, string) {
	cmd := baseEvent.Cmd
	if cmd == "" {
		s.logger.Warn("基础事件缺少 cmd 字段", zap.Any("baseEvent", baseEvent))
		return nil, ""
	}
	eventRoomIDStr := numberToString(baseEvent.RoomID)
	if eventRoomIDStr == "" {
		var dataWithRoomID struct {
			RoomID interface{} `json:"roomid"`
		}
		if err := json.Unmarshal(baseEvent.Data, &dataWithRoomID); err == nil {
			eventRoomIDStr = numberToString(dataWithRoomID.RoomID)
		}
	}
	if eventRoomIDStr == "" {
		eventRoomIDStr = strconv.Itoa(listenerRoomID)
		s.logger.Debug("基础事件中未找到 roomid，使用监听器 roomID", zap.Int("listenerRoomID", listenerRoomID), zap.String("cmd", cmd))
	}
	if eventRoomIDStr == "" {
		s.logger.Error("无法确定事件关联的 Bilibili Room ID", zap.String("cmd", cmd))
		return nil, ""
	}

	if internalRoomID == "" {
		s.logger.Error("convertBilibiliEvent 收到空的 internalRoomID，无法处理事件", zap.String("platform_room_id", eventRoomIDStr), zap.String("cmd", cmd))
		return nil, ""
	}
	now := timestamppb.Now()
	nowMillis := time.Now().UnixMilli() // 用于 JSON 事件

	// --- 修改: LIVE 事件处理 (调用 API, 填充事件, 注入追踪上下文) ---
	if cmd == "LIVE" {
		s.logger.Info("收到 LIVE 事件 (直播开始)，将获取房间信息并发布 Stream Status 事件", zap.String("internal_room_id", internalRoomID))

		// --- 调用 API 获取房间信息 ---
		roomInfo, err := getBilibiliRoomDetailsFromAPI(ctx, listenerRoomID, cookieStr, s.logger)
		roomTitle := ""
		streamerName := ""
		if err != nil {
			s.logger.Warn("获取 Bilibili 房间信息失败 (LIVE 事件)", zap.Int("platform_room_id", listenerRoomID), zap.Error(err))
			if strings.Contains(err.Error(), "room not found or not live") {
				s.logger.Info("房间未找到或未开播，仍发布 LIVE=true 事件，但标题和名称为空")
			}
		} else if roomInfo != nil {
			roomTitle = roomInfo.RoomInfo.Title
			streamerName = roomInfo.AnchorInfo.BaseInfo.Uname
			s.logger.Info("成功获取 Bilibili 房间信息 (LIVE 事件)", zap.Int("platform_room_id", listenerRoomID), zap.String("title", roomTitle), zap.String("streamer", streamerName))
		}
		// --- API 调用结束 ---

		// 创建 PlatformStreamStatusEventJSON 事件 (包含标题和名称)
		streamStatusEvent := PlatformStreamStatusEventJSON{
			RoomID:       internalRoomID,
			IsLive:       true,
			Timestamp:    nowMillis,
			RoomTitle:    roomTitle,    // <-- 填充
			StreamerName: streamerName, // <-- 填充
		}
		payload, err := json.Marshal(streamStatusEvent)
		if err != nil {
			s.logger.Error("序列化 PlatformStreamStatusEvent (LIVE) 失败", zap.Error(err), zap.String("internal_room_id", internalRoomID))
		} else {
			// --- 注入追踪上下文并发布 ---
			traceHeadersMap := observability.InjectNATSHeaders(ctx) // 获取 map[string]string
			natsHeader := make(nats.Header)                         // 创建 nats.Header (map[string][]string)
			for k, v := range traceHeadersMap {
				natsHeader[k] = []string{v} // 转换为 map[string][]string
			}

			msg := &nats.Msg{
				Subject: NatsPlatformStreamStatusSubject,
				Data:    payload,
				Header:  natsHeader, // 设置转换后的 nats.Header
			}
			if pubErr := s.natsConn.PublishMsg(msg); pubErr != nil { // 使用 PublishMsg
				s.logger.Error("发布 PlatformStreamStatusEvent (LIVE) 到 NATS 失败", zap.Error(pubErr), zap.String("subject", NatsPlatformStreamStatusSubject))
			} else {
				s.logger.Info("成功发布 PlatformStreamStatusEvent (LIVE) 到 NATS (带追踪头和房间信息)",
					zap.String("subject", NatsPlatformStreamStatusSubject),
					zap.String("internal_room_id", internalRoomID),
					zap.String("room_title", roomTitle),
					zap.String("streamer_name", streamerName),
					zap.Any("headers", natsHeader)) // Log natsHeader
			}
			// --- 发布结束 ---
		}
		return nil, "" // 不返回 Protobuf 事件
	}

	// --- 修改: PREPARING 事件处理 (注入追踪上下文) ---
	if cmd == "PREPARING" {
		s.logger.Info("收到 PREPARING 事件 (直播结束)，将发布 Stream Status 事件", zap.String("internal_room_id", internalRoomID))

		// 创建 PlatformStreamStatusEventJSON 事件
		streamStatusEvent := PlatformStreamStatusEventJSON{
			RoomID:    internalRoomID,
			IsLive:    false,
			Timestamp: nowMillis,
			// RoomTitle 和 StreamerName 留空
		}
		payload, err := json.Marshal(streamStatusEvent)
		if err != nil {
			s.logger.Error("序列化 PlatformStreamStatusEvent (PREPARING) 失败", zap.Error(err), zap.String("internal_room_id", internalRoomID))
		} else {
			// --- 注入追踪上下文并发布 ---
			traceHeadersMap := observability.InjectNATSHeaders(ctx) // 获取 map[string]string
			natsHeader := make(nats.Header)                         // 创建 nats.Header (map[string][]string)
			for k, v := range traceHeadersMap {
				natsHeader[k] = []string{v} // 转换为 map[string][]string
			}

			msg := &nats.Msg{
				Subject: NatsPlatformStreamStatusSubject,
				Data:    payload,
				Header:  natsHeader, // 设置转换后的 nats.Header
			}
			if pubErr := s.natsConn.PublishMsg(msg); pubErr != nil { // 使用 PublishMsg
				s.logger.Error("发布 PlatformStreamStatusEvent (PREPARING) 到 NATS 失败", zap.Error(pubErr), zap.String("subject", NatsPlatformStreamStatusSubject))
			} else {
				s.logger.Info("成功发布 PlatformStreamStatusEvent (PREPARING) 到 NATS (带追踪头)", zap.String("subject", NatsPlatformStreamStatusSubject), zap.String("internal_room_id", internalRoomID), zap.Any("headers", natsHeader)) // Log natsHeader
			}
			// --- 发布结束 ---
		}
		return nil, "" // 不返回 Protobuf 事件
	}

	// --- 处理其他事件 ---
	switch cmd {

	case "DANMU_MSG":
		var infoArr []json.RawMessage
		if err := json.Unmarshal(baseEvent.Info, &infoArr); err != nil || len(infoArr) < 3 {
			s.logger.Warn("DANMU_MSG 格式错误: 解析 info 数组失败或长度不足", zap.Error(err), zap.ByteString("info", baseEvent.Info))
			return nil, ""
		}
		var content string
		if err := json.Unmarshal(infoArr[1], &content); err != nil {
			s.logger.Warn("DANMU_MSG 格式错误: 解析 content 失败", zap.Error(err), zap.ByteString("content_raw", infoArr[1]))
			return nil, ""
		}
		var userInfo DanmuMsgUserInfoArray
		if err := json.Unmarshal(infoArr[2], &userInfo); err != nil {
			s.logger.Warn("DANMU_MSG 格式错误: 解析 userInfo 失败", zap.Error(err), zap.ByteString("userInfo_raw", infoArr[2]))
			return nil, ""
		}
		userIDStr := numberToString(userInfo.UID())
		userName := userInfo.Uname()
		if userIDStr == "" || content == "" {
			s.logger.Warn("DANMU_MSG 缺少必要信息", zap.String("userID", userIDStr), zap.String("content", content))
			return nil, ""
		}
		event := &eventv1.ChatMessage{
			MessageId:    uuid.NewString(),
			SessionId:    sessionID, // 使用传入的 sessionID
			RoomId:       internalRoomID,
			UserId:       userIDStr,
			UserLevel:    int32(gjson.GetBytes(baseEvent.Info, "4.0").Int()),
			Admin:        gjson.GetBytes(baseEvent.Info, "2.2").Bool(),
			MobileVerify: gjson.GetBytes(baseEvent.Info, "2.6").Bool(),
			GuardLevel:   int32(gjson.GetBytes(baseEvent.Info, "2.7").Int()),
			Medal: &eventv1.Medal{
				MedalLevel:    int32(gjson.GetBytes(baseEvent.Info, "3.0").Int()),
				MedalName:     gjson.GetBytes(baseEvent.Info, "3.1").Str,
				MedalUpname:   gjson.GetBytes(baseEvent.Info, "3.2").Str,
				MedalUproomid: int32(gjson.GetBytes(baseEvent.Info, "3.3").Int()),
				MedalUpuid:    int32(gjson.GetBytes(baseEvent.Info, "3.4").Int()),
			},
			Username:            userName,
			Content:             content,
			Timestamp:           now,
			RawMsg:              string(baseEvent.RawData),
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.chat.message"

	case "INTERACT_WORD":
		var data InteractWordData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("INTERACT_WORD 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		if dataRoomIDStr := numberToString(data.RoomID); dataRoomIDStr != "" && dataRoomIDStr != eventRoomIDStr {
			s.logger.Warn("INTERACT_WORD 事件中的 roomid 与监听器 roomid 不符，忽略", zap.String("event_roomid", dataRoomIDStr), zap.String("listener_roomid", eventRoomIDStr))
			return nil, ""
		}
		userIDStr := numberToString(data.UID)
		if userIDStr == "" || data.Uname == "" {
			s.logger.Warn("INTERACT_WORD 缺少 uid 或 uname", zap.Any("data", data))
			return nil, ""
		}
		eventTime := now
		if data.Timestamp > 0 {
			eventTime = timestamppb.New(time.Unix(data.Timestamp, 0))
		}
		if data.MsgType == 1 { // User entry
			event := &eventv1.UserPresence{
				SessionId:           sessionID, // 使用传入的 sessionID
				RoomId:              internalRoomID,
				UserId:              userIDStr,
				Username:            data.Uname,
				Entered:             true,
				Timestamp:           eventTime,
				IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
			}
			return event, "events.raw.user.presence"
		} else if data.MsgType == 2 { // Follow event
			event := &eventv1.UserInteraction{
				EventId:             uuid.NewString(),
				SessionId:           sessionID, // 使用传入的 sessionID
				RoomId:              internalRoomID,
				UserId:              userIDStr,
				Username:            data.Uname,
				Type:                eventv1.UserInteraction_FOLLOW,
				Timestamp:           eventTime,
				IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
			}
			return event, "events.raw.user.interaction"
		} else {
			s.logger.Debug("收到未处理类型的 INTERACT_WORD", zap.Int("msg_type", data.MsgType), zap.Any("data", data))
			return nil, ""
		}

	case "SEND_GIFT":
		var data SendGiftData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("SEND_GIFT 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		userIDStr := numberToString(data.UID)
		giftIDStr := strconv.Itoa(data.GiftID)
		if giftIDStr == "" || userIDStr == "" || data.Tid == "" || data.Num == 0 {
			s.logger.Warn("SEND_GIFT 缺少必要信息", zap.Any("data", data))
			return nil, ""
		}
		eventTime := now
		if data.Timestamp > 0 {
			eventTime = timestamppb.New(time.Unix(data.Timestamp, 0))
		}
		event := &eventv1.GiftSent{
			EventId:             data.Tid,
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			UserId:              userIDStr,
			Username:            data.Uname,
			GiftId:              giftIDStr,
			GiftName:            data.GiftName,
			GiftCount:           int32(data.Num),
			TotalCoin:           data.TotalCoin,
			CoinType:            data.CoinType,
			Timestamp:           eventTime,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.gift.sent"

	case "GUARD_BUY":
		var data GuardBuyData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("GUARD_BUY 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		userIDStr := numberToString(data.UID)
		if userIDStr == "" || data.GuardLevel == 0 || data.Num == 0 {
			s.logger.Warn("GUARD_BUY 缺少必要信息", zap.Any("data", data))
			return nil, ""
		}
		eventTime := now
		if data.StartTime > 0 {
			eventTime = timestamppb.New(time.Unix(data.StartTime, 0))
		}
		guardName := ""
		switch data.GuardLevel {
		case 1:
			guardName = "总督"
		case 2:
			guardName = "提督"
		case 3:
			guardName = "舰长"
		}
		event := &eventv1.GuardPurchase{
			EventId:             fmt.Sprintf("guard_%s_%d", userIDStr, data.StartTime),
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			UserId:              userIDStr,
			Username:            data.Username,
			GuardLevel:          int32(data.GuardLevel),
			GuardName:           guardName,
			Count:               int32(data.Num),
			Price:               data.Price,
			Timestamp:           eventTime,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.guard.purchase"

	case "SUPER_CHAT_MESSAGE":
		var data SuperChatMessageData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("SUPER_CHAT_MESSAGE 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		if dataRoomIDStr := numberToString(data.RoomID); dataRoomIDStr != "" && dataRoomIDStr != eventRoomIDStr {
			s.logger.Warn("SUPER_CHAT_MESSAGE 事件中的 roomid 与监听器 roomid 不符，忽略", zap.String("event_roomid", dataRoomIDStr), zap.String("listener_roomid", eventRoomIDStr))
			return nil, ""
		}
		messageIDStr := numberToString(data.ID)
		userIDStr := numberToString(data.UID)
		if messageIDStr == "" || userIDStr == "" || data.Message == "" || data.Price == 0 || data.Time == 0 {
			s.logger.Warn("SUPER_CHAT_MESSAGE 缺少必要信息", zap.Any("data", data))
			return nil, ""
		}
		eventTime := now
		if data.StartTime > 0 {
			eventTime = timestamppb.New(time.Unix(data.StartTime, 0))
		}
		event := &eventv1.SuperChatMessage{
			MessageId:           messageIDStr,
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			UserId:              userIDStr,
			Username:            data.UserInfo.Uname,
			Content:             data.Message,
			Price:               int64(data.Price),
			Duration:            int32(data.Time),
			Timestamp:           eventTime,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.superchat.message"

	case "WATCHED_CHANGE":
		var data WatchedChangeData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("WATCHED_CHANGE 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		if data.Num == 0 && data.TextLarge == "" {
			return nil, ""
		}
		event := &eventv1.WatchedCountUpdate{
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			Count:               data.Num,
			TextLarge:           data.TextLarge,
			Timestamp:           now,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.watched.update"

	case "LIKE_INFO_V3_UPDATE":
		var data LikeInfoV3UpdateData
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("LIKE_INFO_V3_UPDATE 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		if data.ClickCount == 0 {
			return nil, ""
		}
		event := &eventv1.LikeCountUpdate{
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			Count:               data.ClickCount,
			Timestamp:           now,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.like.update"

	case "ONLINE_RANK_V2":
		var data OnlineRankV2Data
		if err := json.Unmarshal(baseEvent.Data, &data); err != nil {
			s.logger.Warn("ONLINE_RANK_V2 解析 data 失败", zap.Error(err), zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		if len(data.List) == 0 {
			return nil, ""
		}
		topUsers := make([]*eventv1.OnlineRankUpdate_RankUser, 0, len(data.List))
		for _, user := range data.List {
			uidStr := numberToString(user.UID)
			if uidStr != "" && user.Uname != "" && user.Rank > 0 {
				topUsers = append(topUsers, &eventv1.OnlineRankUpdate_RankUser{
					UserId:   uidStr,
					Username: user.Uname,
					Rank:     int32(user.Rank),
					Score:    user.Score,
					FaceUrl:  user.Face,
				})
			}
		}
		if len(topUsers) == 0 {
			return nil, ""
		}
		event := &eventv1.OnlineRankUpdate{
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			TopUsers:            topUsers,
			Timestamp:           now,
			IsDuringLiveSession: isDuringLiveSession, // <-- 设置新字段
		}
		return event, "events.raw.rank.update"

	case "ROOM_CHANGE": // 这个事件现在由 websocket.go 中的 handleBusinessMessage 处理，这里不再需要
		s.logger.Debug("convertBilibiliEvent 忽略 ROOM_CHANGE 事件 (已在 handleBusinessMessage 中处理)")
		return nil, ""

	// --- Other known but unhandled events ---
	case "ONLINE_RANK_COUNT":
		// 解析 data.online_count
		onlineCountResult := gjson.GetBytes(baseEvent.Data, "online_count")
		if !onlineCountResult.Exists() {
			s.logger.Warn("ONLINE_RANK_COUNT 事件缺少 data.online_count 字段", zap.ByteString("data", baseEvent.Data))
			return nil, ""
		}
		onlineCount := onlineCountResult.Int() // gjson returns int64

		s.logger.Debug("收到 ONLINE_RANK_COUNT", zap.Int64("online_count", onlineCount))

		// 创建内部事件
		event := &eventv1.OnlineRankCountUpdatedEvent{
			SessionId:           sessionID, // 使用传入的 sessionID
			RoomId:              internalRoomID,
			Count:               onlineCount,
			Timestamp:           now,
			IsDuringLiveSession: isDuringLiveSession, // 使用传入的标志
		}
		// 返回事件和兼容现有订阅的 NATS 主题
		return event, "events.raw.online_rank_count.updated" // 修改主题以匹配 events.raw.>

	case "STOP_LIVE_ROOM_LIST", "WIDGET_BANNER", "HOT_RANK_CHANGED", "HOT_RANK_CHANGED_V2",
		"ENTRY_EFFECT", "ROOM_REAL_TIME_MESSAGE_UPDATE", "VOICE_JOIN_STATUS", "ONLINE_RANK_TOP3",
		"COMMON_NOTICE_DANMAKU", "USER_TOAST_MSG", "NOTICE_MSG", "GUARD_HONOR_THOUSAND",
		"POPULARITY_RED_POCKET_START", "POPULARITY_RED_POCKET_WINNER_LIST",
		"PK_BATTLE_PRE", "PK_BATTLE_START", "PK_BATTLE_PROCESS", "PK_BATTLE_END", "PK_BATTLE_SETTLE",
		"RECOMMEND_CARD", "LIVE_INTERACTIVE_GAME", "ANCHOR_LOT_START", "ANCHOR_LOT_AWARD":
		s.logger.Debug("收到已知但未处理的 Bilibili cmd 类型", zap.String("cmd", cmd))
		return nil, ""
	case "WARNING", "CUT_OFF", "CUT_OFF_V2":
		s.logger.Warn("收到平台警告或切断消息", zap.String("cmd", cmd), zap.ByteString("data", baseEvent.Data))
		return nil, ""

	default:
		s.logger.Debug("收到未知的 Bilibili cmd 类型", zap.String("cmd", cmd))
		return nil, ""
	}
}
func numberToString(num interface{}) string {
	switch v := num.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

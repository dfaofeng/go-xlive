package service

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go-xlive/pkg/observability" // <-- 确保 observability 已导入

	// eventv1 "go-xlive/gen/go/event/v1" // 移除 eventv1 导入
	userv1 "go-xlive/gen/go/user/v1" // <-- 新增: 导入 User Service proto

	"github.com/andybalholm/brotli"
	"github.com/nats-io/nats.go" // <-- 确保 nats.go 已导入

	// sessionv1 import removed
	"io"
	"math/rand"
	"net"
	"net/http"           // <-- 新增: HTTP client
	"net/http/cookiejar" // <-- 新增: Cookie Jar
	"net/url"
	"strconv" // <-- 新增: strconv
	"strings" // <-- 新增: strings
	"time"

	// <-- 新增: Brotli 解压缩库
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"    // <-- 新增: gRPC status
	"google.golang.org/protobuf/proto" // <-- 恢复导入 proto
	// "google.golang.org/protobuf/types/known/timestamppb" // 移除 Timestamp 导入
)

// --- Bilibili WebSocket 协议常量 ---
const (
	wsOpHeartbeat      = 2
	wsOpHeartbeatReply = 3
	wsOpMessage        = 5
	wsOpUserAuth       = 7
	wsOpConnectSuccess = 8

	// 协议版本 (对应 protoVer 字段)
	wsProtoVerRaw    uint16 = 0 // Body 为 JSON
	wsProtoVerInt    uint16 = 1 // Body 为 Int32 (人气值) - 实际上这个版本号不常用，人气值在 Op=3 的包体中
	wsProtoVerZlib   uint16 = 2 // Body 为 Zlib 压缩的多个包
	wsProtoVerBrotli uint16 = 3 // Body 为 Brotli 压缩

	// 协议包相关常量
	wsPackageOffset         = 0   // 包总长度偏移量
	wsHeaderOffset          = 4   // 头部长度偏移量
	wsPackageHeaderLength   = 16  // 包头部总长度
	wsPackageHeaderDataMask = 0xF // 包头部数据掩码
)

// --- Bilibili API 相关 ---
const (
	danmuConfURL = "https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo"
	roomInfoURL  = "https://api.live.bilibili.com/xlive/web-room/v1/index/getInfoByRoom"                                             // <-- 新增: 房间信息 API URL
	userAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36" // 模拟浏览器 User-Agent

	// NATS 主题常量和平台标识移至 service.go
)

// --- Bilibili API Response Structs ---

// DanmuConfResponse /Danmu/getConf API 响应结构体 (简化)
type DanmuConfResponse struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    DanmuConfData `json:"data"`
}

// DanmuConfData API 响应中的 data 字段
type DanmuConfData struct {
	Token    string      `json:"token"`     // 认证 key
	HostList []DanmuHost `json:"host_list"` // WebSocket 服务器列表
}

// DanmuHost WebSocket 服务器信息
type DanmuHost struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	WssPort int    `json:"wss_port"`
	WsPort  int    `json:"ws_port"`
}

// RoomInfoResponse /getInfoByRoom API 响应结构体 (简化)
type RoomInfoResponse struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *RoomInfoData `json:"data"` // Use pointer to handle potential null data
}

// RoomInfoData API 响应中的 data 字段
type RoomInfoData struct {
	RoomInfo   RoomInfo   `json:"room_info"`
	AnchorInfo AnchorInfo `json:"anchor_info"`
}

// RoomInfo 房间基本信息
type RoomInfo struct {
	UID            int64  `json:"uid"`              // 主播 UID
	RoomID         int    `json:"room_id"`          // 平台房间 ID
	Title          string `json:"title"`            // 房间标题
	Cover          string `json:"cover"`            // 封面图 URL
	LiveStatus     int    `json:"live_status"`      // 直播状态 (0: 未开播, 1: 直播中, 2: 轮播中)
	AreaID         int    `json:"area_id"`          // 子分区 ID
	AreaName       string `json:"area_name"`        // 子分区名称
	ParentAreaID   int    `json:"parent_area_id"`   // 父分区 ID
	ParentAreaName string `json:"parent_area_name"` // 父分区名称
}

// type sessionIDMap struct { // REMOVED: Unused type
// 	internalRoomID string
// 	sessionID      string
// }

// AnchorInfo 主播信息
type AnchorInfo struct {
	BaseInfo BaseInfo `json:"base_info"`
}

// BaseInfo 主播基础信息
type BaseInfo struct {
	Uname string `json:"uname"` // 主播昵称
	Face  string `json:"face"`  // 主播头像 URL
	// ... 可以根据需要添加更多字段
}

// PlatformRoomInfoUpdatedEventJSON 定义 NATS 平台信息更新事件消息结构 (JSON)
// 与 room/service/service.go 中的定义保持一致
type PlatformRoomInfoUpdatedEventJSON struct {
	RoomID         string `json:"room_id"`
	Platform       string `json:"platform"`
	PlatformRoomID string `json:"platform_room_id"`
	RoomName       string `json:"room_name"`   // 对应旧 Protobuf 的 Title/RoomName
	AnchorName     string `json:"anchor_name"` // <-- 修改: 添加主播名称字段
	AreaName       string `json:"area_name"`
	LiveStatus     int32  `json:"live_status"` // 新增直播状态
	Timestamp      int64  `json:"timestamp"`   // Unix 毫秒时间戳 (可选，由发布者提供)
}

// --- WebSocket 连接逻辑 ---

// runListenerLoop 是实际执行单个房间监听的循环
func (s *BilibiliAdapterService) runListenerLoop(ctx context.Context, roomID int) {
	logger := s.logger.With(zap.Int("bili_room_id", roomID))
	logger.Info("开始监听 Bilibili 直播间 (runListenerLoop)")
	retryDelay := 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			logger.Info("收到退出信号，停止监听循环")
			return
		default:
			listenCtx, listenCancel := context.WithCancel(ctx)
			err := s.connectAndListenOnce(listenCtx, roomID, logger)
			listenCancel()

			if err != nil {
				if errors.Is(err, context.Canceled) {
					logger.Info("Context 取消，正常退出监听循环")
					return
				}
				logger.Error("监听过程中发生错误，准备重连...", zap.Error(err))
				jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
				time.Sleep(retryDelay + jitter)
				if errors.Is(err, fmt.Errorf("system bilibili cookie not found")) {
					retryDelay = 300 * time.Second
					logger.Warn("系统 Bilibili Cookie 未设置，大幅度延长重试间隔", zap.Duration("retry_delay", retryDelay))
				} else {
					if retryDelay < 60*time.Second {
						retryDelay *= 2
					} else {
						retryDelay = 60 * time.Second
					}
				}
			} else {
				// 如果 connectAndListenOnce 正常返回 (通常是连接关闭)，则立即重连
				logger.Info("WebSocket 连接关闭，准备重连...")
				retryDelay = 5 * time.Second // 重置重连延迟
			}
			time.Sleep(retryDelay) // 在重连前等待
		}
	}
}

// connectAndListenOnce 执行一次 WebSocket 连接、认证、心跳和消息监听
func (s *BilibiliAdapterService) connectAndListenOnce(ctx context.Context, roomID int, logger *zap.Logger) error {
	// --- 1. 获取系统 Cookie ---
	cookieCtx, cookieCancel := context.WithTimeout(ctx, 5*time.Second)
	cookieResp, err := s.userClient.GetSystemBilibiliCookie(cookieCtx, &userv1.GetSystemBilibiliCookieRequest{})
	cookieCancel()
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.NotFound {
			logger.Error("无法连接 WebSocket：系统 Bilibili Cookie 未设置。请通过 API 设置。", zap.Int("bili_room_id", roomID))
			return fmt.Errorf("system bilibili cookie not found")
		}
		logger.Error("获取系统 Bilibili Cookie 失败", zap.Error(err))
		return fmt.Errorf("getting system cookie: %w", err)
	}
	systemCookie := cookieResp.GetCookie()
	if systemCookie == "" {
		logger.Error("系统 Bilibili Cookie 为空，无法连接")
		return fmt.Errorf("system bilibili cookie is empty")
	}
	uid, err := getBilibiliUIDFromCookie(systemCookie)
	if err != nil {
		logger.Error("无法从 Cookie 中解析 Bilibili UID", zap.Error(err))
		return fmt.Errorf("parsing uid from cookie: %w", err)
	}

	// --- 3. 调用 Bilibili API 获取认证信息 (token 和 host_list) ---
	danmuInfo, err := getDanmuInfo(ctx, roomID, systemCookie, logger)
	if err != nil {
		logger.Error("获取 Bilibili Danmu 配置失败", zap.Error(err))
		return fmt.Errorf("getting danmu info: %w", err)
	}
	if danmuInfo == nil || danmuInfo.Token == "" || len(danmuInfo.HostList) == 0 {
		logger.Error("获取到的 Danmu 配置无效", zap.Any("danmu_info", danmuInfo))
		return fmt.Errorf("invalid danmu info received")
	}

	// --- 4. 选择 WebSocket 服务器并构建 URL ---
	selectedHost := danmuInfo.HostList[0]
	wsURL := url.URL{
		Scheme: "wss",
		Host:   fmt.Sprintf("%s:%d", selectedHost.Host, selectedHost.WssPort),
		Path:   "/sub",
	}
	logger.Info("准备连接 WebSocket", zap.String("url", wsURL.String()))

	// --- 5. 建立 WebSocket 连接 ---
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("WebSocket 连接被取消")
			return err
		}
		logger.Error("WebSocket 连接失败", zap.Error(err))
		return fmt.Errorf("dialing websocket: %w", err)
	}
	defer conn.Close()
	logger.Info("WebSocket 连接成功")

	// --- 6. 发送认证包 ---
	authBody := fmt.Sprintf(`{"roomid":%d,"protover":%d,"platform":"web","clientver":"1.14.3","uid":%d,"key":"%s"}`,
		roomID, wsProtoVerZlib, uid, danmuInfo.Token) // 请求使用 Zlib 压缩
	err = s.sendWebsocketPacket(conn, wsOpUserAuth, []byte(authBody))
	if err != nil {
		logger.Error("发送认证包失败", zap.Error(err))
		return fmt.Errorf("sending auth packet: %w", err)
	}
	logger.Info("认证包已发送", zap.Int64("uid", uid))

	// --- 7. 启动心跳和消息接收循环 ---
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go s.runHeartbeat(heartbeatCtx, conn, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("接收循环收到退出信号")
			return context.Canceled
		default:
			conn.SetReadDeadline(time.Now().Add(60 * time.Second)) // 设置读取超时
			msgType, msgData, err := conn.ReadMessage()
			conn.SetReadDeadline(time.Time{}) // 清除读取超时

			if err != nil {
				select {
				case <-ctx.Done():
					logger.Info("WebSocket 读取因 context 取消而中断")
					return context.Canceled
				default:
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						logger.Info("WebSocket 正常关闭")
					} else if websocket.IsUnexpectedCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseNoStatusReceived) {
						logger.Warn("WebSocket 异常关闭", zap.Error(err))
					} else if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
						logger.Info("WebSocket 连接已关闭 (EOF/NetClosed)")
					} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						logger.Warn("WebSocket 读取超时")
						return fmt.Errorf("websocket read timeout: %w", err)
					} else {
						logger.Error("WebSocket 读取消息时发生未知错误", zap.Error(err))
					}
					return fmt.Errorf("reading websocket message: %w", err)
				}
			}

			if msgType == websocket.BinaryMessage {
				// --- 修改: 传递 systemCookie ---
				s.processWebsocketMessage(ctx, msgData, logger, roomID, systemCookie)
			} else {
				logger.Warn("收到非二进制 WebSocket 消息", zap.Int("type", msgType))
			}
		}
	}
}

// sendWebsocketPacket 封装发送 Bilibili WebSocket 数据包的逻辑
func (s *BilibiliAdapterService) sendWebsocketPacket(conn *websocket.Conn, operation int32, body []byte) error {
	packetLen := int32(wsPackageHeaderLength + len(body))
	header := new(bytes.Buffer)
	_ = binary.Write(header, binary.BigEndian, packetLen)
	_ = binary.Write(header, binary.BigEndian, int16(wsPackageHeaderLength)) // Header length
	_ = binary.Write(header, binary.BigEndian, int16(wsProtoVerRaw))         // Protocol version (use raw for sending)
	_ = binary.Write(header, binary.BigEndian, operation)
	_ = binary.Write(header, binary.BigEndian, int32(1)) // Sequence ID (usually 1)

	fullPacket := append(header.Bytes(), body...)
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second)) // 设置写入超时
	err := conn.WriteMessage(websocket.BinaryMessage, fullPacket)
	conn.SetWriteDeadline(time.Time{}) // 清除写入超时
	return err
}

// runHeartbeat 定期发送心跳包
func (s *BilibiliAdapterService) runHeartbeat(ctx context.Context, conn *websocket.Conn, logger *zap.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	logger.Info("心跳 goroutine 已启动")

	for {
		select {
		case <-ticker.C:
			logger.Debug("发送心跳包...")
			if err := s.sendWebsocketPacket(conn, wsOpHeartbeat, []byte("[]")); err != nil {
				logger.Error("发送心跳包失败，心跳 goroutine 退出", zap.Error(err))
				return
			}
		case <-ctx.Done():
			logger.Info("心跳 goroutine 收到退出信号")
			return
		}
	}
}

// processWebsocketMessage 处理收到的单个 WebSocket 二进制消息
// --- 修改: 添加 ctx 参数 ---
func (s *BilibiliAdapterService) processWebsocketMessage(ctx context.Context, msgData []byte, logger *zap.Logger, platformRoomID int, cookieStr string) {
	if len(msgData) < wsPackageHeaderLength {
		logger.Warn("收到的 WebSocket 数据包过短", zap.Int("len", len(msgData)), zap.Int("min_expected", wsPackageHeaderLength))
		return
	}

	packetLen := binary.BigEndian.Uint32(msgData[wsPackageOffset:])
	headerLen := binary.BigEndian.Uint16(msgData[wsHeaderOffset:])
	protoVer := binary.BigEndian.Uint16(msgData[6:8])
	operation := binary.BigEndian.Uint32(msgData[8:12])

	if headerLen < wsPackageHeaderLength || int(headerLen) > len(msgData) {
		logger.Warn("收到的 WebSocket 数据包头部长无效", zap.Uint16("header_len", headerLen), zap.Int("msg_len", len(msgData)))
		return
	}

	if packetLen < uint32(len(msgData)) {
		// --- 修改: 传递 ctx ---
		s.processWebsocketMessage(ctx, msgData[:packetLen], logger, platformRoomID, cookieStr)
		s.processWebsocketMessage(ctx, msgData[packetLen:], logger, platformRoomID, cookieStr)
		return
	}

	body := msgData[int(headerLen):]

	switch operation {
	case wsOpHeartbeatReply:
		if len(body) >= 4 {
			onlineCount := binary.BigEndian.Uint32(body)
			logger.Debug("收到心跳回应(人气值)", zap.Uint32("online_count", onlineCount))
			// 可选：发布人气值事件
		} else {
			logger.Warn("收到的心跳回应包体过短", zap.Int("body_len", len(body)))
		}
	case wsOpConnectSuccess:
		logger.Info("WebSocket 认证成功")
		// 认证成功后，立即获取并发布一次房间信息
		// --- 修改: 传递 ctx ---
		go s.fetchAndPublishRoomInfo(ctx, platformRoomID, logger.With(zap.String("trigger", "connect_success")), cookieStr)
	case wsOpMessage:
		logger.Debug("收到业务消息包", zap.Uint16("protoVer", protoVer))
		switch protoVer {
		case wsProtoVerRaw:
			// --- 修改: 传递 ctx ---
			s.handleBusinessMessage(ctx, body, logger, platformRoomID, cookieStr)
		case wsProtoVerZlib:
			reader, zlibErr := zlib.NewReader(bytes.NewReader(body))
			if zlibErr != nil {
				logger.Error("创建 zlib reader 失败", zap.Error(zlibErr))
				return
			}
			decompressedBody, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				logger.Error("解压 zlib 消息体失败", zap.Error(err))
				return
			}
			logger.Debug("Zlib 解压成功", zap.Int("original_len", len(body)), zap.Int("decompressed_len", len(decompressedBody)))
			// --- 修改: 传递 ctx ---
			s.processDecompressedMessages(ctx, decompressedBody, logger, platformRoomID, cookieStr)
		case wsProtoVerBrotli:
			logger.Debug("收到 Brotli 压缩消息，开始解压")
			reader := brotli.NewReader(bytes.NewReader(body))
			decompressedBody, err := io.ReadAll(reader)
			if err != nil {
				logger.Error("Brotli解压失败", zap.Error(err), zap.Int("compressed_length", len(body)), zap.String("first_bytes_hex", fmt.Sprintf("%x", body[:min(20, len(body))])))
				return
			}
			if len(decompressedBody) == 0 {
				logger.Error("Brotli解压后数据为空", zap.Int("compressed_length", len(body)))
				return
			}
			logger.Debug("Brotli解压成功", zap.Int("original_len", len(body)), zap.Int("decompressed_len", len(decompressedBody)))
			// --- 修改: 传递 ctx ---
			s.processDecompressedMessages(ctx, decompressedBody, logger, platformRoomID, cookieStr)
		default:
			logger.Warn("收到未知的压缩格式业务消息", zap.Uint16("protoVer", protoVer))
		}
	default:
		logger.Warn("收到未知的 WebSocket 操作码", zap.Uint32("operation", operation))
	}
}

// processDecompressedMessages 处理解压后的消息数据
// --- 修改: 添加 ctx 参数 ---
func (s *BilibiliAdapterService) processDecompressedMessages(ctx context.Context, decompressedBody []byte, logger *zap.Logger, platformRoomID int, cookieStr string) {
	cursor := 0
	totalLen := len(decompressedBody)

	for cursor < totalLen {
		if totalLen-cursor < wsPackageHeaderLength {
			logger.Warn("处理解压数据时剩余数据不足", zap.Int("remaining", totalLen-cursor), zap.Int("min_expected", wsPackageHeaderLength))
			break
		}

		subPacketLen := int(binary.BigEndian.Uint32(decompressedBody[cursor : cursor+4]))
		subHeaderLen := int(binary.BigEndian.Uint16(decompressedBody[cursor+4 : cursor+6]))
		subOperation := binary.BigEndian.Uint32(decompressedBody[cursor+8 : cursor+12])

		if subPacketLen <= 0 || subPacketLen > totalLen-cursor {
			logger.Error("处理解压数据时子包长度无效或超出剩余数据", zap.Int("subPacketLen", subPacketLen), zap.Int("remaining", totalLen-cursor), zap.Int("cursor", cursor))
			break
		}
		if subHeaderLen < wsPackageHeaderLength || subHeaderLen > subPacketLen {
			logger.Error("处理解压数据时子包头部长度无效", zap.Int("subHeaderLen", subHeaderLen), zap.Int("subPacketLen", subPacketLen), zap.Int("cursor", cursor))
			break
		}

		subBody := decompressedBody[cursor+subHeaderLen : cursor+subPacketLen]

		if subOperation == wsOpMessage {
			// --- 修改: 传递 ctx ---
			s.handleBusinessMessage(ctx, subBody, logger, platformRoomID, cookieStr)
		} else {
			logger.Debug("在解压后的数据中发现非业务消息类型的子包", zap.Uint32("subOperation", subOperation))
		}

		cursor += subPacketLen
	}

	if cursor != totalLen {
		logger.Debug("处理解压数据后有剩余字节", zap.Int("cursor", cursor), zap.Int("totalLen", totalLen), zap.Int("remaining", totalLen-cursor))
	}
}

// handleBusinessMessage 处理单个解压后的 JSON 业务消息体
// --- 修改: 添加 ctx 参数, 修改调用 convertBilibiliEvent 传递 cookieStr 和 isDuringLiveSession ---
func (s *BilibiliAdapterService) handleBusinessMessage(ctx context.Context, jsonData []byte, logger *zap.Logger, platformRoomID int, cookieStr string) {
	var baseEvent BilibiliBaseEvent
	if err := json.Unmarshal(jsonData, &baseEvent); err == nil {
		if baseEvent.Cmd == "" {
			var genericData map[string]interface{}
			if json.Unmarshal(jsonData, &genericData) == nil {
				if cmdStr, ok := genericData["cmd"].(string); ok {
					baseEvent.Cmd = cmdStr
				}
			}
			if baseEvent.Cmd == "" {
				logger.Warn("业务消息 JSON 中缺少 'cmd' 字段", zap.ByteString("json_data", jsonData))
				return
			}
		}
		baseEvent.RawData = jsonData
		logger.Debug("成功解析业务消息 JSON", zap.String("cmd", baseEvent.Cmd))

		// <-- 新增: 处理 ROOM_CHANGE 事件 -->
		if baseEvent.Cmd == "ROOM_CHANGE" {
			logger.Info("收到 ROOM_CHANGE 事件，将获取并发布最新房间信息", zap.Int("platform_room_id", platformRoomID))
			// --- 修改: 传递 ctx ---
			go s.fetchAndPublishRoomInfo(ctx, platformRoomID, logger.With(zap.String("trigger", "room_change")), cookieStr)
			// ROOM_CHANGE 事件本身通常不包含需要转换为内部事件的数据，所以这里可以直接返回
			return
		}
		// <-- ROOM_CHANGE 处理结束 -->

		s.roomMapMutex.RLock()
		internalRoomID, ok := s.platformToInternalRoomID[platformRoomID]
		s.roomMapMutex.RUnlock()

		if !ok {
			logger.Error("无法找到 platformRoomID 对应的 internalRoomID，跳过事件处理", zap.Int("platform_room_id", platformRoomID))
			return
		}

		// 异步处理和发布 (Protobuf 格式)
		go func(ctx context.Context, be BilibiliBaseEvent, rid int, internalID string, cookie string) { // <-- 添加 ctx 和 cookie
			// --- 修改: 从本地缓存获取 Session ID ---
			s.sessionIDCacheMutex.RLock()
			sessionID, found := s.sessionIDCache[internalID]
			s.sessionIDCacheMutex.RUnlock()

			isDuringLiveSession := found // 如果在缓存中找到，则认为处于直播会话中

			if found {
				logger.Debug("从本地缓存找到活跃会话", zap.String("internal_room_id", internalID), zap.String("session_id", sessionID))
			} else {
				logger.Debug("本地缓存未找到活跃会话", zap.String("internal_room_id", internalID))
			}
			// --- 本地缓存获取结束 ---

			// --- 新增: 强制要求 Session 创建成功才处理依赖事件 ---
			if isEventSessionRequired(be.Cmd) && sessionID == "" {
				logger.Warn("事件需要 Session ID 但获取失败 (或未找到)，跳过处理",
					zap.String("cmd", be.Cmd),
					zap.String("internal_room_id", internalID),
				)
				return // 放弃处理此事件
			}
			// --- Session 检查结束 ---

			// --- 修改: 传递 ctx, cookie, 和 isDuringLiveSession ---
			internalProtoEvent, natsSubject := s.convertBilibiliEvent(ctx, be, rid, internalID, sessionID, cookie, isDuringLiveSession)
			if internalProtoEvent != nil && natsSubject != "" {
				payloadBytes, marshalErr := proto.Marshal(internalProtoEvent)
				if marshalErr != nil {
					logger.Error("序列化内部事件为 Protobuf 失败", zap.Error(marshalErr), zap.String("subject", natsSubject))
					return
				}

				// --- 修改: 发布时也注入追踪上下文 ---
				traceHeadersMap := observability.InjectNATSHeaders(ctx) // 获取 map[string]string
				natsHeader := make(nats.Header)                         // 创建 nats.Header (map[string][]string)
				for k, v := range traceHeadersMap {
					natsHeader[k] = []string{v} // 转换为 map[string][]string
				}
				msg := &nats.Msg{
					Subject: natsSubject,
					Data:    payloadBytes,
					Header:  natsHeader, // 设置转换后的 nats.Header
				}

				if pubErr := s.natsConn.PublishMsg(msg); pubErr != nil { // 使用 PublishMsg
					logger.Error("发布 Protobuf 事件到 NATS 失败", zap.Error(pubErr), zap.String("subject", natsSubject))
				} else {
					logger.Debug("成功发布 Protobuf 事件到 NATS",
						zap.String("subject", natsSubject),
						zap.String("session_id", sessionID),             // Log the session ID used
						zap.Bool("is_during_live", isDuringLiveSession), // Log the live status flag
						zap.Any("headers", natsHeader),                  // Log headers for tracing
					)
				}
			}
		}(ctx, baseEvent, platformRoomID, internalRoomID, cookieStr) // <-- 传递 ctx 和 cookieStr

	} else {
		logger.Error("解析业务消息 JSON 失败", zap.Error(err), zap.ByteString("json_data", jsonData))
	}
}

// fetchAndPublishRoomInfo 获取并发布房间信息到 NATS (JSON 格式)
// --- 修改: 添加 ctx 参数 ---
func (s *BilibiliAdapterService) fetchAndPublishRoomInfo(ctx context.Context, platformRoomID int, logger *zap.Logger, cookieStr string) {
	logger.Info("开始获取并发布房间信息 (JSON)", zap.Int("platform_room_id", platformRoomID))
	// 1. 获取 Internal Room ID
	s.roomMapMutex.RLock()
	internalRoomID, ok := s.platformToInternalRoomID[platformRoomID]
	s.roomMapMutex.RUnlock()
	if !ok {
		logger.Error("无法找到 platformRoomID 对应的 internalRoomID，无法发布房间信息", zap.Int("platform_room_id", platformRoomID))
		return
	}
	// --- 如果 cookie 为空，则重新获取 ---
	if cookieStr == "" {
		logger.Info("未提供 Cookie，尝试从 User Service 获取最新系统 Cookie", zap.Int("platform_room_id", platformRoomID))
		cookieCtx, cookieCancel := context.WithTimeout(ctx, 5*time.Second)
		// --- 修改: 传递 cookieCtx ---
		cookieResp, err := s.userClient.GetSystemBilibiliCookie(cookieCtx, &userv1.GetSystemBilibiliCookieRequest{})
		cookieCancel()
		if err != nil {
			logger.Error("重新获取系统 Bilibili Cookie 失败，无法继续获取房间信息", zap.Error(err), zap.Int("platform_room_id", platformRoomID))
			return
		}
		cookieStr = cookieResp.GetCookie()
		if cookieStr == "" {
			logger.Error("重新获取的系统 Bilibili Cookie 为空，无法继续获取房间信息", zap.Int("platform_room_id", platformRoomID))
			return
		}
		logger.Info("成功重新获取系统 Cookie", zap.Int("platform_room_id", platformRoomID))
	}

	// 2. 调用 API 获取房间详情
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// --- 修改: 传递 fetchCtx ---
	roomInfoData, err := getBilibiliRoomDetailsFromAPI(fetchCtx, platformRoomID, cookieStr, logger)

	// 3. 构造 PlatformRoomInfoUpdatedEventJSON 事件
	var event PlatformRoomInfoUpdatedEventJSON
	if err != nil {
		// 处理获取失败的情况，发布 "未开播" 状态
		if strings.Contains(err.Error(), "room not found or not live") {
			logger.Warn("获取房间信息失败：房间不存在或未开播，将发布 '未开播' 状态 (JSON)", zap.Int("platform_room_id", platformRoomID), zap.Error(err))
			event = PlatformRoomInfoUpdatedEventJSON{
				RoomID:         internalRoomID,
				Platform:       bilibiliPlatform,
				PlatformRoomID: strconv.Itoa(platformRoomID),
				RoomName:       "", // 未知
				AnchorName:     "", // 未知
				AreaName:       "", // 未知
				LiveStatus:     0,  // 明确设置为未开播
				Timestamp:      time.Now().UnixMilli(),
			}
		} else {
			logger.Error("获取 Bilibili 房间详细信息失败，不发布事件", zap.Error(err), zap.Int("platform_room_id", platformRoomID))
			return // 其他错误不发布
		}
	} else {
		// 获取成功，构造完整事件
		event = PlatformRoomInfoUpdatedEventJSON{
			RoomID:         internalRoomID,
			Platform:       bilibiliPlatform,
			PlatformRoomID: strconv.Itoa(roomInfoData.RoomInfo.RoomID),
			RoomName:       roomInfoData.RoomInfo.Title,
			AnchorName:     roomInfoData.AnchorInfo.BaseInfo.Uname, // <-- 修改: 填充主播名称
			AreaName:       roomInfoData.RoomInfo.AreaName,
			LiveStatus:     int32(roomInfoData.RoomInfo.LiveStatus),
			Timestamp:      time.Now().UnixMilli(),
		}
	}

	// 4. 发布 PlatformRoomInfoUpdatedEventJSON 事件 (JSON 格式)
	s.publishPlatformRoomInfoJSONEvent(&event, logger)

	// 5. --- 新增: 根据 LiveStatus 发布 PlatformStreamStatusEventJSON 事件 ---
	streamStatusEvent := PlatformStreamStatusEventJSON{
		RoomID:       internalRoomID,
		IsLive:       event.LiveStatus == 1, // 1 表示直播中
		Timestamp:    event.Timestamp,
		RoomTitle:    event.RoomName,
		StreamerName: event.AnchorName,
		// SessionID 在开始时通常为空，由 session 服务生成；结束时可能需要提供
	}
	s.publishPlatformStreamStatusJSONEvent(&streamStatusEvent, logger)
	// --- 发布 Stream Status 事件结束 ---
}

// publishPlatformRoomInfoJSONEvent 辅助函数用于发布 PlatformRoomInfoUpdated 事件 (JSON 格式)
func (s *BilibiliAdapterService) publishPlatformRoomInfoJSONEvent(event *PlatformRoomInfoUpdatedEventJSON, logger *zap.Logger) {
	payload, err := json.Marshal(event) // 使用 JSON 序列化
	if err != nil {
		logger.Error("序列化 PlatformRoomInfoUpdated JSON 事件失败", zap.Error(err), zap.String("internal_room_id", event.RoomID))
		return
	}
	if err := s.natsConn.Publish(NatsPlatformRoomInfoUpdatedSubject, payload); err != nil {
		logger.Error("发布 PlatformRoomInfoUpdated JSON 事件到 NATS 失败", zap.Error(err), zap.String("subject", NatsPlatformRoomInfoUpdatedSubject), zap.String("internal_room_id", event.RoomID))
	} else {
		logger.Info("成功发布 PlatformRoomInfoUpdated JSON 事件到 NATS",
			zap.String("subject", NatsPlatformRoomInfoUpdatedSubject),
			zap.String("internal_room_id", event.RoomID),
			zap.Int32("live_status", event.LiveStatus),
			zap.String("room_name", event.RoomName),
			zap.String("anchor_name", event.AnchorName),
			zap.String("area_name", event.AreaName), // <-- 添加 area_name 到日志
		)
	}
}

// publishPlatformStreamStatusJSONEvent 辅助函数用于发布 PlatformStreamStatus 事件 (JSON 格式)
func (s *BilibiliAdapterService) publishPlatformStreamStatusJSONEvent(event *PlatformStreamStatusEventJSON, logger *zap.Logger) {
	payload, err := json.Marshal(event)
	if err != nil {
		logger.Error("序列化 PlatformStreamStatus JSON 事件失败", zap.Error(err), zap.String("internal_room_id", event.RoomID))
		return
	}
	// 注意: 使用 NatsPlatformStreamStatusSubject
	if err := s.natsConn.Publish(NatsPlatformStreamStatusSubject, payload); err != nil {
		logger.Error("发布 PlatformStreamStatus JSON 事件到 NATS 失败", zap.Error(err), zap.String("subject", NatsPlatformStreamStatusSubject), zap.String("internal_room_id", event.RoomID))
	} else {
		logger.Info("成功发布 PlatformStreamStatus JSON 事件到 NATS",
			zap.String("subject", NatsPlatformStreamStatusSubject),
			zap.String("internal_room_id", event.RoomID),
			zap.Bool("is_live", event.IsLive),
		)
	}
}

// --- 辅助函数 ---

// getBilibiliUIDFromCookie 从 Bilibili Cookie 字符串中解析 DedeUserID
func getBilibiliUIDFromCookie(cookieStr string) (int64, error) {
	parts := strings.Split(cookieStr, ";")
	for _, part := range parts {
		trimmedPart := strings.TrimSpace(part)
		if strings.HasPrefix(trimmedPart, "DedeUserID=") {
			value := strings.TrimPrefix(trimmedPart, "DedeUserID=")
			uid, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing DedeUserID '%s': %w", value, err)
			}
			return uid, nil
		}
	}
	return 0, errors.New("DedeUserID not found in cookie")
}

// getDanmuInfo 调用 Bilibili API 获取弹幕服务器信息和认证 token
func getDanmuInfo(ctx context.Context, roomID int, cookieStr string, logger *zap.Logger) (*DanmuConfData, error) {
	reqURL := fmt.Sprintf("%s?id=%d", danmuConfURL, roomID)
	logger.Debug("正在获取 Danmu 配置", zap.String("url", reqURL))

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", fmt.Sprintf("https://live.bilibili.com/%d", roomID))

	jar, _ := cookiejar.New(nil)
	apiURL, _ := url.Parse(danmuConfURL)
	var cookies []*http.Cookie
	parts := strings.Split(cookieStr, ";")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			cookies = append(cookies, &http.Cookie{Name: kv[0], Value: kv[1]})
		}
	}
	jar.SetCookies(apiURL, cookies)

	client := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		logger.Error("获取 Danmu 配置 API 返回非 200 状态码", zap.Int("status_code", resp.StatusCode), zap.String("url", reqURL), zap.ByteString("response_body", bodyBytes))
		return nil, fmt.Errorf("http request failed with status code %d", resp.StatusCode)
	}

	var danmuResp DanmuConfResponse
	if err := json.NewDecoder(resp.Body).Decode(&danmuResp); err != nil {
		return nil, fmt.Errorf("decoding json response: %w", err)
	}

	if danmuResp.Code != 0 {
		logger.Error("获取 Danmu 配置 API 返回错误码", zap.Int("code", danmuResp.Code), zap.String("message", danmuResp.Message), zap.String("url", reqURL))
		return nil, fmt.Errorf("api error code %d: %s", danmuResp.Code, danmuResp.Message)
	}

	logger.Info("成功获取 Danmu 配置", zap.String("token_prefix", danmuResp.Data.Token[:min(len(danmuResp.Data.Token), 5)]+"..."))
	return &danmuResp.Data, nil
}

// getBilibiliRoomDetailsFromAPI 获取 Bilibili 房间详细信息
func getBilibiliRoomDetailsFromAPI(ctx context.Context, roomID int, cookieStr string, logger *zap.Logger) (*RoomInfoData, error) {
	reqURL := fmt.Sprintf("%s?room_id=%d", roomInfoURL, roomID)
	logger.Debug("正在获取 Bilibili 房间信息", zap.String("url", reqURL))

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating http request for room info: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", fmt.Sprintf("https://live.bilibili.com/%d", roomID))

	var client *http.Client
	if cookieStr != "" {
		jar, _ := cookiejar.New(nil)
		apiURL, _ := url.Parse(roomInfoURL)
		var cookies []*http.Cookie
		parts := strings.Split(cookieStr, ";")
		for _, part := range parts {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				cookies = append(cookies, &http.Cookie{Name: kv[0], Value: kv[1]})
			}
		}
		jar.SetCookies(apiURL, cookies)
		client = &http.Client{
			Jar:     jar,
			Timeout: 10 * time.Second,
		}
		logger.Debug("为 getBilibiliRoomDetailsFromAPI 请求设置了 Cookie")
	} else {
		client = &http.Client{
			Timeout: 10 * time.Second,
		}
		logger.Debug("未提供 Cookie，getBilibiliRoomDetailsFromAPI 请求未使用 Cookie")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing http request for room info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		logger.Error("获取 Bilibili 房间信息 API 返回非 200 状态码", zap.Int("status_code", resp.StatusCode), zap.String("url", reqURL), zap.ByteString("response_body", bodyBytes))
		return nil, fmt.Errorf("room info http request failed with status code %d", resp.StatusCode)
	}

	var roomInfoResp RoomInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&roomInfoResp); err != nil {
		logger.Error("解码 Bilibili 房间信息 JSON 响应失败", zap.Error(err))
		return nil, fmt.Errorf("decoding room info json response: %w", err)
	}

	if roomInfoResp.Code != 0 {
		logger.Error("获取 Bilibili 房间信息 API 返回错误码", zap.Int("code", roomInfoResp.Code), zap.String("message", roomInfoResp.Message), zap.String("url", reqURL))
		if roomInfoResp.Code == 60004 {
			return nil, fmt.Errorf("room not found or not live (api code 60004)")
		}
		return nil, fmt.Errorf("room info api error code %d: %s", roomInfoResp.Code, roomInfoResp.Message)
	}

	if roomInfoResp.Data == nil || roomInfoResp.Data.RoomInfo.RoomID == 0 {
		logger.Error("获取 Bilibili 房间信息 API 返回空的 data 或无效的 room_info", zap.String("url", reqURL), zap.Any("response", roomInfoResp))
		return nil, errors.New("room info api returned empty or invalid data")
	}

	logger.Info("成功获取 Bilibili 房间信息",
		zap.Int("room_id", roomInfoResp.Data.RoomInfo.RoomID),
		zap.Int("live_status", roomInfoResp.Data.RoomInfo.LiveStatus),
		zap.String("title", roomInfoResp.Data.RoomInfo.Title),
		zap.String("anchor_uname", roomInfoResp.Data.AnchorInfo.BaseInfo.Uname),
	)
	return roomInfoResp.Data, nil
}

// min 返回两个整数中较小的一个 (辅助函数)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isEventSessionRequired 判断给定的 Bilibili 事件 cmd 是否强依赖于 Session ID
// 对于这些事件，如果无法获取到 Session ID，则不应处理或发布。
func isEventSessionRequired(cmd string) bool {
	switch cmd {
	case "DANMU_MSG", // 弹幕消息
		"SEND_GIFT",             // 礼物赠送
		"SUPER_CHAT_MESSAGE",    // 醒目留言 (SC)
		"GUARD_BUY",             // 上舰/提督/总督购买
		"USER_TOAST_MSG",        // 用户互动提示 (例如舰长购买)
		"NOTICE_MSG",            // 系统通知 (可能包含与会话相关的统计信息)
		"LIVE_INTERACTIVE_GAME", // 互动游戏 (例如天选之人) - 可能需要关联会话
		"WATCHED_CHANGE",        // 看过人数变化 (通常与会话关联)
		"LIKE_INFO_V3_UPDATE",   // 点赞数更新 (通常与会话关联)
		"ONLINE_RANK_COUNT",     // 在线排名人数
		"ONLINE_RANK_V2",        // 新版在线排名
		"STOP_LIVE_ROOM_LIST",   // 停止直播的房间列表 (这个可能不需要，但保守起见加入)
		"WIDGET_BANNER":         // 小部件横幅 (可能与活动有关)
		return true
	// 以下事件通常不强依赖 Session ID 或发生在 Session 创建之前/独立于 Session
	case "INTERACT_WORD", // 用户进入直播间
		"ENTRY_EFFECT",     // 入场特效
		"ONLINE_RANK_TOP3", // 在线排名前三 (可能独立于具体会话)
		"HOT_RANK_CHANGED", // 热门榜单变化
		"HOT_RANK_CHANGED_V2",
		"PREPARING",                     // 直播准备中
		"LIVE",                          // 直播开始 (Session 创建的触发器之一)
		"ROOM_REAL_TIME_MESSAGE_UPDATE", // 房间实时消息更新 (可能是通用的)
		"ROOM_CHANGE",                   // 房间信息变更 (例如标题)
		"ROOM_BLOCK_MSG",                // 房间封禁消息
		"POPULARITY_RED_POCKET_NEW",     // 人气红包-新
		"POPULARITY_RED_POCKET_START",   // 人气红包-开始
		"COMMON_NOTICE_DANMAKU",         // 通用通知弹幕
		"VOICE_JOIN_STATUS",             // 连麦状态
		"VOICE_JOIN_ROOM_COUNT_INFO":    // 连麦房间人数信息
		return false
	default:
		// 对于未明确列出的命令，保守起见，假设它可能需要 Session ID
		// 但为了避免过度阻塞，这里返回 false，让其按原逻辑处理（可能 sessionID 为空）
		// 如果发现新的命令确实需要 Session ID，应将其添加到上面的 true 分支
		return false
	}
}

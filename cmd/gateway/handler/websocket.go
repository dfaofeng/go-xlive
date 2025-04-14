package handler // 包名应为 handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	// !!! 替换模块路径 !!!
	realtimev1 "go-xlive/gen/go/realtime/v1" // 需要导入 Realtime 服务定义
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson" // 用于将收到的 Protobuf 转 JSON
)

const (
	// WebSocket 相关常量
	writeWait      = 10 * time.Second    // 允许写入消息到对端的时间
	pongWait       = 60 * time.Second    // 允许从对端读取下一个 pong 消息的时间
	pingPeriod     = (pongWait * 9) / 10 // 发送 ping 到对端的时间间隔 (必须小于 pongWait)
	maxMessageSize = 1024                // 允许从对端接收的消息最大尺寸
)

// Client 代表一个 WebSocket 客户端连接
type Client struct {
	hub       *Hub            // 指向所属的 Hub
	sessionID string          // 客户端关联的 Session ID
	conn      *websocket.Conn // WebSocket 连接指针
	send      chan []byte     // 带缓冲的消息发送通道
}

// Hub 维护活跃的 WebSocket 客户端集合，并处理消息广播
type Hub struct {
	// 使用读写锁保护并发访问 clients map
	mu         sync.RWMutex
	clients    map[string]map[*Client]bool // sessionID -> Client Set (指向 Client 指针)
	broadcast  chan *Message               // 从 NATS 来的需要广播的消息 (由 main.go 里的 NATS 订阅者放入)
	register   chan *Client                // 注册新客户端的通道
	unregister chan *Client                // 注销客户端的通道
	logger     *zap.Logger
}

// Message 结构用于在 Hub 内部传递需要广播的消息
type Message struct {
	SessionID string // 目标 Session ID
	Data      []byte // 消息内容 (通常是序列化后的 JSON 或 Protobuf)
}

// NewHub 创建一个新的 Hub 实例
func NewHub(logger *zap.Logger) *Hub {
	return &Hub{
		broadcast:  make(chan *Message, 512), // 增加广播通道缓冲区
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[string]map[*Client]bool), // 初始化 map
		logger:     logger.Named("websocket_hub"),     // 给 Hub 的 logger 加个名字
	}
}

// Run 启动 Hub 的主事件循环，处理客户端注册、注销和消息广播
func (h *Hub) Run() {
	h.logger.Info("WebSocket Hub 事件循环已启动")
	for {
		select {
		case client := <-h.register: // 处理新客户端注册
			h.registerClient(client)
		case client := <-h.unregister: // 处理客户端注销
			h.unregisterClient(client)
		case message := <-h.broadcast: // 处理来自 NATS 的广播消息
			h.broadcastMessage(message)
		}
	}
}

// registerClient 处理客户端注册逻辑
func (h *Hub) registerClient(client *Client) {
	h.mu.Lock() // 获取写锁以修改 clients map
	defer h.mu.Unlock()
	if _, ok := h.clients[client.sessionID]; !ok {
		// 如果这个 sessionID 还没有客户端列表，创建一个新的
		h.clients[client.sessionID] = make(map[*Client]bool)
	}
	h.clients[client.sessionID][client] = true // 将客户端添加到对应 session 的集合中
	h.logger.Info("WebSocket 客户端已注册",
		zap.String("session_id", client.sessionID),
		zap.String("remote_addr", client.conn.RemoteAddr().String()),
		zap.Int("current_session_clients", len(h.clients[client.sessionID])))
}

// unregisterClient 处理客户端注销逻辑
func (h *Hub) unregisterClient(client *Client) {
	h.mu.Lock() // 获取写锁
	defer h.mu.Unlock()
	if sessionClients, ok := h.clients[client.sessionID]; ok {
		if _, clientExists := sessionClients[client]; clientExists {
			// 从 session 的客户端集合中删除该客户端
			delete(sessionClients, client)
			// 关闭该客户端的发送通道，防止向已关闭的连接发送数据
			// 需要检查通道是否已关闭，避免 double close panic
			select {
			case <-client.send: // 如果通道已关闭，会立即返回
			default:
				close(client.send)
			}

			// 如果这个 session 没有其他客户端了，从 Hub 中移除该 session 条目
			if len(sessionClients) == 0 {
				delete(h.clients, client.sessionID)
				h.logger.Info("移除了 Session 的最后一个客户端", zap.String("session_id", client.sessionID))
			}
			h.logger.Info("WebSocket 客户端已注销",
				zap.String("session_id", client.sessionID),
				zap.String("remote_addr", client.conn.RemoteAddr().String()),
				zap.Int("remaining_session_clients", len(sessionClients)))
		}
	}
}

// broadcastMessage 将消息广播给特定 sessionID 的所有订阅者
func (h *Hub) broadcastMessage(message *Message) {
	h.mu.RLock() // 获取读锁，允许并发广播到不同 session
	// 查找该消息对应的 session 是否有活跃的客户端
	subscribers, ok := h.clients[message.SessionID]
	if !ok || len(subscribers) == 0 {
		h.mu.RUnlock()
		// h.logger.Debug("无需广播：无订阅者", zap.String("session_id", message.SessionID))
		return
	}

	// 复制一份订阅者列表以避免长时间持有读锁（如果发送耗时）
	clientsToSend := make([]*Client, 0, len(subscribers))
	for client := range subscribers {
		clientsToSend = append(clientsToSend, client)
	}
	h.mu.RUnlock() // 释放读锁

	h.logger.Debug("准备广播消息",
		zap.String("session_id", message.SessionID),
		zap.Int("target_count", len(clientsToSend)),
		// zap.ByteString("data", message.Data), // 避免记录完整数据
		zap.Int("data_len", len(message.Data)),
	)

	// 向复制出来的列表中的客户端发送事件
	for _, client := range clientsToSend {
		// 这里使用非阻塞发送，如果发送失败（例如客户端已断开），则移除订阅
		select {
		case client.send <- message.Data:
			// 成功放入通道
		default:
			// 如果客户端的发送通道已满或已关闭
			h.logger.Warn("客户端发送通道已满或关闭，准备注销慢客户端",
				zap.String("session_id", client.sessionID),
				zap.String("remote_addr", client.conn.RemoteAddr().String()))
			// 异步触发注销流程
			go func(c *Client) {
				h.unregister <- c // 将客户端放入注销通道
			}(client)
		}
	}
}

// --- WebSocket Upgrader ---
// 用于将 HTTP 连接升级到 WebSocket
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// CheckOrigin 用于检查请求来源，防止跨站 WebSocket 劫持
	CheckOrigin: func(r *http.Request) bool {
		// 生产环境应该实现更严格的来源检查，例如只允许来自特定域名的请求
		return true // 开发环境暂时允许所有来源
	},
}

// ServeWs 处理传入的 WebSocket 请求，升级连接，并桥接到 RealtimeService gRPC 流
// 这个函数现在由 router 调用
func ServeWs(hub *Hub, logger *zap.Logger, realtimeClient realtimev1.RealtimeServiceClient, w http.ResponseWriter, r *http.Request) {
	// --- 提取 Session ID ---
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	var sessionID string
	if len(pathParts) == 3 && pathParts[0] == "ws" && pathParts[1] == "sessions" {
		sessionID = pathParts[2]
	}
	if sessionID == "" {
		logger.Error("无法从 WebSocket 路径中提取 session_id", zap.String("path", r.URL.Path))
		http.Error(w, "错误的请求：路径中缺少或无效的 session_id", http.StatusBadRequest)
		return
	}

	// TODO: 在这里添加用户认证逻辑，例如检查 r.Header 中的 Token

	logger.Info("收到 WebSocket 连接请求", zap.String("session_id", sessionID), zap.String("remote_addr", r.RemoteAddr))

	// --- 升级到 WebSocket 连接 ---
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket 升级失败", zap.Error(err), zap.String("remote_addr", r.RemoteAddr))
		return
	}
	defer conn.Close()
	logger.Info("WebSocket 连接已建立", zap.String("session_id", sessionID), zap.String("remote_addr", conn.RemoteAddr().String()))

	// --- 创建此 WebSocket 连接对应的 Client 实例 ---
	client := &Client{
		hub:       hub, // 关联到 Hub
		sessionID: sessionID,
		conn:      conn,
		send:      make(chan []byte, 256), // 创建带缓冲的发送通道
	}
	// 将新客户端注册到 Hub
	client.hub.register <- client

	// --- 连接到 RealtimeService 的 gRPC 流 ---
	// 使用请求的 Context，这样如果 HTTP 请求被取消，gRPC 流也会被取消
	streamCtx, streamCancel := context.WithCancel(r.Context())
	defer streamCancel() // 确保 gRPC 流最终被取消

	subscribeReq := &realtimev1.SubscribeSessionEventsRequest{
		SessionId: sessionID,
		// TODO: 传递认证后的用户信息（如果需要 RealtimeService 验证）
	}
	// 调用 RealtimeService 的流式 RPC 方法
	stream, err := realtimeClient.SubscribeSessionEvents(streamCtx, subscribeReq)
	if err != nil {
		logger.Error("无法订阅 RealtimeService 事件流", zap.Error(err), zap.String("session_id", sessionID))
		// 订阅失败，通知 WebSocket 客户端并关闭连接
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "无法连接实时服务"))
		// 既然连接 Realtime 失败，也需要从 Hub 注销此 Client
		client.hub.unregister <- client
		return
	}
	logger.Info("已成功订阅 RealtimeService 事件流", zap.String("session_id", sessionID))

	// --- 启动读写 Goroutine ---
	// 启动一个 Goroutine 将来自 Hub 的消息写入 WebSocket
	go client.writePump()
	// 启动一个 Goroutine 将来自 gRPC Stream 的消息放入 Hub 的广播通道
	go client.grpcReadPump(stream)

	// 在当前 Goroutine 中处理从 WebSocket 读取消息（主要是心跳和关闭检测）
	client.readPump()
}

// readPump 从 WebSocket 读取消息 (处理 Pong 和检测关闭)
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c // 请求 Hub 注销自己
		_ = c.conn.Close()    // 关闭 WebSocket 连接
		c.hub.logger.Info("WS readPump 退出", zap.String("sid", c.sessionID), zap.String("raddr", c.conn.RemoteAddr().String()))
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { _ = c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				c.hub.logger.Error("WS 读取错误", zap.Error(err), zap.String("sid", c.sessionID))
			} else {
				c.hub.logger.Info("WS 连接关闭或读取超时", zap.Error(err), zap.String("sid", c.sessionID))
			}
			break // 退出循环，触发 defer
		}
		// 通常忽略业务消息
	}
}

// writePump 从 client.send channel 读取消息并写入 WebSocket (处理 Ping)
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close() // 关闭连接
		// 不需要在这里 unregister，readPump 退出时会处理
		c.hub.logger.Info("WS writePump 退出", zap.String("sid", c.sessionID), zap.String("raddr", c.conn.RemoteAddr().String()))
	}()
	for {
		select {
		case message, ok := <-c.send: // 从 Hub 广播或直接发送的消息
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub 关闭了通道
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil { // 假设都是文本消息 (JSON)
				c.hub.logger.Error("写入 WS 消息失败", zap.Error(err), zap.String("sid", c.sessionID))
				return // 写入失败，退出
			}
		case <-ticker.C: // 定时发送 Ping
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.hub.logger.Error("发送 WS Ping 失败", zap.Error(err), zap.String("sid", c.sessionID))
				return // Ping 失败，连接可能断开
			}
			c.hub.logger.Debug("已发送 Ping", zap.String("sid", c.sessionID), zap.String("raddr", c.conn.RemoteAddr().String()))
		}
	}
}

// grpcReadPump 从 RealtimeService 的 gRPC 流读取事件，并放入 Hub 的广播通道
func (c *Client) grpcReadPump(stream realtimev1.RealtimeService_SubscribeSessionEventsClient) {
	defer func() {
		c.hub.unregister <- c // 如果 gRPC 流结束，也注销客户端
		c.hub.logger.Info("gRPC readPump 退出", zap.String("sid", c.sessionID))
	}()
	for {
		event, err := stream.Recv() // 从 gRPC 流接收事件
		if err != nil {
			// 处理流结束或错误
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || status.Code(err) == codes.Unavailable {
				c.hub.logger.Info("gRPC 事件流关闭或取消 (grpcReadPump)", zap.Error(err), zap.String("session_id", c.sessionID))
			} else {
				c.hub.logger.Error("从 gRPC 事件流接收错误 (grpcReadPump)", zap.Error(err), zap.String("session_id", c.sessionID))
			}
			return // 退出 goroutine
		}

		// 将接收到的 Protobuf 事件转换为 JSON 发送给 WebSocket 客户端
		jsonBytes, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(event)
		if err != nil {
			c.hub.logger.Error("序列化事件为 JSON 失败 (grpcReadPump)", zap.Error(err), zap.String("session_id", c.sessionID))
			continue // 跳过这条消息
		}

		// 将准备好的 JSON 放入 Hub 的广播通道
		// 注意：这里假设所有从 RealtimeService 收到的消息都需要广播
		// 如果 RealtimeService 已经做了过滤，这里可以直接广播
		// 如果需要网关再做一层过滤或处理，在这里进行
		c.hub.broadcast <- &Message{
			SessionID: c.sessionID,
			Data:      jsonBytes,
		}
	}
}

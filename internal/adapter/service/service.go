package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv" // 用于将 Bilibili UID (数字) 转换为字符串
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1"
)

// AdapterService 结构体
type AdapterService struct {
	logger   *zap.Logger
	natsConn *nats.Conn
	httpPort int
	server   *http.Server // HTTP 服务器实例
}

// NewAdapterService 创建 AdapterService 实例
func NewAdapterService(logger *zap.Logger, nc *nats.Conn, httpPort int) *AdapterService {
	return &AdapterService{
		logger:   logger.Named("adapter_service"),
		natsConn: nc,
		httpPort: httpPort,
	}
}

// RunHTTPServer 启动 HTTP Webhook 服务器
func (s *AdapterService) RunHTTPServer(wg *sync.WaitGroup) <-chan error {
	errChan := make(chan error, 1)
	addr := fmt.Sprintf(":%d", s.httpPort)
	mux := http.NewServeMux()

	// --- 定义 Webhook 处理端点 ---
	mux.HandleFunc("/webhooks/bilibili", s.handleBilibiliWebhook) // 修改为 Bilibili 端点

	s.server = &http.Server{
		Addr:    addr,
		Handler: mux,
		// 可以添加 ReadTimeout, WriteTimeout, IdleTimeout 等
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	wg.Add(1) // 增加 WaitGroup 计数
	go func() {
		defer wg.Done() // 确保 Goroutine 退出时减少计数
		s.logger.Info("启动 Webhook HTTP 服务器", zap.String("address", addr))
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("Webhook HTTP 服务器启动失败", zap.Error(err))
			errChan <- err
		}
		close(errChan)
	}()

	return errChan
}

// ShutdownHTTPServer 优雅关闭 HTTP 服务器
func (s *AdapterService) ShutdownHTTPServer(ctx context.Context) {
	if s.server == nil {
		return
	}
	s.logger.Info("正在关闭 Webhook HTTP 服务器...")
	if err := s.server.Shutdown(ctx); err != nil {
		s.logger.Error("关闭 Webhook HTTP 服务器失败", zap.Error(err))
	} else {
		s.logger.Info("Webhook HTTP 服务器已关闭")
	}
}

// handleBilibiliWebhook 处理来自模拟 Bilibili 平台的 Webhook 请求
func (s *AdapterService) handleBilibiliWebhook(w http.ResponseWriter, r *http.Request) { // 重命名
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// --- 实现基础的请求验证 (检查请求头) ---
	expectedSecret := "your-very-secret-token" // 实际应用中应来自配置
	receivedSecret := r.Header.Get("X-Platform-Secret")
	if receivedSecret != expectedSecret {
		s.logger.Warn("Webhook 请求验证失败", zap.String("received_secret", receivedSecret))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// --- 验证通过 ---

	s.logger.Info("收到 Bilibili Webhook 请求", zap.String("remote_addr", r.RemoteAddr), zap.String("path", r.URL.Path)) // 更新日志

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("读取 Webhook 请求体失败", zap.Error(err))
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// --- 解析和转换事件 ---
	// 假设平台发送的是 JSON 格式
	var platformEvent map[string]interface{} // 使用 map[string]interface{} 灵活处理
	if err := json.Unmarshal(body, &platformEvent); err != nil {
		s.logger.Error("解析 Webhook JSON 失败", zap.Error(err), zap.ByteString("body", body))
		http.Error(w, "Invalid JSON format", http.StatusBadRequest)
		return
	}

	// 根据平台事件内容，转换为内部 Protobuf 事件
	internalEvent, natsSubject := s.convertBilibiliEvent(platformEvent) // 调用新的转换函数
	if internalEvent == nil || natsSubject == "" {
		s.logger.Warn("无法将 Bilibili 事件转换为内部事件或确定 NATS 主题", zap.Any("platform_event", platformEvent))
		// 返回成功，因为我们收到了请求，只是无法处理
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Event received but not processed"))
		return
	}

	// --- 发布到 NATS ---
	payload, err := proto.Marshal(internalEvent)
	if err != nil {
		s.logger.Error("序列化内部事件失败", zap.Error(err), zap.String("subject", natsSubject))
		http.Error(w, "Failed to serialize internal event", http.StatusInternalServerError)
		return
	}

	if err := s.natsConn.Publish(natsSubject, payload); err != nil {
		s.logger.Error("发布事件到 NATS 失败", zap.Error(err), zap.String("subject", natsSubject))
		http.Error(w, "Failed to publish event to NATS", http.StatusInternalServerError)
		return
	}

	s.logger.Info("成功处理并发布 Bilibili 事件到 NATS", zap.String("subject", natsSubject)) // 更新日志
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Event processed successfully"))
}

// convertBilibiliEvent 将模拟的 Bilibili 事件转换为内部 Protobuf 事件和 NATS 主题
// 返回: proto.Message (内部事件), string (NATS 主题)
func (s *AdapterService) convertBilibiliEvent(platformEvent map[string]interface{}) (proto.Message, string) {
	cmd, _ := platformEvent["cmd"].(string) // Bilibili 使用 cmd 字段

	// 从 Bilibili 事件中提取房间 ID (通常是数字)
	roomIDFloat, okRoom := platformEvent["roomid"].(float64) // JSON 数字通常是 float64
	if !okRoom {
		s.logger.Warn("Bilibili 事件缺少或无效的 roomid", zap.Any("event", platformEvent))
		return nil, ""
	}
	roomIDStr := strconv.FormatFloat(roomIDFloat, 'f', -1, 64) // 转换为字符串

	// 生成临时的 sessionID (因为 Bilibili Webhook 通常不直接提供 session 概念)
	sessionID := fmt.Sprintf("bili_room_%s", roomIDStr)

	now := timestamppb.Now()

	switch cmd {
	case "INTERACT_WORD": // 用户进入直播间事件
		data, okData := platformEvent["data"].(map[string]interface{})
		if !okData {
			return nil, ""
		}
		uidFloat, okUID := data["uid"].(float64)
		if !okUID {
			return nil, ""
		}
		userIDStr := strconv.FormatFloat(uidFloat, 'f', -1, 64)

		event := &eventv1.UserPresence{
			SessionId: sessionID,
			RoomId:    roomIDStr,
			UserId:    userIDStr,
			Entered:   true,
			Timestamp: now,
			// CurrentOnlineCount: -1, // Bilibili 进入事件通常不直接提供总在线人数
		}
		return event, "events.raw.user.presence"

	case "DANMU_MSG": // 弹幕消息事件
		info, okInfo := platformEvent["info"].([]interface{}) // 弹幕信息在 info 数组中
		if !okInfo || len(info) < 3 {
			return nil, ""
		}

		// 提取弹幕内容
		content, okContent := info[1].(string)
		if !okContent {
			return nil, ""
		}

		// 提取用户信息数组
		userInfo, okUserInfo := info[2].([]interface{})
		if !okUserInfo || len(userInfo) < 1 {
			return nil, ""
		}

		// 提取用户 UID
		uidFloat, okUID := userInfo[0].(float64)
		if !okUID {
			return nil, ""
		}
		userIDStr := strconv.FormatFloat(uidFloat, 'f', -1, 64)

		event := &eventv1.ChatMessage{
			MessageId: uuid.NewString(), // 生成新的消息 ID
			SessionId: sessionID,
			RoomId:    roomIDStr,
			UserId:    userIDStr,
			Content:   content,
			Timestamp: now,
		}
		return event, "events.raw.chat.message"

	// TODO: 添加对其他 Bilibili cmd (如 SEND_GIFT, LIVE 等) 的处理
	// case "SEND_GIFT":
	// ...

	default:
		s.logger.Debug("收到未知的 Bilibili cmd 类型", zap.String("cmd", cmd))
		return nil, ""
	}
}

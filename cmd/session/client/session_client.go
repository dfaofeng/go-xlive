package client

import (
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure" // 用于本地非加密连接
)

// SessionClientWrapper 封装 Session Service 客户端连接
// (与 RoomClientWrapper 类似，但针对 Session Service)
type SessionClientWrapper struct {
	ClientConn *grpc.ClientConn
	// 可以添加 SessionServiceClient 实例如果需要
}

// NewSessionClientSelf 创建连接到 Session Service 自身 gRPC 服务器的客户端
// port: Session Service gRPC 服务器监听的端口
func NewSessionClientSelf(port int) (*SessionClientWrapper, error) {
	addr := fmt.Sprintf("localhost:%d", port) // 假设服务监听在 localhost
	// 使用非安全凭证，因为是内部调用
	conn, err := grpc.Dial(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),                // 阻塞直到连接成功或超时
		grpc.WithTimeout(5*time.Second), // 设置连接超时
	)
	if err != nil {
		return nil, fmt.Errorf("无法连接到内部 Session Service gRPC 服务器 at %s: %w", addr, err)
	}

	// 可以选择在这里创建 SessionServiceClient 实例
	// client := sessionv1.NewSessionServiceClient(conn)

	return &SessionClientWrapper{
		ClientConn: conn,
		// Client: client,
	}, nil
}

// Close 关闭 gRPC 连接
func (w *SessionClientWrapper) Close() {
	if w.ClientConn != nil {
		w.ClientConn.Close()
	}
}

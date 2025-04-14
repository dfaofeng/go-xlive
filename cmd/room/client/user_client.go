// cmd/room/client/user_client.go
package client

import (
	"fmt"
	"log" // 用于关键初始化错误
	"strings"

	// !!! 替换模块路径 !!!
	"go-xlive/configs"
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials/insecure"
)

// UserClient holds the gRPC client for User Service
type UserClient struct {
	Client userv1.UserServiceClient
	Conn   *grpc.ClientConn
}

// NewUserClient creates a new connection and client for the User Service
func NewUserClient(cfg configs.ClientConfig) (*UserClient, error) {
	userSvcAddresses := cfg.UserService.Addresses
	if len(userSvcAddresses) == 0 {
		userSvcAddresses = []string{"localhost:50051"}
	} // 默认值
	userSvcTarget := "passthrough:///" + strings.Join(userSvcAddresses, ",")
	log.Printf("准备连接到用户服务 Target: %s\n", userSvcTarget) // 使用标准 log，因为此时 Zap 可能未初始化

	otelClientHandler := otelgrpc.NewClientHandler(
		otelgrpc.WithTracerProvider(otel.GetTracerProvider()),
		otelgrpc.WithPropagators(otel.GetTextMapPropagator()),
	)
	loadBalancingConfig := fmt.Sprintf(`{"loadBalancingConfig": [{"%s":{}}]}`, roundrobin.Name)
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelClientHandler),
		grpc.WithChainUnaryInterceptor(
			observability.MetricsUnaryClientInterceptor(),
		),
		grpc.WithDefaultServiceConfig(loadBalancingConfig),
	}

	conn, err := grpc.Dial(userSvcTarget, opts...)
	if err != nil {
		return nil, fmt.Errorf("连接用户服务失败 (%s): %w", userSvcTarget, err)
	}

	log.Printf("已连接到用户服务: %v\n", userSvcAddresses)
	return &UserClient{
		Client: userv1.NewUserServiceClient(conn),
		Conn:   conn,
	}, nil
}

// Close closes the underlying gRPC connection
func (uc *UserClient) Close() error {
	if uc.Conn != nil {
		log.Println("正在关闭用户服务客户端连接...")
		return uc.Conn.Close()
	}
	return nil
}

// cmd/session/client/room_client.go
package client

import (
	"fmt"
	"log"
	"strings"

	// !!! 替换模块路径 !!!
	"go-xlive/configs"
	roomv1 "go-xlive/gen/go/room/v1" // 导入 Room 服务 proto
	"go-xlive/pkg/observability"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials/insecure"
)

// RoomClient holds the gRPC client for Room Service
type RoomClient struct {
	Client roomv1.RoomServiceClient
	Conn   *grpc.ClientConn
}

// NewRoomClient creates a new connection and client for the Room Service
func NewRoomClient(cfg configs.ClientConfig) (*RoomClient, error) {
	roomSvcAddresses := cfg.RoomService.Addresses
	if len(roomSvcAddresses) == 0 {
		roomSvcAddresses = []string{"localhost:50052"}
	} // 默认值
	roomSvcTarget := "passthrough:///" + strings.Join(roomSvcAddresses, ",")
	log.Printf("准备连接到房间服务 Target: %s\n", roomSvcTarget)

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

	conn, err := grpc.Dial(roomSvcTarget, opts...)
	if err != nil {
		return nil, fmt.Errorf("连接房间服务失败 (%s): %w", roomSvcTarget, err)
	}

	log.Printf("已连接到房间服务: %v\n", roomSvcAddresses)
	return &RoomClient{
		Client: roomv1.NewRoomServiceClient(conn),
		Conn:   conn,
	}, nil
}

// Close closes the underlying gRPC connection
func (rc *RoomClient) Close() error {
	if rc.Conn != nil {
		log.Println("正在关闭房间服务客户端连接...")
		return rc.Conn.Close()
	}
	return nil
}

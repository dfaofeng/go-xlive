package main

import (
	"context"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sessionv1 "go-xlive/gen/go/session/v1"
)

// registerGatewayHandlers 注册所有 gRPC 网关处理程序
func registerGatewayHandlers(ctx context.Context, mux *runtime.ServeMux, grpcServerEndpoint string) error {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	// 注册 Session 服务
	if err := sessionv1.RegisterSessionServiceHandlerFromEndpoint(ctx, mux, grpcServerEndpoint, opts); err != nil {
		return err
	}

	return nil
}

// newGatewayMux 创建并配置新的网关 Mux
func newGatewayMux(ctx context.Context, grpcServerEndpoint string) (*runtime.ServeMux, error) {
	mux := runtime.NewServeMux(
		runtime.WithErrorHandler(runtime.DefaultHTTPErrorHandler),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{}),
	)

	if err := registerGatewayHandlers(ctx, mux, grpcServerEndpoint); err != nil {
		return nil, err
	}

	return mux, nil
}

// cmd/gateway/router/router.go
package router

import (
	"encoding/json"
	"net/http"

	"go-xlive/cmd/gateway/handler"    // 导入 handler 包
	"go-xlive/cmd/gateway/middleware" // 导入 middleware 包
	// !!! 替换模块路径 !!!
	realtimev1 "go-xlive/gen/go/realtime/v1"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
)

// NewRouter 创建并配置网关的主 HTTP 路由器
func NewRouter(
	logger *zap.Logger,
	realtimeClient realtimev1.RealtimeServiceClient, // 传入 Realtime 客户端
	restMux *runtime.ServeMux, // 传入已注册 handler 的 gRPC-Gateway Mux
	wsHub *handler.Hub, // 传入 WebSocket Hub
) http.Handler {

	mainMux := http.NewServeMux()

	// --- WebSocket 路由 ---
	mainMux.HandleFunc("/ws/sessions/", func(w http.ResponseWriter, r *http.Request) {
		handler.ServeWs(wsHub, logger, realtimeClient, w, r) // 调用 handler 中的 ServeWs
	})

	// --- REST API 路由 (应用中间件) ---
	// 1. OTEL 中间件
	otelRestHandler := otelhttp.NewHandler(restMux, "api-gateway-rest",
		otelhttp.WithTracerProvider(otel.GetTracerProvider()),
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
	)
	// 2. 统一响应中间件
	unifiedHandler := middleware.UnifiedResponseMiddleware(otelRestHandler)

	// 3. 挂载到 /v1/ 前缀
	mainMux.Handle("/v1/", unifiedHandler)

	// --- 根路径处理 ---
	mainMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "API Gateway is running"})
	})

	return mainMux // 返回配置好的主路由器
}

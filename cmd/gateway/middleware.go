package main // 或者放到 pkg/middleware 包

import (
	"bytes"
	"encoding/json"
	"net/http"
	// !!! 替换模块路径 !!!
	"go-xlive/pkg/response"          // 导入定义的响应结构体包
	"go.opentelemetry.io/otel/trace" // 用于获取 Trace ID
)

// responseWriterInterceptor 包装 http.ResponseWriter 以拦截状态码和响应体
type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

// NewResponseWriterInterceptor 创建一个新的拦截器
func NewResponseWriterInterceptor(w http.ResponseWriter) *responseWriterInterceptor {
	return &responseWriterInterceptor{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // 默认状态码
		body:           new(bytes.Buffer),
	}
}

// WriteHeader 拦截设置状态码
func (w *responseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	// 不直接调用原始的 WriteHeader，在最后统一设置
}

// Write 拦截写入响应体
func (w *responseWriterInterceptor) Write(b []byte) (int, error) {
	return w.body.Write(b) // 写入内部缓冲区
}

// UnifiedResponseMiddleware 是实现统一响应格式的中间件
func UnifiedResponseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 创建 response writer 拦截器
		interceptor := NewResponseWriterInterceptor(w)

		// 调用下一个处理器 (可能是 otelHandler 或 restMux)
		next.ServeHTTP(interceptor, r)

		// --- 处理响应 ---
		var finalResponse *response.UnifiedResponse
		originalBody := interceptor.body.Bytes()
		statusCode := interceptor.statusCode

		// 从 Context 获取 Trace ID
		traceID := trace.SpanContextFromContext(r.Context()).TraceID().String()

		if statusCode >= 200 && statusCode < 300 { // 成功响应
			var responseData interface{}
			// 尝试将原始响应体解析为 JSON，如果失败则作为字符串处理
			if err := json.Unmarshal(originalBody, &responseData); err != nil {
				// 如果原始响应不是 JSON，可能是一个简单的字符串或空响应
				// 对于 grpc-gateway 返回的空成功响应 (例如 Delete)，body 可能为空
				if len(originalBody) == 0 {
					responseData = nil
				} else {
					responseData = originalBody // 或者 responseData = string(originalBody)
				}

			}
			finalResponse = response.Success(responseData)
		} else { // 错误响应
			var errResp struct { // 尝试解析 grpc-gateway 的错误格式
				Error   string        `json:"error"`   // gRPC 错误信息 (deprecated)
				Message string        `json:"message"` // gRPC 错误信息
				Code    int           `json:"code"`    // gRPC 状态码
				Details []interface{} `json:"details"` // gRPC 错误详情 (可选)
			}
			errMsg := "发生未知错误"
			errCode := statusCode // 默认使用 HTTP 状态码

			if err := json.Unmarshal(originalBody, &errResp); err == nil {
				// 成功解析 grpc-gateway 错误
				if errResp.Message != "" {
					errMsg = errResp.Message
				} else if errResp.Error != "" {
					errMsg = errResp.Error
				}
				// 可以将 gRPC 状态码映射到自定义业务错误码
				// errCode = mapGrpcErrorCodeToBusinessCode(errResp.Code)
				errCode = errResp.Code // 直接使用 gRPC 状态码作为内部码示例
				finalResponse = response.Fail(errCode, errMsg, errResp.Details)
			} else {
				// 无法解析错误体，使用 HTTP 状态文本
				errMsg = http.StatusText(statusCode)
				if len(originalBody) > 0 {
					// 可以将原始 body 作为 details 返回
					finalResponse = response.Fail(errCode, errMsg, string(originalBody))
				} else {
					finalResponse = response.Fail(errCode, errMsg)
				}
			}
		}

		// 设置 Trace ID
		if traceID != "" && traceID != "00000000000000000000000000000000" {
			finalResponse.TraceID = traceID
		}

		// --- 将最终的统一响应写入原始 ResponseWriter ---
		finalJson, err := json.Marshal(finalResponse)
		if err != nil {
			// 如果连最终响应都序列化失败，返回一个标准的服务器错误
			http.Error(w, `{"success":false,"code":500,"message":"无法序列化最终响应"}`, http.StatusInternalServerError)
			return
		}

		// 设置正确的 Header 和状态码
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// 根据 success 标志决定最终的 HTTP 状态码，或者保留原始错误码（如果需要）
		// 如果希望所有业务错误都返回 200 OK，可以在这里覆盖 statusCode
		// if !finalResponse.Success { statusCode = http.StatusOK }
		w.WriteHeader(statusCode) // 写入拦截到的或默认的 StatusOK
		w.Write(finalJson)        // 写入最终的 JSON 响应体
	})
}

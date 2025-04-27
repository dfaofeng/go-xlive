package middleware // 修改包名

import (
	"bytes"
	"encoding/json"
	"net/http"

	// !!! 替换模块路径 !!!
	"go-xlive/pkg/response"

	"go.opentelemetry.io/otel/trace"
)

// ResponseWriterInterceptor (保持不变)
type ResponseWriterInterceptor struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

// NewResponseWriterInterceptor (保持不变)
func NewResponseWriterInterceptor(w http.ResponseWriter) *ResponseWriterInterceptor {
	return &ResponseWriterInterceptor{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           new(bytes.Buffer),
	}
}

// WriteHeader (保持不变)
func (w *ResponseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Write (保持不变)
func (w *ResponseWriterInterceptor) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

// getGrpcErrorMessage 根据 gRPC 状态码返回对应的用户友好消息
func getGrpcErrorMessage(code int) string {
	switch code {
	case 1:
		return "操作被取消"
	case 2:
		return "未知错误"
	case 3:
		return "无效参数"
	case 4:
		return "操作超时"
	case 5:
		return "未找到资源"
	case 6:
		return "资源已存在"
	case 7:
		return "权限不足"
	case 8:
		return "资源耗尽"
	case 9:
		return "前置条件失败"
	case 10:
		return "操作被中止"
	case 11:
		return "超出范围"
	case 12:
		return "未实现"
	case 13:
		return "内部错误"
	case 14:
		return "服务不可用"
	case 15:
		return "数据丢失"
	case 16:
		return "未认证"
	default:
		return "未知错误"
	}
}

// UnifiedResponseMiddleware (保持不变)
func UnifiedResponseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		interceptor := NewResponseWriterInterceptor(w)
		next.ServeHTTP(interceptor, r)

		var finalResponse *response.UnifiedResponse
		originalBody := interceptor.body.Bytes()
		statusCode := interceptor.statusCode
		traceID := trace.SpanContextFromContext(r.Context()).TraceID().String()

		// 尝试解析原始响应，检查是否包含错误信息
		var errResp struct {
			Error   string        `json:"error"`
			Message string        `json:"message"`
			Code    int           `json:"code"`
			Details []interface{} `json:"details"`
		}
		err := json.Unmarshal(originalBody, &errResp)

		// 如果原始响应包含错误信息，或者状态码不是 2xx，则视为失败响应
		if err == nil && (errResp.Error != "" || errResp.Message != "" || errResp.Code != 0) || statusCode >= 300 {
			errMsg := getGrpcErrorMessage(statusCode)
			errCode := statusCode // 默认用 HTTP 状态码
			if err == nil {
				if errResp.Code != 0 {
					errCode = errResp.Code // 使用 gRPC 错误码
					errMsg = getGrpcErrorMessage(errCode)
				}
				finalResponse = response.Fail(errCode, errMsg, errResp.Details)
			} else {
				if len(originalBody) > 0 {
					finalResponse = response.Fail(errCode, errMsg, string(originalBody))
				} else {
					finalResponse = response.Fail(errCode, errMsg)
				}
			}
		} else {
			// 成功响应
			var responseData interface{}
			if err := json.Unmarshal(originalBody, &responseData); err != nil {
				if len(originalBody) == 0 {
					responseData = nil
				} else {
					responseData = string(originalBody)
				} // 将非 JSON 作为字符串处理
			}
			finalResponse = response.Success(responseData)
		}

		if traceID != "" && traceID != "00000000000000000000000000000000" {
			finalResponse.TraceID = traceID
		}

		finalJson, err := json.Marshal(finalResponse)
		if err != nil {
			http.Error(w, `{"success":false,"code":500,"message":"无法序列化最终响应"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// 无论成功还是失败，都返回 200 OK，让客户端通过 success 字段判断
		w.WriteHeader(http.StatusOK)
		w.Write(finalJson)
	})
}

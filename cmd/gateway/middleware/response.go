package middleware // 修改包名

import (
	"bytes"
	"encoding/json"
	"net/http"
	// !!! 替换模块路径 !!!
	"go-xlive/pkg/response"
	"go.opentelemetry.io/otel/trace"
)

// responseWriterInterceptor (保持不变)
type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

// NewResponseWriterInterceptor (保持不变)
func NewResponseWriterInterceptor(w http.ResponseWriter) *responseWriterInterceptor {
	return &responseWriterInterceptor{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           new(bytes.Buffer),
	}
}

// WriteHeader (保持不变)
func (w *responseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Write (保持不变)
func (w *responseWriterInterceptor) Write(b []byte) (int, error) {
	return w.body.Write(b)
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

		if statusCode >= 200 && statusCode < 300 {
			var responseData interface{}
			if err := json.Unmarshal(originalBody, &responseData); err != nil {
				if len(originalBody) == 0 {
					responseData = nil
				} else {
					responseData = string(originalBody)
				} // 将非 JSON 作为字符串处理
			}
			finalResponse = response.Success(responseData)
		} else {
			var errResp struct {
				Error   string        `json:"error"`
				Message string        `json:"message"`
				Code    int           `json:"code"`
				Details []interface{} `json:"details"`
			}
			errMsg := http.StatusText(statusCode)
			errCode := statusCode // 默认用 HTTP 状态码
			if err := json.Unmarshal(originalBody, &errResp); err == nil {
				if errResp.Message != "" {
					errMsg = errResp.Message
				} else if errResp.Error != "" {
					errMsg = errResp.Error
				}
				errCode = errResp.Code // 使用 gRPC 错误码
				finalResponse = response.Fail(errCode, errMsg, errResp.Details)
			} else {
				if len(originalBody) > 0 {
					finalResponse = response.Fail(errCode, errMsg, string(originalBody))
				} else {
					finalResponse = response.Fail(errCode, errMsg)
				}
			}
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
		w.WriteHeader(statusCode)
		w.Write(finalJson)
	})
}

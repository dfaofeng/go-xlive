// 可以放在一个新的包，例如 pkg/response
package response

// UnifiedResponse 是所有 API 返回的统一结构
type UnifiedResponse struct {
	Success bool        `json:"success"`            // 操作是否成功
	Code    int         `json:"code"`               // 自定义业务状态码 (可选)
	Message string      `json:"message"`            // 给用户的提示信息
	Data    interface{} `json:"data,omitempty"`     // 实际业务数据 (成功时)
	Error   *ApiError   `json:"error,omitempty"`    // 错误详情 (失败时)
	TraceID string      `json:"trace_id,omitempty"` // (可选) 追踪 ID，方便排查问题
}

// ApiError 包含更详细的错误信息
type ApiError struct {
	Code    int         `json:"code"`              // 内部错误码 (可选)
	Message string      `json:"message"`           // 错误的详细描述
	Details interface{} `json:"details,omitempty"` // (可选) 错误的额外细节，例如字段验证错误
}

// Success 创建一个成功的响应
func Success(data interface{}, message ...string) *UnifiedResponse {
	msg := "操作成功"
	if len(message) > 0 {
		msg = message[0]
	}
	return &UnifiedResponse{
		Success: true,
		Code:    0, // 0 通常表示成功
		Message: msg,
		Data:    data,
	}
}

// Fail 创建一个失败的响应
func Fail(code int, message string, details ...interface{}) *UnifiedResponse {
	var detailData interface{}
	if len(details) > 0 {
		detailData = details[0]
	}
	return &UnifiedResponse{
		Success: false,
		Code:    code, // 自定义错误码
		Message: message,
		Error: &ApiError{
			Code:    code,
			Message: message,
			Details: detailData,
		},
	}
}

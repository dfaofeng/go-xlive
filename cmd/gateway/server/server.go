// cmd/gateway/server/server.go
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// RunHTTPServer 启动 HTTP 服务器并在后台运行
// 返回一个 channel 用于接收服务器错误
func RunHTTPServer(ctx context.Context, logger *zap.Logger, addr string, handler http.Handler) (*http.Server, <-chan error) {
	errChan := make(chan error, 1)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
		// 可以添加 Read/Write/Idle 超时设置
		// ReadTimeout:  5 * time.Second,
		// WriteTimeout: 10 * time.Second,
		// IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("HTTP 网关服务器正在监听", zap.String("address", addr))
		// ListenAndServe 总是返回非 nil 错误
		// 在 Shutdown 或 Close 后会返回 http.ErrServerClosed
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("HTTP 服务器启动失败: %w", err)
		}
		close(errChan) // 正常关闭或启动失败后关闭 channel
	}()

	return httpServer, errChan
}

// ShutdownHTTPServer 优雅关闭 HTTP 服务器
func ShutdownHTTPServer(ctx context.Context, logger *zap.Logger, server *http.Server, timeout time.Duration) {
	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.Info("正在优雅关闭 HTTP 网关服务器...")
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("关闭 HTTP 网关失败", zap.Error(err))
	} else {
		logger.Info("HTTP 网关服务器已关闭")
	}
}

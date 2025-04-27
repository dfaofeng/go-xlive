package service

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// --- Listener Manager ---
type listenerControl struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type listenerManager struct {
	mu        sync.Mutex
	listeners map[int]*listenerControl // key: Bilibili Room ID
	adapter   *BilibiliAdapterService  // Keep reference to adapter for calling runListenerLoop
	logger    *zap.Logger
}

func newListenerManager(adapter *BilibiliAdapterService) *listenerManager {
	return &listenerManager{
		listeners: make(map[int]*listenerControl),
		adapter:   adapter,
		logger:    adapter.logger.Named("listener_manager"),
	}
}

// startListener 启动指定房间的监听 goroutine (如果尚未运行)
func (lm *listenerManager) startListener(biliRoomID int) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if _, exists := lm.listeners[biliRoomID]; exists {
		lm.logger.Debug("监听器已在运行", zap.Int("bili_room_id", biliRoomID))
		return
	}

	ctx, cancel := context.WithCancel(context.Background()) // 为每个 listener 创建独立的 context
	lc := &listenerControl{
		cancel: cancel,
	}
	lm.listeners[biliRoomID] = lc

	lc.wg.Add(1)
	go func(id int, ctx context.Context, lc *listenerControl) {
		defer lc.wg.Done()
		lm.logger.Info("启动监听 goroutine", zap.Int("bili_room_id", id))
		// 调用实际的监听循环函数 (需要 adapter 引用)
		lm.adapter.runListenerLoop(ctx, id) // runListenerLoop will be moved to websocket.go but called via adapter
		lm.logger.Info("监听 goroutine 退出", zap.Int("bili_room_id", id))

		// 从 map 中移除自身 (在 goroutine 退出后)
		lm.mu.Lock()
		delete(lm.listeners, id)
		lm.mu.Unlock()
	}(biliRoomID, ctx, lc)

}

// stopListener 停止指定房间的监听 goroutine
func (lm *listenerManager) stopListener(biliRoomID int) {
	lm.mu.Lock()
	lc, exists := lm.listeners[biliRoomID]
	if !exists {
		lm.logger.Debug("监听器未运行，无需停止", zap.Int("bili_room_id", biliRoomID))
		lm.mu.Unlock()
		return
	}
	// 从 map 中移除，防止重复停止
	delete(lm.listeners, biliRoomID)
	lm.mu.Unlock() // 在调用 cancel 前解锁，避免潜在死锁

	lm.logger.Info("正在停止监听 goroutine", zap.Int("bili_room_id", biliRoomID))
	lc.cancel() // 发送取消信号
}

// stopAllListeners 停止所有监听 goroutine
func (lm *listenerManager) stopAllListeners() {
	lm.mu.Lock()
	lm.logger.Info("正在停止所有监听器...", zap.Int("count", len(lm.listeners)))
	listenersToStop := make([]*listenerControl, 0, len(lm.listeners))
	idsToStop := make([]int, 0, len(lm.listeners))
	for id, lc := range lm.listeners {
		listenersToStop = append(listenersToStop, lc)
		idsToStop = append(idsToStop, id)
	}
	// 清空 map
	lm.listeners = make(map[int]*listenerControl)
	lm.mu.Unlock() // 在调用 cancel 前解锁

	for i, lc := range listenersToStop {
		lm.logger.Debug("发送停止信号", zap.Int("bili_room_id", idsToStop[i]))
		lc.cancel()
	}
	// 等待所有 goroutine 退出
	// for _, lc := range listenersToStop {
	// 	lc.wg.Wait() // 同步等待可能阻塞关闭流程，考虑超时或异步等待
	// }
	lm.logger.Info("所有监听器已停止")
}

// isListening 检查指定房间是否正在监听
func (lm *listenerManager) isListening(biliRoomID int) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	_, exists := lm.listeners[biliRoomID]
	return exists
}

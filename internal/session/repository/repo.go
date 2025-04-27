package repository

import (
	"context"
	// "errors" // No longer needed for IsSessionActive
	// "fmt" // No longer needed for IsSessionActive

	// "github.com/jackc/pgx/v5" // No longer needed for IsSessionActive
	"github.com/jackc/pgx/v5/pgxpool"

	// !!! 替换模块路径 !!!
	db "go-xlive/internal/session/repository/db"
)

// Repository 定义了与场次数据交互的方法接口
type Repository interface {
	// CreateSession 实现创建场次方法
	CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	// GetSessionByID 实现获取场次方法
	GetSessionByID(ctx context.Context, sessionID string) (db.Session, error) // 参数保持 string
	// EndSession 实现结束场次方法
	EndSession(ctx context.Context, arg db.EndSessionParams) (db.Session, error)
	// UpdateSessionAggregates 实现更新聚合统计的方法
	UpdateSessionAggregates(ctx context.Context, arg db.UpdateSessionAggregatesParams) (db.Session, error)
	// GetLiveSessionByRoomID 实现获取直播中场次的方法
	GetLiveSessionByRoomID(ctx context.Context, roomID string) (db.Session, error)
	// GetLiveSessionIDByRoomID 实现获取直播中场次 ID 的方法
	GetLiveSessionIDByRoomID(ctx context.Context, roomID string) (string, error)
	// DeleteSession 实现删除场次的方法
	DeleteSession(ctx context.Context, sessionID string) error
	// ListSessions 实现分页列出场次的方法
	ListSessions(ctx context.Context, arg db.ListSessionsParams) ([]db.Session, error)
	// GetSessionsCount 实现获取场次总数的方法
	GetSessionsCount(ctx context.Context, arg db.GetSessionsCountParams) (int64, error)
	// Ping 实现数据库连接检查方法
	Ping(ctx context.Context) error // <--- 新增
	// ListLiveSessionIDs 实现获取所有直播中场次 ID 的方法
	ListLiveSessionIDs(ctx context.Context) ([]string, error)
}

// postgresRepository 是 Repository 接口的 PostgreSQL 实现
type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

// NewRepository 创建一个新的 Repository 实例
func NewRepository(dbpool *pgxpool.Pool) Repository {
	return &postgresRepository{
		db:      dbpool,
		queries: db.New(dbpool),
	}
}

// CreateSession 实现创建场次方法
func (r *postgresRepository) CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error) {
	return r.queries.CreateSession(ctx, arg)
}

// GetSessionByID 实现获取场次方法
func (r *postgresRepository) GetSessionByID(ctx context.Context, sessionID string) (db.Session, error) {
	return r.queries.GetSessionByID(ctx, sessionID)
}

// EndSession 实现结束场次方法
func (r *postgresRepository) EndSession(ctx context.Context, arg db.EndSessionParams) (db.Session, error) {
	return r.queries.EndSession(ctx, arg)
}

// UpdateSessionAggregates 实现更新聚合统计的方法
func (r *postgresRepository) UpdateSessionAggregates(ctx context.Context, arg db.UpdateSessionAggregatesParams) (db.Session, error) { // <--- 新增实现
	return r.queries.UpdateSessionAggregates(ctx, arg)
}

// GetLiveSessionByRoomID 实现获取直播中场次的方法
func (r *postgresRepository) GetLiveSessionByRoomID(ctx context.Context, roomID string) (db.Session, error) { // <--- 新增实现
	return r.queries.GetLiveSessionByRoomID(ctx, roomID)
}

// GetLiveSessionIDByRoomID 实现获取直播中场次 ID 的方法
func (r *postgresRepository) GetLiveSessionIDByRoomID(ctx context.Context, roomID string) (string, error) { // <--- 新增实现
	return r.queries.GetLiveSessionIDByRoomID(ctx, roomID)
}

// DeleteSession 实现删除场次的方法
func (r *postgresRepository) DeleteSession(ctx context.Context, sessionID string) error {
	return r.queries.DeleteSession(ctx, sessionID)
}

// ListSessions 实现分页列出场次的方法
func (r *postgresRepository) ListSessions(ctx context.Context, arg db.ListSessionsParams) ([]db.Session, error) {
	return r.queries.ListSessions(ctx, arg)
}

// GetSessionsCount 实现获取场次总数的方法
func (r *postgresRepository) GetSessionsCount(ctx context.Context, arg db.GetSessionsCountParams) (int64, error) {
	return r.queries.GetSessionsCount(ctx, arg)
}

// Ping 实现数据库连接检查方法
func (r *postgresRepository) Ping(ctx context.Context) error { // <--- 新增实现
	return r.db.Ping(ctx)
}

// ListLiveSessionIDs 实现获取所有直播中场次 ID 的方法
func (r *postgresRepository) ListLiveSessionIDs(ctx context.Context) ([]string, error) {
	return r.queries.ListLiveSessionIDs(ctx)
}

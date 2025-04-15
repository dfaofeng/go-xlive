package repository

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	db "go-xlive/internal/session/repository/db"
)

// Repository 定义了与场次数据交互的方法接口
type Repository interface {
	CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	GetSessionByID(ctx context.Context, sessionID string) (db.Session, error) // 参数保持 string
	EndSession(ctx context.Context, arg db.EndSessionParams) (db.Session, error)
	UpdateSessionAggregates(ctx context.Context, arg db.UpdateSessionAggregatesParams) (db.Session, error) // <--- 新增
	Ping(ctx context.Context) error                                                                        // <--- 新增
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

// Ping 实现数据库连接检查方法
func (r *postgresRepository) Ping(ctx context.Context) error { // <--- 新增实现
	return r.db.Ping(ctx)
}

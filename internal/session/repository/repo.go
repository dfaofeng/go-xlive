package repository

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	db "go-xlive/internal/session/repository/db"
)

// Repository defines the methods for interacting with session data.
type Repository interface {
	CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	GetSessionByID(ctx context.Context, sessionID string) (db.Session, error)
	EndSession(ctx context.Context, arg db.EndSessionParams) (db.Session, error)
	UpdateSessionAggregates(ctx context.Context, arg db.UpdateSessionAggregatesParams) (db.Session, error)
	Ping(ctx context.Context) error // <--- 添加 Ping 方法接口定义// <--- 新增
}

type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

// NewRepository creates a new repository with database access.
func NewRepository(dbpool *pgxpool.Pool) Repository {
	return &postgresRepository{
		db:      dbpool,
		queries: db.New(dbpool),
	}
}

func (r *postgresRepository) CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error) {
	return r.queries.CreateSession(ctx, arg)
}

func (r *postgresRepository) GetSessionByID(ctx context.Context, sessionID string) (db.Session, error) {
	return r.queries.GetSessionByID(ctx, sessionID)
}

func (r *postgresRepository) EndSession(ctx context.Context, arg db.EndSessionParams) (db.Session, error) {
	return r.queries.EndSession(ctx, arg)
}

// UpdateSessionAggregates implements the update method.
func (r *postgresRepository) UpdateSessionAggregates(ctx context.Context, arg db.UpdateSessionAggregatesParams) (db.Session, error) { // <--- 新增实现
	return r.queries.UpdateSessionAggregates(ctx, arg)
}

// Ping 实现数据库连接检查方法 <--- 新增实现
func (r *postgresRepository) Ping(ctx context.Context) error {
	return r.db.Ping(ctx)
}

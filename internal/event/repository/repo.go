package repository

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	db "go-xlive/internal/event/repository/db"
)

type Repository interface {
	CreateEvent(context.Context, db.CreateEventParams) error
	// --- 修改查询方法签名以匹配新的 SQL 参数 ---
	QueryEventsBySessionID(ctx context.Context, arg db.QueryEventsBySessionIDParams) ([]db.Event, error)
	QueryEventsBySessionIDAndType(ctx context.Context, arg db.QueryEventsBySessionIDAndTypeParams) ([]db.Event, error)
	CountEventsBySessionID(ctx context.Context, arg db.CountEventsBySessionIDParams) (int64, error) // <--- 新增 Count 方法
	// --- 修改结束 ---
	Ping(ctx context.Context) error
}

type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewRepository(p *pgxpool.Pool) Repository { return &postgresRepository{p, db.New(p)} }
func (r *postgresRepository) CreateEvent(c context.Context, a db.CreateEventParams) error {
	return r.queries.CreateEvent(c, a)
}

// --- 修改查询方法实现 ---
func (r *postgresRepository) QueryEventsBySessionID(c context.Context, arg db.QueryEventsBySessionIDParams) ([]db.Event, error) {
	return r.queries.QueryEventsBySessionID(c, arg)
}
func (r *postgresRepository) QueryEventsBySessionIDAndType(c context.Context, a db.QueryEventsBySessionIDAndTypeParams) ([]db.Event, error) {
	return r.queries.QueryEventsBySessionIDAndType(c, a)
}
func (r *postgresRepository) CountEventsBySessionID(c context.Context, arg db.CountEventsBySessionIDParams) (int64, error) {
	return r.queries.CountEventsBySessionID(c, arg)
} // <--- 新增 Count 实现
// --- 修改结束 ---
func (r *postgresRepository) Ping(ctx context.Context) error { return r.db.Ping(ctx) }

// uuidBytesToString 辅助函数 (可选)
func uuidBytesToString(uuidBytes pgtype.UUID) string {
	if !uuidBytes.Valid {
		return ""
	}
	u, err := uuid.FromBytes(uuidBytes.Bytes[:])
	if err != nil {
		return ""
	}
	return u.String()
}

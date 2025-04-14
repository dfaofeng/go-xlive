package repository

import (
	"context"
	// "database/sql" // pgtype 包含了处理 null 的方法
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	db "go-xlive/internal/event/repository/db"
)

type Repository interface {
	CreateEvent(context.Context, db.CreateEventParams) error
	QueryEventsBySessionID(ctx context.Context, sessionID pgtype.Text) ([]db.Event, error) // 参数改为 pgtype.Text
	QueryEventsBySessionIDAndType(context.Context, db.QueryEventsBySessionIDAndTypeParams) ([]db.Event, error)
	Ping(ctx context.Context) error // 添加 Ping 方法
}

type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewRepository(p *pgxpool.Pool) Repository { return &postgresRepository{p, db.New(p)} }
func (r *postgresRepository) CreateEvent(c context.Context, a db.CreateEventParams) error {
	return r.queries.CreateEvent(c, a)
}
func (r *postgresRepository) QueryEventsBySessionID(c context.Context, sid pgtype.Text) ([]db.Event, error) {
	return r.queries.QueryEventsBySessionID(c, sid)
}
func (r *postgresRepository) QueryEventsBySessionIDAndType(c context.Context, a db.QueryEventsBySessionIDAndTypeParams) ([]db.Event, error) {
	return r.queries.QueryEventsBySessionIDAndType(c, a)
}
func (r *postgresRepository) Ping(ctx context.Context) error { return r.db.Ping(ctx) } // 实现 Ping

// 辅助函数 (可选, service 层也可以处理)
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

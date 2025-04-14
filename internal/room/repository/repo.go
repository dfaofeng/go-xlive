package repository

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	// !!! 替换模块路径 !!!
	db "go-xlive/internal/room/repository/db"
)

// Repository defines the methods for interacting with room data.
type Repository interface {
	CreateRoom(ctx context.Context, arg db.CreateRoomParams) (db.Room, error)
	GetRoomByID(ctx context.Context, roomID string) (db.Room, error)
	UpdateRoomStatus(ctx context.Context, arg db.UpdateRoomStatusParams) (db.Room, error)
	Ping(ctx context.Context) error // <--- 添加 Ping 方法接口定义
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

func (r *postgresRepository) CreateRoom(ctx context.Context, arg db.CreateRoomParams) (db.Room, error) {
	return r.queries.CreateRoom(ctx, arg)
}

func (r *postgresRepository) GetRoomByID(ctx context.Context, roomID string) (db.Room, error) {
	return r.queries.GetRoomByID(ctx, roomID)
}

func (r *postgresRepository) UpdateRoomStatus(ctx context.Context, arg db.UpdateRoomStatusParams) (db.Room, error) {
	return r.queries.UpdateRoomStatus(ctx, arg)
}

// Ping 实现数据库连接检查方法 <--- 新增实现
func (r *postgresRepository) Ping(ctx context.Context) error {
	return r.db.Ping(ctx)
}

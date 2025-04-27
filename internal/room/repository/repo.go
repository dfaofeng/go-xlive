package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	// !!! 替换模块路径 !!!
	db "go-xlive/internal/room/repository/db"
)

// Repository defines the methods for interacting with room data.
type Repository interface {
	// !!! 修正: CreateRoom 参数已由 sqlc 自动更新 !!!
	CreateRoom(ctx context.Context, arg db.CreateRoomParams) (db.Room, error)
	GetRoomByID(ctx context.Context, roomID string) (db.Room, error)
	UpdateRoomStatus(ctx context.Context, arg db.UpdateRoomStatusParams) (db.Room, error)
	// !!! 新增: UpdateRoom 方法接口定义 !!!
	UpdateRoom(ctx context.Context, arg db.UpdateRoomParams) (db.Room, error)
	// !!! 新增: ListRooms 方法接口定义 !!!
	ListRooms(ctx context.Context, arg db.ListRoomsParams) ([]db.Room, error)
	// !!! 新增: GetRoomByPlatformID 方法接口定义 !!!
	GetRoomByPlatformID(ctx context.Context, arg db.GetRoomByPlatformIDParams) (db.Room, error)
	// !!! 新增: DeleteRoom 方法接口定义 !!!
	DeleteRoom(ctx context.Context, roomID string) error
	// !!! 新增: UpdateRoomPlatformInfo 方法接口定义 !!!
	UpdateRoomPlatformInfo(ctx context.Context, arg db.UpdateRoomPlatformInfoParams) (db.Room, error)
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

// !!! 新增: UpdateRoom 实现 !!!
func (r *postgresRepository) UpdateRoom(ctx context.Context, arg db.UpdateRoomParams) (db.Room, error) {
	return r.queries.UpdateRoom(ctx, arg)
}

// !!! 新增: ListRooms 实现 !!!
func (r *postgresRepository) ListRooms(ctx context.Context, arg db.ListRoomsParams) ([]db.Room, error) {
	return r.queries.ListRooms(ctx, arg)
}

// !!! 新增: GetRoomByPlatformID 实现 !!!
func (r *postgresRepository) GetRoomByPlatformID(ctx context.Context, arg db.GetRoomByPlatformIDParams) (db.Room, error) {
	return r.queries.GetRoomByPlatformID(ctx, arg)
}

// !!! 新增: DeleteRoom 实现 !!!
func (r *postgresRepository) DeleteRoom(ctx context.Context, roomID string) error {
	return r.queries.DeleteRoom(ctx, roomID)
}

// Ping 实现数据库连接检查方法 <--- 新增实现
func (r *postgresRepository) Ping(ctx context.Context) error {
	return r.db.Ping(ctx)
}

// !!! 新增: UpdateRoomPlatformInfo 实现 !!!
func (r *postgresRepository) UpdateRoomPlatformInfo(ctx context.Context, arg db.UpdateRoomPlatformInfoParams) (db.Room, error) {
	return r.queries.UpdateRoomPlatformInfo(ctx, arg)
}

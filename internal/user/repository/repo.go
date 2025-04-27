package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	// !!! 替换模块路径 !!!
	db "go-xlive/internal/user/repository/db"
)

type Repository interface {
	CreateUser(context.Context, db.CreateUserParams) (db.User, error)
	GetUserByID(context.Context, string) (db.User, error)
	// !!! 新增: UpdateUser 方法接口定义 !!!
	UpdateUser(context.Context, db.UpdateUserParams) (db.User, error)
	Ping(ctx context.Context) error // 添加 Ping 方法

	// --- 新增: System Settings ---
	SetSystemSetting(ctx context.Context, params db.SetSystemSettingParams) error
	GetSystemSetting(ctx context.Context, key string) (string, error)
}

type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewRepository(p *pgxpool.Pool) Repository { return &postgresRepository{p, db.New(p)} }
func (r *postgresRepository) CreateUser(c context.Context, a db.CreateUserParams) (db.User, error) {
	return r.queries.CreateUser(c, a)
}
func (r *postgresRepository) GetUserByID(c context.Context, id string) (db.User, error) {
	return r.queries.GetUserByID(c, id)
}

// !!! 新增: UpdateUser 实现 !!!
func (r *postgresRepository) UpdateUser(c context.Context, a db.UpdateUserParams) (db.User, error) {
	return r.queries.UpdateUser(c, a)
}
func (r *postgresRepository) Ping(ctx context.Context) error { return r.db.Ping(ctx) } // 实现 Ping

// --- 新增: System Settings 实现 ---
func (r *postgresRepository) SetSystemSetting(ctx context.Context, params db.SetSystemSettingParams) error {
	return r.queries.SetSystemSetting(ctx, params)
}

func (r *postgresRepository) GetSystemSetting(ctx context.Context, key string) (string, error) {
	return r.queries.GetSystemSetting(ctx, key)
}

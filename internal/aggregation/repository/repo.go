package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	// !!! 替换模块路径 !!!
	db "go-xlive/internal/aggregation/repository/db"
)

// Repository 定义了与聚合数据交互的方法接口
type Repository interface {
	// InsertSessionMetric 插入一条新的时间点指标数据
	InsertSessionMetric(ctx context.Context, arg db.InsertSessionMetricParams) (db.SessionMetricsTimeline, error)
	// GetSessionMetricsTimeline 查询指定 session_id 在某个时间范围内的所有指标数据
	GetSessionMetricsTimeline(ctx context.Context, sessionID string, startTime, endTime time.Time) ([]db.GetSessionMetricsTimelineRow, error)
	// Ping 实现数据库连接检查方法 (可选，但推荐)
	Ping(ctx context.Context) error
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

// InsertSessionMetric 实现插入指标数据方法
func (r *postgresRepository) InsertSessionMetric(ctx context.Context, arg db.InsertSessionMetricParams) (db.SessionMetricsTimeline, error) {
	return r.queries.InsertSessionMetric(ctx, arg)
}

// GetSessionMetricsTimeline 实现查询时间序列数据方法
func (r *postgresRepository) GetSessionMetricsTimeline(ctx context.Context, sessionID string, startTime, endTime time.Time) ([]db.GetSessionMetricsTimelineRow, error) {
	params := db.GetSessionMetricsTimelineParams{
		SessionID:   sessionID,
		Timestamp:   pgtype.Timestamptz{Time: startTime, Valid: true}, // Start time
		Timestamp_2: pgtype.Timestamptz{Time: endTime, Valid: true},   // End time
	}
	return r.queries.GetSessionMetricsTimeline(ctx, params)
}

// Ping 实现数据库连接检查方法
func (r *postgresRepository) Ping(ctx context.Context) error {
	return r.db.Ping(ctx)
}

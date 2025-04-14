-- 创建场次表
CREATE TABLE sessions (
                          session_id TEXT PRIMARY KEY,                   -- 场次 ID (主键)
                          room_id TEXT NOT NULL REFERENCES rooms(room_id), -- 关联的房间 ID (外键)
                          start_time TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- 开始时间 (非空，默认当前)
                          end_time TIMESTAMPTZ,                          -- 结束时间 (可为空，直到结束时才设置)
                          status TEXT NOT NULL DEFAULT 'live',           -- 场次状态 (例如: live, ended)，默认 live
    -- 稍后添加聚合统计列
    -- total_danmaku BIGINT NOT NULL DEFAULT 0,
                          created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()   -- 创建时间
);

-- 为 room_id 和 status 创建索引
CREATE INDEX idx_sessions_room_id ON sessions(room_id);
CREATE INDEX idx_sessions_status ON sessions(status);

-- 假设 sessions 表已存在
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS total_events BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS total_danmaku BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS total_gifts_value BIGINT NOT NULL DEFAULT 0;
-- 注意：实际应用推荐使用迁移工具管理 schema 变更
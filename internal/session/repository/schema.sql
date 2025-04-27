-- 创建场次表
CREATE TABLE sessions (
                          session_id TEXT PRIMARY KEY,                   -- 场次 ID (主键)
                          room_id TEXT NOT NULL REFERENCES rooms(room_id), -- 关联的房间 ID (外键)
                          owner_user_id TEXT NOT NULL,                     -- 房主用户 ID (保留, 便于按用户查询场次)
                          start_time TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- 开始时间 (非空，默认当前)
                          end_time TIMESTAMPTZ,                          -- 结束时间 (可为空，直到结束时才设置)
                          status TEXT NOT NULL DEFAULT 'live',           -- 场次状态 (例如: live, ended)，默认 live
                          -- 聚合统计列
                          total_events BIGINT NOT NULL DEFAULT 0,
                          total_danmaku BIGINT NOT NULL DEFAULT 0,
                          total_gifts_value BIGINT NOT NULL DEFAULT 0,
                          total_likes BIGINT NOT NULL DEFAULT 0,
                          total_watched BIGINT NOT NULL DEFAULT 0,
                          created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),   -- 创建时间
                          session_title TEXT,                            -- 场次标题 (会话自身的标题)
                          anchor_name TEXT,                              -- 主播名称 (Bilibili streamer_name)
                          room_title TEXT                                -- 房间标题 (Bilibili room_title) <-- 新增
);

-- 为 room_id 和 status 创建索引
CREATE INDEX idx_sessions_room_id ON sessions(room_id);
CREATE INDEX idx_sessions_status ON sessions(status);
-- 为 owner_user_id 创建索引
CREATE INDEX idx_sessions_owner_user_id ON sessions(owner_user_id);

-- 添加唯一约束以防止同一房间有多个活跃会话
CREATE UNIQUE INDEX idx_sessions_unique_live_room ON sessions (room_id) WHERE end_time IS NULL;

-- 注意: SQLC 只关注上面的 CREATE TABLE 语句来生成代码。
-- 下面的 ALTER TABLE 语句仅用于手动修改现有数据库，与代码生成无关。
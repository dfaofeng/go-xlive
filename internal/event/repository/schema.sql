-- 推荐使用 pgcrypto 的 gen_random_uuid()
-- 如果数据库没有 pgcrypto 扩展, 可以先执行 CREATE EXTENSION IF NOT EXISTS "pgcrypto";
-- 或者在应用层生成 UUID 字符串传入
CREATE TABLE events (
                        event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 事件 ID
                        session_id TEXT,                     -- 关联场次 ID (对于房间事件可能为空)
                        room_id TEXT,                        -- 关联房间 ID (可选)
                        event_type TEXT NOT NULL,            -- 事件类型字符串 (例如 "user.presence", "chat.message")
                        user_id TEXT,                        -- 关联用户 ID (可能为空)
                        event_time TIMESTAMPTZ NOT NULL,     -- 事件发生精确时间
                        data BYTEA,                          -- 存储原始 Protobuf 二进制数据
                        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW() -- 记录创建时间
);
-- 索引优化查询
CREATE INDEX idx_events_session_id_event_time ON events(session_id, event_time);
CREATE INDEX idx_events_session_id_event_type ON events(session_id, event_type);
CREATE INDEX idx_events_event_type ON events(event_type); -- 如果经常按类型查
CREATE INDEX idx_events_room_id ON events(room_id);    -- 如果需要按房间查事件
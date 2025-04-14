-- 创建房间表
CREATE TABLE rooms (
                       room_id TEXT PRIMARY KEY,                          -- 房间 ID (主键)
                       room_name TEXT NOT NULL,                           -- 房间名 (非空)
                       owner_user_id TEXT NOT NULL REFERENCES users(user_id), -- 房主用户 ID (外键关联 users 表)
                       status TEXT NOT NULL DEFAULT 'inactive',           -- 房间状态 (例如: inactive, active, disabled)，默认 inactive
                       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()      -- 创建时间
);

-- 为房主 ID 创建索引以提高查询效率
CREATE INDEX idx_rooms_owner_user_id ON rooms(owner_user_id);
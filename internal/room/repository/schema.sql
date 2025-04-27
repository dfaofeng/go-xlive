-- 创建房间表
CREATE TABLE rooms (
                       room_id TEXT PRIMARY KEY,                          -- 房间 ID (主键)
                       room_name TEXT NOT NULL,                           -- 房间名 (非空)
                       owner_user_id TEXT NOT NULL REFERENCES users(user_id), -- 房主用户 ID (外键关联 users 表)
                       status TEXT NOT NULL DEFAULT 'inactive',           -- 房间状态 (例如: inactive, active, disabled)，默认 inactive
                       platform VARCHAR(50) NOT NULL DEFAULT 'unknown',   -- 平台标识, e.g., "bilibili", "douyin", 默认 unknown
                       platform_room_id VARCHAR(255) NOT NULL,            -- 平台原始房间 ID (非空)
                       area_name TEXT,                                    -- 新增: 分区名称 (允许为空)
                       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),      -- 创建时间
                       anchor_name TEXT,                                  -- 主播名称 (新增)
                       deleted_at TIMESTAMPTZ NULL                        -- 新增: 软删除时间戳 (NULL 表示未删除)
);

-- 为房主 ID 创建索引以提高查询效率
CREATE INDEX idx_rooms_owner_user_id ON rooms(owner_user_id);

-- (可选) 为平台和平台房间 ID 创建索引，如果经常需要按此查询
CREATE INDEX idx_rooms_platform ON rooms(platform);
CREATE INDEX idx_rooms_platform_room_id ON rooms(platform_room_id);

-- (可选) 创建唯一约束，确保同一平台下房间 ID 唯一 (如果启用，则下面的复合索引可以省略)
-- ALTER TABLE rooms ADD CONSTRAINT unique_platform_room_id UNIQUE (platform, platform_room_id);

-- 为 platform 和 platform_room_id 创建复合索引，优化 GetRoomByPlatformID 查询
CREATE INDEX IF NOT EXISTS idx_rooms_platform_platform_room_id ON rooms(platform, platform_room_id);

-- 为 status 创建索引，优化 ListRooms 按状态过滤
CREATE INDEX IF NOT EXISTS idx_rooms_status ON rooms(status);

-- 为 created_at 创建索引，优化 ListRooms 按创建时间排序
CREATE INDEX IF NOT EXISTS idx_rooms_created_at ON rooms(created_at);

-- 新增: 为软删除字段创建索引，通常查询未删除的记录
CREATE INDEX IF NOT EXISTS idx_rooms_deleted_at ON rooms(deleted_at) WHERE deleted_at IS NULL;

-- (可选) 为分区名称创建索引
-- CREATE INDEX idx_rooms_area_name ON rooms(area_name);

-- --- 以下 ALTER TABLE 语句用于更新已存在的表 ---
-- 注意：实际应用推荐使用迁移工具管理 schema 变更

-- 添加 area_name 列 (如果不存在)
ALTER TABLE rooms
    ADD COLUMN IF NOT EXISTS area_name TEXT;
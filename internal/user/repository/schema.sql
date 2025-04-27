-- 创建用户表
CREATE TABLE users (
                       user_id TEXT PRIMARY KEY,                     -- 用户 ID (主键)
                       username TEXT NOT NULL UNIQUE,                -- 用户名 (唯一, 非空)
                       follower_count INTEGER NOT NULL DEFAULT 0,    -- 新增: 粉丝数 (非空, 默认 0)
                       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW() -- 创建时间 (带时区，默认当前时间)
);

-- --- 以下 ALTER TABLE 语句用于更新已存在的表 ---
-- 注意：实际应用推荐使用迁移工具管理 schema 变更

-- 添加 follower_count 列 (如果不存在)
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS follower_count INTEGER NOT NULL DEFAULT 0;

-- 新增: 系统设置表
CREATE TABLE IF NOT EXISTS system_settings (
    setting_key VARCHAR(255) PRIMARY KEY,
    setting_value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
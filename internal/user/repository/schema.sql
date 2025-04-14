-- 创建用户表
CREATE TABLE users (
                       user_id TEXT PRIMARY KEY,                     -- 用户 ID (主键)
                       username TEXT NOT NULL UNIQUE,                -- 用户名 (唯一, 非空)
                       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW() -- 创建时间 (带时区，默认当前时间)
);
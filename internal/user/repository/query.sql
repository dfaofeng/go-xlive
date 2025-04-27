-- name: CreateUser :one
-- 创建一个新用户，并返回创建后的所有字段
-- !!! 修正: 添加 follower_count (通常插入默认值 0) !!!
INSERT INTO users (user_id, username, follower_count)
VALUES ($1, $2, $3) -- 添加 $3 for follower_count
    RETURNING *;

-- name: GetUserByID :one
-- 根据用户 ID 查询用户信息，限制返回一行
SELECT *
FROM users
WHERE user_id = $1
    LIMIT 1;

-- name: UpdateUser :one
-- 更新用户信息 (Service 层会先 Get 再根据 FieldMask 决定更新哪些字段)
UPDATE users
SET
    username = $2,
    follower_count = $3
WHERE user_id = $1
RETURNING *;

-- --- System Settings ---

-- name: SetSystemSetting :exec
-- 设置或更新一个系统设置项
INSERT INTO system_settings (setting_key, setting_value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (setting_key) DO UPDATE SET
    setting_value = EXCLUDED.setting_value,
    updated_at = NOW();

-- name: GetSystemSetting :one
-- 获取一个系统设置项的值
SELECT setting_value FROM system_settings
WHERE setting_key = $1;
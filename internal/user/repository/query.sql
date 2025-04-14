-- name: CreateUser :one
-- 创建一个新用户，并返回创建后的所有字段
INSERT INTO users (user_id, username)
VALUES ($1, $2)
    RETURNING *;

-- name: GetUserByID :one
-- 根据用户 ID 查询用户信息，限制返回一行
SELECT *
FROM users
WHERE user_id = $1
    LIMIT 1;
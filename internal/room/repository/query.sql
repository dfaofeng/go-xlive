-- name: CreateRoom :one
-- 创建一个新房间，并返回创建后的所有字段
INSERT INTO rooms (room_id, room_name, owner_user_id, status)
VALUES ($1, $2, $3, $4)
    RETURNING *;

-- name: GetRoomByID :one
-- 根据房间 ID 查询房间信息
SELECT *
FROM rooms
WHERE room_id = $1
    LIMIT 1;

-- name: UpdateRoomStatus :one
-- 更新指定房间的状态，并返回更新后的记录
UPDATE rooms
SET status = $2
WHERE room_id = $1
    RETURNING *;
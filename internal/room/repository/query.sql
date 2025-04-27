-- name: CreateRoom :one
-- 创建一个新房间，并返回创建后的所有字段
INSERT INTO rooms (room_id, room_name, owner_user_id, status, platform, platform_room_id, area_name)
VALUES ($1, $2, $3, $4, $5, $6, $7) -- 添加 $7 for area_name
    RETURNING *;

-- name: GetRoomByID :one
-- 根据房间 ID 查询房间信息 (仅限未删除)
SELECT *
FROM rooms
WHERE room_id = $1 AND deleted_at IS NULL
    LIMIT 1;

-- name: UpdateRoomStatus :one
-- 更新指定房间的状态，并返回更新后的记录 (仅限未删除)
UPDATE rooms
SET status = $2
WHERE room_id = $1 AND deleted_at IS NULL
    RETURNING *;

-- name: UpdateRoom :one
-- 更新房间信息 (Service 层会先 Get 再根据 FieldMask 决定更新哪些字段) (仅限未删除)
UPDATE rooms
SET
    room_name = $2,
    owner_user_id = $3, -- 理论上 owner 不应该变，但以防万一
    status = $4,
    platform = $5, -- 平台通常也不变
    platform_room_id = $6, -- 平台房间 ID 通常也不变
    area_name = $7
WHERE room_id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: ListRooms :many
-- 列出房间，支持按状态和平台过滤 (仅限未删除)
SELECT *
FROM rooms
WHERE
    deleted_at IS NULL -- 仅查询未删除的房间
AND
    -- 如果 status_filter 不为 NULL，则按 status 过滤
    (sqlc.narg('status_filter')::text IS NULL OR status = sqlc.narg('status_filter'))
AND
-- 如果 platform_filter 不为 NULL，则按 platform 过滤
    (sqlc.narg('platform_filter')::text IS NULL OR platform = sqlc.narg('platform_filter'))
ORDER BY created_at DESC
LIMIT sqlc.arg('page_limit')::int
OFFSET sqlc.arg('page_offset')::int;

-- name: GetRoomByPlatformID :one
-- 根据平台和平台房间 ID 查询房间信息 (仅限未删除)
SELECT *
FROM rooms
WHERE platform = $1 AND platform_room_id = $2 AND deleted_at IS NULL
LIMIT 1;

-- name: DeleteRoom :exec
-- 根据房间 ID 软删除房间 (更新 deleted_at)
UPDATE rooms
SET deleted_at = NOW()
WHERE room_id = $1 AND deleted_at IS NULL; -- 确保只软删除一次

-- name: UpdateRoomPlatformInfo :one
-- 根据 NATS 事件更新房间的平台特定信息 (room_name, anchor_name, area_name)
UPDATE rooms
SET room_name = $2, anchor_name = $3, area_name = $4
WHERE room_id = $1 AND deleted_at IS NULL -- 确保只更新未删除的房间
RETURNING *;
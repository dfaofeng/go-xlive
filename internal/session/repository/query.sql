-- name: CreateSession :one
-- !!! 修正: 添加 room_title 列 !!!
INSERT INTO sessions (
    session_id, room_id, owner_user_id, start_time, status, session_title, anchor_name, room_title
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8 -- $8 is room_title
) RETURNING *;

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE session_id = $1 LIMIT 1;

-- name: EndSession :one
UPDATE sessions SET status = 'ended', end_time = $2 WHERE session_id = $1 AND status = 'live' RETURNING *;

-- name: UpdateSessionAggregates :one
UPDATE sessions
SET
    total_events = $2,
    total_danmaku = $3,
    total_gifts_value = $4,
    total_likes = $5,
    total_watched = $6
WHERE session_id = $1
RETURNING *;

-- name: GetLiveSessionByRoomID :one
SELECT * FROM sessions WHERE room_id = $1 AND status = 'live' LIMIT 1;

-- --- 移除 UpdateSessionRoomInfo 查询 ---
-- -- name: UpdateSessionRoomInfo :many
-- -- 根据 room_id 更新所有直播中场次的房间信息
-- UPDATE sessions
-- SET
--     room_title = $2, -- $1 room_id, $2 room_title
--     area_name = $3  -- $3 area_name
-- WHERE room_id = $1 AND status = 'live'
-- RETURNING *; -- 返回所有被更新的场次

-- --- 移除 UpdateSessionOwnerInfo 查询 ---
-- -- name: UpdateSessionOwnerInfo :many
-- -- 根据 owner_user_id 更新所有直播中场次的主播信息
-- UPDATE sessions
-- SET
--     owner_name = $2,     -- $1 owner_user_id, $2 owner_name
--     follower_count = $3 -- $3 follower_count
-- WHERE owner_user_id = $1 AND status = 'live'
-- RETURNING *; -- 返回所有被更新的场次

-- name: GetLiveSessionIDByRoomID :one
-- 根据内部 room_id 查找活跃会话的 session_id
SELECT session_id
FROM sessions
WHERE room_id = $1 AND status = 'live'
LIMIT 1;

-- name: ListLiveSessionIDs :many
SELECT session_id
FROM sessions
WHERE status = 'live';

-- name: DeleteSession :exec
-- 根据场次 ID 删除场次
DELETE FROM sessions
WHERE session_id = $1;

-- name: ListSessions :many
-- 列出所有场次，支持分页和过滤
SELECT *
FROM sessions
WHERE (status = @status OR @status::text = '')
  AND (room_id = @room_id OR @room_id::text = '')
  AND (owner_user_id = @owner_user_id OR @owner_user_id::text = '')
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: GetSessionsCount :one
-- 获取符合条件的场次总数
SELECT COUNT(*)
FROM sessions
WHERE (status = @status OR @status::text = '')
  AND (room_id = @room_id OR @room_id::text = '')
  AND (owner_user_id = @owner_user_id OR @owner_user_id::text = '');
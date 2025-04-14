-- name: CreateEvent :exec
-- 存储一个事件，包含 room_id
INSERT INTO events (session_id, room_id, event_type, user_id, event_time, data)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: QueryEventsBySessionID :many
-- 查询指定场次的所有事件 (按时间排序)
-- TODO: 添加分页和时间范围过滤
SELECT event_id, session_id, room_id, event_type, user_id, event_time, data, created_at
FROM events
WHERE session_id = $1
ORDER BY event_time ASC;

-- name: QueryEventsBySessionIDAndType :many
-- 查询指定场次和类型的事件
-- TODO: 添加分页和时间范围过滤
SELECT event_id, session_id, room_id, event_type, user_id, event_time, data, created_at
FROM events
WHERE session_id = $1 AND event_type = $2
ORDER BY event_time ASC;
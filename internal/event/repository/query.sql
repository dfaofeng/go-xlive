-- name: CreateEvent :exec
INSERT INTO events (session_id, room_id, event_type, user_id, event_time, data)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: QueryEventsBySessionID :many
-- 修改：为所有参数添加 sqlc.arg() 注释以生成明确的字段名
SELECT event_id, session_id, room_id, event_type, user_id, event_time, data, created_at
FROM events
WHERE session_id = sqlc.arg(session_id) -- $1
  AND (sqlc.arg(start_time)::TIMESTAMPTZ IS NULL OR event_time >= sqlc.arg(start_time)) -- $2
  AND (sqlc.arg(end_time)::TIMESTAMPTZ IS NULL OR event_time <= sqlc.arg(end_time))     -- $3
ORDER BY event_time ASC
    LIMIT sqlc.arg(lim)  -- $4 (使用 'lim' 作为字段名示例)
OFFSET sqlc.arg(offs); -- $5 (使用 'offs' 作为字段名示例)

-- name: QueryEventsBySessionIDAndType :many
-- 修改：为所有参数添加 sqlc.arg() 注释
SELECT event_id, session_id, room_id, event_type, user_id, event_time, data, created_at
FROM events
WHERE session_id = sqlc.arg(session_id)             -- $1
  AND event_type = sqlc.arg(event_type)           -- $2
  AND (sqlc.arg(start_time)::TIMESTAMPTZ IS NULL OR event_time >= sqlc.arg(start_time)) -- $3
  AND (sqlc.arg(end_time)::TIMESTAMPTZ IS NULL OR event_time <= sqlc.arg(end_time))     -- $4
ORDER BY event_time ASC
    LIMIT sqlc.arg(lim)                               -- $5
OFFSET sqlc.arg(offs);                              -- $6

-- name: CountEventsBySessionID :one
-- 修改：为所有参数添加 sqlc.arg() 注释
SELECT count(*)
FROM events
WHERE session_id = sqlc.arg(session_id)              -- $1
  AND (sqlc.arg(start_time)::TIMESTAMPTZ IS NULL OR event_time >= sqlc.arg(start_time))  -- $2
  AND (sqlc.arg(end_time)::TIMESTAMPTZ IS NULL OR event_time <= sqlc.arg(end_time))      -- $3
  AND (sqlc.arg(event_type_filter)::TEXT IS NULL OR event_type = sqlc.arg(event_type_filter)); -- $4 (重命名以示区分)
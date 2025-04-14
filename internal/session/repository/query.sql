-- name: CreateSession :one
INSERT INTO sessions (session_id, room_id, start_time, status) VALUES ($1, $2, $3, $4) RETURNING *;
-- name: GetSessionByID :one
SELECT * FROM sessions WHERE session_id = $1 LIMIT 1;
-- name: EndSession :one
UPDATE sessions SET status = 'ended', end_time = $2 WHERE session_id = $1 AND status = 'live' RETURNING *;
-- name: UpdateSessionAggregates :one
UPDATE sessions SET total_events = $2, total_danmaku = $3, total_gifts_value = $4 WHERE session_id = $1 RETURNING *;
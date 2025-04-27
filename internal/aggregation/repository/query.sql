-- internal/aggregation/repository/query.sql

-- name: InsertSessionMetric :one
INSERT INTO session_metrics_timeline (
    session_id,
    timestamp,
    danmaku_count,
    gift_value,
    like_count,
    watched_count,
    online_rank_count -- 新增
   ) VALUES (
    $1, $2, $3, $4, $5, $6, $7 -- 新增参数 $7
   )
   RETURNING *;

-- name: GetSessionMetricsTimeline :many
SELECT
    timestamp,
    danmaku_count,
    gift_value,
    like_count,
    watched_count,
    online_rank_count -- 新增
   FROM
    session_metrics_timeline
WHERE
    session_id = $1
AND
    timestamp >= $2 -- 开始时间 (包含)
AND
    timestamp <= $3 -- 结束时间 (包含)
ORDER BY
    timestamp ASC;
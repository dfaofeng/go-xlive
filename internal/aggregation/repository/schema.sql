-- internal/aggregation/repository/schema.sql
CREATE TABLE session_metrics_timeline (
    id BIGSERIAL PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    timestamp TIMESTAMPTZ NOT NULL,
    danmaku_count BIGINT NOT NULL DEFAULT 0,
    gift_value BIGINT NOT NULL DEFAULT 0,
    like_count BIGINT NOT NULL DEFAULT 0,
    watched_count BIGINT NOT NULL DEFAULT 0, -- 可能需要根据实际情况调整是否能获取
    online_rank_count BIGINT NOT NULL DEFAULT 0, -- 新增: 在线排名计数
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
   );

CREATE UNIQUE INDEX session_metrics_timeline_session_id_timestamp_key ON session_metrics_timeline (session_id, timestamp);
CREATE INDEX session_metrics_timeline_session_id_idx ON session_metrics_timeline (session_id);
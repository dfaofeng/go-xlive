-- name: InsertChatMessage :exec
INSERT INTO chat_messages (
    message_id, session_id, room_id, user_id, username, content, userlever, admin, mobileverify, guardlevel, medal_upname, medal_level, medal_name, medal_color, medal_uproomid, medal_upuid,timestamp,raw_msg
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,$8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18
);

-- name: InsertGiftSent :exec
INSERT INTO gifts_sent (
    event_id, session_id, room_id, user_id, username, gift_id, gift_name, gift_count, total_coin, coin_type, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
);

-- name: InsertGuardPurchase :exec
INSERT INTO guard_purchases (
    event_id, session_id, room_id, user_id, username, guard_level, guard_name, count, price, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: InsertSuperChatMessage :exec
INSERT INTO super_chat_messages (
    message_id, session_id, room_id, user_id, username, content, price, duration, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: InsertUserInteraction :exec
INSERT INTO user_interactions (
    event_id, session_id, room_id, user_id, username, interaction_type, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: InsertUserPresence :exec
INSERT INTO user_presences (
    session_id, room_id, user_id, username, entered, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: InsertStreamStatus :exec
INSERT INTO stream_statuses (
    session_id, room_id, is_live, timestamp
) VALUES (
    $1, $2, $3, $4
);

-- name: InsertWatchedCountUpdate :exec
INSERT INTO watched_count_updates (
    session_id, room_id, count, text_large, timestamp
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: InsertLikeCountUpdate :exec
INSERT INTO like_count_updates (
    session_id, room_id, count, timestamp
) VALUES (
    $1, $2, $3, $4
);

-- name: InsertOnlineRankUpdate :one
INSERT INTO online_rank_updates (
    session_id, room_id, total_count, timestamp
) VALUES (
    $1, $2, $3, $4
) RETURNING event_id; -- 返回生成的 event_id 以便关联用户

-- name: InsertOnlineRankUser :exec
INSERT INTO online_rank_users (
    rank_update_event_id, user_id, username, rank, score, face_url
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- Queries for Aggregation Service
-- name: QueryGiftSentBySessionID :many
SELECT * FROM gifts_sent
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: QueryChatMessageBySessionID :many
SELECT * FROM chat_messages
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: QueryUserPresenceBySessionID :many
SELECT * FROM user_presences
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: QueryGuardPurchaseBySessionID :many
SELECT * FROM guard_purchases
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: QuerySuperChatMessageBySessionID :many
SELECT * FROM super_chat_messages
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: QueryUserInteractionBySessionID :many
SELECT * FROM user_interactions
WHERE session_id = $1
ORDER BY timestamp ASC;

-- name: GetAllSessionEvents :many
SELECT
    'chat' as event_type,
    cm.message_id as event_id,
    cm.session_id,
    cm.room_id,
    cm.user_id,
    cm.username,
    json_build_object(
        'content', cm.content,
        'userlever',cm.userlever,
        'admin',cm.admin,
        'mobileverify',cm.mobileverify,
        'guardlevel',cm.guardlevel,
        'medal_upname',cm.medal_upname,
        'medal_level',cm.medal_level,
        'medal_name',cm.medal_name,
        'medal_color',cm.medal_color,
        'medal_uproomid',cm.medal_uproomid,
        'medal_upuid',cm.medal_upuid,
        'created_at',cm.created_at
    )as event_data,
    cm.timestamp
FROM chat_messages cm
WHERE cm.session_id = $1

UNION ALL

SELECT
    'gift' as event_type,
    gs.event_id,
    gs.session_id,
    gs.room_id,
    gs.user_id,
    gs.username,
    json_build_object(
        'gift_id', gs.gift_id,
        'gift_name', gs.gift_name,
        'gift_count', gs.gift_count,
        'total_coin', gs.total_coin,
        'coin_type', gs.coin_type
    )as event_data,
    gs.timestamp
FROM gifts_sent gs
WHERE gs.session_id = $1

UNION ALL

SELECT
    'guard' as event_type,
    gp.event_id,
    gp.session_id,
    gp.room_id,
    gp.user_id,
    gp.username,
    json_build_object(
        'guard_level', gp.guard_level,
        'guard_name', gp.guard_name,
        'count', gp.count,
        'price', gp.price
    )as event_data,
    gp.timestamp
FROM guard_purchases gp
WHERE gp.session_id = $1

UNION ALL

SELECT
    'superchat' as event_type,
    scm.message_id as event_id,
    scm.session_id,
    scm.room_id,
    scm.user_id,
    scm.username,
    json_build_object(
        'content', scm.content,
        'price', scm.price,
        'duration', scm.duration
    )as event_data,
    scm.timestamp
FROM super_chat_messages scm
WHERE scm.session_id = $1

UNION ALL

SELECT
    'interaction' as event_type,
    ui.event_id,
    ui.session_id,
    ui.room_id,
    ui.user_id,
    ui.username,
    json_build_object(
        'interaction_type', ui.interaction_type
    )as event_data,
    ui.timestamp
FROM user_interactions ui
WHERE ui.session_id = $1

UNION ALL

SELECT
    'presence' as event_type,
    up.event_id::text,
    up.session_id,
    up.room_id,
    up.user_id,
    up.username,
    json_build_object(
        'entered', up.entered
    )as event_data,
    up.timestamp
FROM user_presences up
WHERE up.session_id = $1

UNION ALL

SELECT
    'stream_status' as event_type,
    ss.event_id::text,
    ss.session_id,
    ss.room_id,
    '' as user_id,
    '' as username,
    json_build_object(
        'is_live', ss.is_live
    )as event_data,
    ss.timestamp
FROM stream_statuses ss
WHERE ss.session_id = $1

UNION ALL

SELECT
    'watched_count' as event_type,
    wc.event_id::text,
    wc.session_id,
    wc.room_id,
    '' as user_id,
    '' as username,
    json_build_object(
        'count', wc.count,
        'text_large', wc.text_large
    )as event_data,
    wc.timestamp
FROM watched_count_updates wc
WHERE wc.session_id = $1

UNION ALL

SELECT
    'like_count' as event_type,
    lc.event_id::text,
    lc.session_id,
    lc.room_id,
    '' as user_id,
    '' as username,
    json_build_object(
        'count', lc.count
    )as event_data,
    lc.timestamp
FROM like_count_updates lc
WHERE lc.session_id = $1

UNION ALL

SELECT
    'online_rank' as event_type,
    oru.event_id::text,
    oru.session_id,
    oru.room_id,
    '' as user_id,
    '' as username,
    json_build_object(
        'total_count', oru.total_count,
        'users', (
            SELECT json_agg(
                json_build_object(
                    'user_id', oru2.user_id,
                    'username', oru2.username,
                    'rank', oru2.rank,
                    'score', oru2.score,
                    'face_url', oru2.face_url
                )
            )
            FROM online_rank_users oru2
            WHERE oru2.rank_update_event_id = oru.event_id
        )
    )as event_data,
    oru.timestamp
FROM online_rank_updates oru
WHERE oru.session_id = $1

ORDER BY timestamp ASC, event_type ASC;

-- ============================================
--          Non-Live Event Inserts
-- ============================================

-- name: InsertNonLiveChatMessage :exec
INSERT INTO non_live_chat_messages (
    message_id, room_id, user_id, username, content, userlever, admin, mobileverify, guardlevel, medal_upname, medal_level, medal_name, medal_color, medal_uproomid, medal_upuid, timestamp, raw_msg
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
);

-- name: InsertNonLiveGiftSent :exec
INSERT INTO non_live_gifts_sent (
    event_id, room_id, user_id, username, gift_id, gift_name, gift_count, total_coin, coin_type, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: InsertNonLiveGuardPurchase :exec
INSERT INTO non_live_guard_purchases (
    event_id, room_id, user_id, username, guard_level, guard_name, count, price, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: InsertNonLiveSuperChatMessage :exec
INSERT INTO non_live_super_chat_messages (
    message_id, room_id, user_id, username, content, price, duration, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
);

-- name: InsertNonLiveUserInteraction :exec
INSERT INTO non_live_user_interactions (
    event_id, room_id, user_id, username, interaction_type, timestamp
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: InsertNonLiveUserPresence :exec
INSERT INTO non_live_user_presences (
    room_id, user_id, username, entered, timestamp
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: InsertNonLiveWatchedCountUpdate :exec
INSERT INTO non_live_watched_count_updates (
    room_id, count, text_large, timestamp
) VALUES (
    $1, $2, $3, $4
);

-- name: InsertNonLiveLikeCountUpdate :exec
INSERT INTO non_live_like_count_updates (
    room_id, count, timestamp
) VALUES (
    $1, $2, $3
);

-- name: InsertNonLiveOnlineRankUpdate :one
INSERT INTO non_live_online_rank_updates (
    room_id, total_count, timestamp
) VALUES (
    $1, $2, $3
) RETURNING event_id; -- 返回生成的 event_id 以便关联用户

-- name: InsertNonLiveOnlineRankUser :exec
INSERT INTO non_live_online_rank_users (
    rank_update_event_id, user_id, username, rank, score, face_url
) VALUES (
    $1, $2, $3, $4, $5, $6
);
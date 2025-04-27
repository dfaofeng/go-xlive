-- 推荐使用 pgcrypto 的 gen_random_uuid()
-- 如果数据库没有 pgcrypto 扩展, 可以先执行 CREATE EXTENSION IF NOT EXISTS "pgcrypto";
-- 或者在应用层生成 UUID 字符串传入

-- ============================================
--          结构化事件表 (直播期间)
-- ============================================

-- 聊天消息表
CREATE TABLE chat_messages (
    message_id TEXT PRIMARY KEY,         -- 消息的唯一 ID (来自 Bilibili 或其他平台)
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 发送用户 ID
    username TEXT NOT NULL,             -- 发送用户名 (Standardized)
    content TEXT NOT NULL,               -- 消息内容
    userlever INT NOT NULL,               -- 用户等级
    admin BOOLEAN NOT NULL,               -- 是否为管理员
    mobileverify BOOLEAN NOT NULL,        -- 手机端验证
    guardlevel INT NOT NULL,               -- 舰长等级
    medal_upname varchar not null ,                   -- 勋章名称
    medal_level INT,
    medal_name varchar not null ,
    medal_color INT,
    medal_uproomid INT,
    medal_upuid INT,
    timestamp TIMESTAMPTZ NOT NULL,      -- 消息发送时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    raw_msg TEXT NOT NULL
);
CREATE INDEX idx_chat_messages_session_id_timestamp ON chat_messages(session_id, timestamp);
CREATE INDEX idx_chat_messages_user_id ON chat_messages(user_id);
CREATE INDEX idx_chat_messages_room_id_timestamp ON chat_messages(room_id, timestamp); -- Add room_id index

-- 礼物赠送表
CREATE TABLE gifts_sent (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID (来自 Bilibili 或其他平台, e.g., tid)
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 送礼用户 ID
    username TEXT NOT NULL,             -- 送礼用户名 (Standardized)
    gift_id TEXT NOT NULL,               -- 礼物 ID
    gift_name TEXT NOT NULL,             -- 礼物名称
    gift_count INT NOT NULL,             -- 礼物数量
    total_coin BIGINT NOT NULL,          -- 礼物总价值 (平台货币)
    coin_type TEXT NOT NULL,             -- 平台货币类型 (e.g., "gold", "silver")
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_gifts_sent_session_id_timestamp ON gifts_sent(session_id, timestamp);
CREATE INDEX idx_gifts_sent_user_id ON gifts_sent(user_id);
CREATE INDEX idx_gifts_sent_gift_id ON gifts_sent(gift_id);
CREATE INDEX idx_gifts_sent_room_id_timestamp ON gifts_sent(room_id, timestamp); -- Add room_id index

-- 大航海购买表
CREATE TABLE guard_purchases (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID (来自 Bilibili 或其他平台)
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 购买用户 ID
    username TEXT NOT NULL,             -- 购买用户名 (Standardized)
    guard_level INT NOT NULL,            -- 大航海等级 (e.g., 1, 2, 3)
    guard_name TEXT NOT NULL,            -- 等级名称 (e.g., "总督")
    count INT NOT NULL,                  -- 购买数量 (通常为 1)
    price BIGINT NOT NULL,               -- 价格 (平台货币)
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_guard_purchases_session_id_timestamp ON guard_purchases(session_id, timestamp);
CREATE INDEX idx_guard_purchases_user_id ON guard_purchases(user_id);
CREATE INDEX idx_guard_purchases_guard_level ON guard_purchases(guard_level);
CREATE INDEX idx_guard_purchases_room_id_timestamp ON guard_purchases(room_id, timestamp); -- Add room_id index

-- 醒目留言表 (Super Chat)
CREATE TABLE super_chat_messages (
    message_id TEXT PRIMARY KEY,         -- SC 消息 ID (来自 Bilibili 或其他平台)
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 发送用户 ID
    username TEXT NOT NULL,             -- 发送用户名 (Standardized)
    content TEXT NOT NULL,               -- SC 内容
    price BIGINT NOT NULL,               -- SC 金额 (通常为法币，如 CNY, 以分为单位存储避免浮点数问题)
    duration INT NOT NULL,               -- SC 持续时间 (秒)
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_super_chat_messages_session_id_timestamp ON super_chat_messages(session_id, timestamp);
CREATE INDEX idx_super_chat_messages_user_id ON super_chat_messages(user_id);
CREATE INDEX idx_super_chat_messages_room_id_timestamp ON super_chat_messages(room_id, timestamp); -- Add room_id index

-- 用户互动表 (关注/分享等)
CREATE TABLE user_interactions (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID (来自 Bilibili 或其他平台)
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 互动用户 ID
    username TEXT NOT NULL,             -- 互动用户名 (Standardized)
    interaction_type INT NOT NULL,       -- 互动类型 (1: FOLLOW, 2: SHARE, ...)
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_user_interactions_session_id_timestamp ON user_interactions(session_id, timestamp);
CREATE INDEX idx_user_interactions_user_id ON user_interactions(user_id);
CREATE INDEX idx_user_interactions_interaction_type ON user_interactions(interaction_type);
CREATE INDEX idx_user_interactions_room_id_timestamp ON user_interactions(room_id, timestamp); -- Add room_id index

-- 用户状态表 (进入/离开)
CREATE TABLE user_presences (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 用户 ID
    username TEXT NOT NULL,             -- 用户名 (Standardized)
    entered BOOLEAN NOT NULL,            -- true 表示进入，false 表示离开
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_user_presences_session_id_timestamp ON user_presences(session_id, timestamp);
CREATE INDEX idx_user_presences_user_id ON user_presences(user_id);
CREATE INDEX idx_user_presences_room_id_timestamp ON user_presences(room_id, timestamp); -- Add room_id index

-- 直播流状态表 (开始/结束)
CREATE TABLE stream_statuses (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    is_live BOOLEAN NOT NULL,            -- true 表示直播开始，false 表示直播结束
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_stream_statuses_session_id_timestamp ON stream_statuses(session_id, timestamp);
CREATE INDEX idx_stream_statuses_room_id_timestamp ON stream_statuses(room_id, timestamp); -- 按房间查状态变化

-- 观看人数更新表
CREATE TABLE watched_count_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    count BIGINT NOT NULL,               -- 当前累计观看人数
    text_large TEXT,                     -- (可选) 平台提供的显示文本
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_watched_count_updates_session_id_timestamp ON watched_count_updates(session_id, timestamp);
CREATE INDEX idx_watched_count_updates_room_id_timestamp ON watched_count_updates(room_id, timestamp); -- Add room_id index

-- 点赞数更新表
CREATE TABLE like_count_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    count BIGINT NOT NULL,               -- 当前累计点赞数
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_like_count_updates_session_id_timestamp ON like_count_updates(session_id, timestamp);
CREATE INDEX idx_like_count_updates_room_id_timestamp ON like_count_updates(room_id, timestamp); -- Add room_id index

-- 高能榜更新事件表
CREATE TABLE online_rank_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    session_id TEXT NOT NULL,            -- 关联场次 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    total_count BIGINT,                  -- (可选) 高能榜总人数
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_online_rank_updates_session_id_timestamp ON online_rank_updates(session_id, timestamp);
CREATE INDEX idx_online_rank_updates_room_id_timestamp ON online_rank_updates(room_id, timestamp); -- Add room_id index

-- 高能榜用户列表项表 (与 online_rank_updates 关联)
CREATE TABLE online_rank_users (
    rank_update_event_id UUID NOT NULL REFERENCES online_rank_updates(event_id) ON DELETE CASCADE, -- 外键关联
    user_id TEXT NOT NULL,               -- 上榜用户 ID
    username TEXT NOT NULL,             -- 上榜用户名 (Standardized)
    rank INT NOT NULL,                   -- 排名
    score TEXT,                          -- 贡献值/分数 (字符串以兼容不同平台)
    face_url TEXT,                       -- (可选) 用户头像 URL
    PRIMARY KEY (rank_update_event_id, rank) -- 联合主键
);
CREATE INDEX idx_online_rank_users_rank_update_event_id ON online_rank_users(rank_update_event_id);
CREATE INDEX idx_online_rank_users_user_id ON online_rank_users(user_id); -- 如果需要按用户查上榜记录

-- ============================================
--          结构化事件表 (非直播期间)
-- ============================================

-- 非直播期间聊天消息表
CREATE TABLE non_live_chat_messages (
    message_id TEXT PRIMARY KEY,         -- 消息的唯一 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 发送用户 ID
    username TEXT NOT NULL,             -- 发送用户名
    content TEXT NOT NULL,               -- 消息内容
    userlever INT NOT NULL,               -- 用户等级
    admin BOOLEAN NOT NULL,               -- 是否为管理员
    mobileverify BOOLEAN NOT NULL,        -- 手机端验证
    guardlevel INT NOT NULL,               -- 舰长等级
    medal_upname varchar not null ,                   -- 勋章名称
    medal_level INT,
    medal_name varchar not null ,
    medal_color INT,
    medal_uproomid INT,
    medal_upuid INT,
    timestamp TIMESTAMPTZ NOT NULL,      -- 消息发送时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    raw_msg TEXT NOT NULL
);
CREATE INDEX idx_non_live_chat_messages_room_id_timestamp ON non_live_chat_messages(room_id, timestamp);
CREATE INDEX idx_non_live_chat_messages_user_id ON non_live_chat_messages(user_id);

-- 非直播期间礼物赠送表
CREATE TABLE non_live_gifts_sent (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 送礼用户 ID
    username TEXT NOT NULL,             -- 送礼用户名
    gift_id TEXT NOT NULL,               -- 礼物 ID
    gift_name TEXT NOT NULL,             -- 礼物名称
    gift_count INT NOT NULL,             -- 礼物数量
    total_coin BIGINT NOT NULL,          -- 礼物总价值
    coin_type TEXT NOT NULL,             -- 平台货币类型
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_gifts_sent_room_id_timestamp ON non_live_gifts_sent(room_id, timestamp);
CREATE INDEX idx_non_live_gifts_sent_user_id ON non_live_gifts_sent(user_id);
CREATE INDEX idx_non_live_gifts_sent_gift_id ON non_live_gifts_sent(gift_id);

-- 非直播期间大航海购买表
CREATE TABLE non_live_guard_purchases (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 购买用户 ID
    username TEXT NOT NULL,             -- 购买用户名
    guard_level INT NOT NULL,            -- 大航海等级
    guard_name TEXT NOT NULL,            -- 等级名称
    count INT NOT NULL,                  -- 购买数量
    price BIGINT NOT NULL,               -- 价格
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_guard_purchases_room_id_timestamp ON non_live_guard_purchases(room_id, timestamp);
CREATE INDEX idx_non_live_guard_purchases_user_id ON non_live_guard_purchases(user_id);
CREATE INDEX idx_non_live_guard_purchases_guard_level ON non_live_guard_purchases(guard_level);

-- 非直播期间醒目留言表
CREATE TABLE non_live_super_chat_messages (
    message_id TEXT PRIMARY KEY,         -- SC 消息 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 发送用户 ID
    username TEXT NOT NULL,             -- 发送用户名
    content TEXT NOT NULL,               -- SC 内容
    price BIGINT NOT NULL,               -- SC 金额
    duration INT NOT NULL,               -- SC 持续时间
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_super_chat_messages_room_id_timestamp ON non_live_super_chat_messages(room_id, timestamp);
CREATE INDEX idx_non_live_super_chat_messages_user_id ON non_live_super_chat_messages(user_id);

-- 非直播期间用户互动表
CREATE TABLE non_live_user_interactions (
    event_id TEXT PRIMARY KEY,           -- 事件唯一 ID
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 互动用户 ID
    username TEXT NOT NULL,             -- 互动用户名
    interaction_type INT NOT NULL,       -- 互动类型
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_user_interactions_room_id_timestamp ON non_live_user_interactions(room_id, timestamp);
CREATE INDEX idx_non_live_user_interactions_user_id ON non_live_user_interactions(user_id);
CREATE INDEX idx_non_live_user_interactions_interaction_type ON non_live_user_interactions(interaction_type);

-- 非直播期间用户状态表
CREATE TABLE non_live_user_presences (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    room_id TEXT NOT NULL,               -- 关联房间 ID
    user_id TEXT NOT NULL,               -- 用户 ID
    username TEXT NOT NULL,             -- 用户名
    entered BOOLEAN NOT NULL,            -- true 表示进入，false 表示离开
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_user_presences_room_id_timestamp ON non_live_user_presences(room_id, timestamp);
CREATE INDEX idx_non_live_user_presences_user_id ON non_live_user_presences(user_id);

-- 非直播期间观看人数更新表
CREATE TABLE non_live_watched_count_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    room_id TEXT NOT NULL,               -- 关联房间 ID
    count BIGINT NOT NULL,               -- 当前累计观看人数
    text_large TEXT,                     -- (可选) 平台提供的显示文本
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_watched_count_updates_room_id_timestamp ON non_live_watched_count_updates(room_id, timestamp);

-- 非直播期间点赞数更新表
CREATE TABLE non_live_like_count_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    room_id TEXT NOT NULL,               -- 关联房间 ID
    count BIGINT NOT NULL,               -- 当前累计点赞数
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_like_count_updates_room_id_timestamp ON non_live_like_count_updates(room_id, timestamp);

-- 非直播期间高能榜更新事件表
CREATE TABLE non_live_online_rank_updates (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- 使用 UUID 作为主键
    room_id TEXT NOT NULL,               -- 关联房间 ID
    total_count BIGINT,                  -- (可选) 高能榜总人数
    timestamp TIMESTAMPTZ NOT NULL,      -- 事件发生时间
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_non_live_online_rank_updates_room_id_timestamp ON non_live_online_rank_updates(room_id, timestamp);

-- 非直播期间高能榜用户列表项表
CREATE TABLE non_live_online_rank_users (
    rank_update_event_id UUID NOT NULL REFERENCES non_live_online_rank_updates(event_id) ON DELETE CASCADE, -- 外键关联
    user_id TEXT NOT NULL,               -- 上榜用户 ID
    username TEXT NOT NULL,             -- 上榜用户名
    rank INT NOT NULL,                   -- 排名
    score TEXT,                          -- 贡献值/分数
    face_url TEXT,                       -- (可选) 用户头像 URL
    PRIMARY KEY (rank_update_event_id, rank) -- 联合主键
);
CREATE INDEX idx_non_live_online_rank_users_rank_update_event_id ON non_live_online_rank_users(rank_update_event_id);
CREATE INDEX idx_non_live_online_rank_users_user_id ON non_live_online_rank_users(user_id);
package repository

import (
	"context"
	"fmt" // Import fmt for error wrapping

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	// !!! 替换模块路径 !!!
	db "go-xlive/internal/event/repository/db"
)

// OnlineRankUser is a helper struct to pass rank user data to the repository layer
// This avoids direct dependency on eventv1 proto in the repository layer if preferred,
// though using db.InsertOnlineRankUserParams directly is also fine.
type OnlineRankUser struct {
	UserID   string
	Username string // <-- Corrected field name
	Rank     int32
	Score    string
	FaceURL  string
}

// NonLiveOnlineRankUser is a helper struct for non-live rank updates.
type NonLiveOnlineRankUser struct {
	UserID   string
	Username string
	Rank     int32
	Score    string
	FaceURL  string
}

type Repository interface {
	// --- 结构化事件插入方法 (直播期间) ---
	InsertChatMessage(ctx context.Context, arg db.InsertChatMessageParams) error
	InsertGiftSent(ctx context.Context, arg db.InsertGiftSentParams) error
	InsertGuardPurchase(ctx context.Context, arg db.InsertGuardPurchaseParams) error
	InsertSuperChatMessage(ctx context.Context, arg db.InsertSuperChatMessageParams) error
	InsertUserInteraction(ctx context.Context, arg db.InsertUserInteractionParams) error
	InsertUserPresence(ctx context.Context, arg db.InsertUserPresenceParams) error
	InsertStreamStatus(ctx context.Context, arg db.InsertStreamStatusParams) error
	InsertWatchedCountUpdate(ctx context.Context, arg db.InsertWatchedCountUpdateParams) error
	InsertLikeCountUpdate(ctx context.Context, arg db.InsertLikeCountUpdateParams) error
	InsertOnlineRankUpdate(ctx context.Context, updateParams db.InsertOnlineRankUpdateParams, users []OnlineRankUser) error

	// --- 新增: 非直播期间事件插入方法 ---
	InsertNonLiveChatMessage(ctx context.Context, arg db.InsertNonLiveChatMessageParams) error
	InsertNonLiveGiftSent(ctx context.Context, arg db.InsertNonLiveGiftSentParams) error
	InsertNonLiveGuardPurchase(ctx context.Context, arg db.InsertNonLiveGuardPurchaseParams) error
	InsertNonLiveSuperChatMessage(ctx context.Context, arg db.InsertNonLiveSuperChatMessageParams) error
	InsertNonLiveUserInteraction(ctx context.Context, arg db.InsertNonLiveUserInteractionParams) error
	InsertNonLiveUserPresence(ctx context.Context, arg db.InsertNonLiveUserPresenceParams) error
	InsertNonLiveWatchedCountUpdate(ctx context.Context, arg db.InsertNonLiveWatchedCountUpdateParams) error
	InsertNonLiveLikeCountUpdate(ctx context.Context, arg db.InsertNonLiveLikeCountUpdateParams) error
	InsertNonLiveOnlineRankUpdate(ctx context.Context, updateParams db.InsertNonLiveOnlineRankUpdateParams, users []NonLiveOnlineRankUser) error // Use NonLiveOnlineRankUser

	// --- 聚合服务所需的查询方法 ---
	QueryGiftSentBySessionID(ctx context.Context, sessionID string) ([]db.GiftsSent, error)
	QueryChatMessageBySessionID(ctx context.Context, sessionID string) ([]db.ChatMessage, error)
	QueryUserPresenceBySessionID(ctx context.Context, sessionID string) ([]db.UserPresence, error)
	QueryGuardPurchaseBySessionID(ctx context.Context, sessionID string) ([]db.GuardPurchase, error)
	QuerySuperChatMessageBySessionID(ctx context.Context, sessionID string) ([]db.SuperChatMessage, error)
	QueryUserInteractionBySessionID(ctx context.Context, sessionID string) ([]db.UserInteraction, error)

	// --- 获取所有事件的方法 ---
	GetAllSessionEvents(ctx context.Context, sessionID string) ([]db.GetAllSessionEventsRow, error)

	Ping(ctx context.Context) error
}

type postgresRepository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewRepository(p *pgxpool.Pool) Repository { return &postgresRepository{p, db.New(p)} }

// --- 结构化事件插入方法实现 (直播期间) ---
func (r *postgresRepository) InsertChatMessage(ctx context.Context, arg db.InsertChatMessageParams) error {
	return r.queries.InsertChatMessage(ctx, arg)
}
func (r *postgresRepository) InsertGiftSent(ctx context.Context, arg db.InsertGiftSentParams) error {
	return r.queries.InsertGiftSent(ctx, arg)
}
func (r *postgresRepository) InsertGuardPurchase(ctx context.Context, arg db.InsertGuardPurchaseParams) error {
	return r.queries.InsertGuardPurchase(ctx, arg)
}
func (r *postgresRepository) InsertSuperChatMessage(ctx context.Context, arg db.InsertSuperChatMessageParams) error {
	return r.queries.InsertSuperChatMessage(ctx, arg)
}
func (r *postgresRepository) InsertUserInteraction(ctx context.Context, arg db.InsertUserInteractionParams) error {
	return r.queries.InsertUserInteraction(ctx, arg)
}
func (r *postgresRepository) InsertUserPresence(ctx context.Context, arg db.InsertUserPresenceParams) error {
	return r.queries.InsertUserPresence(ctx, arg)
}
func (r *postgresRepository) InsertStreamStatus(ctx context.Context, arg db.InsertStreamStatusParams) error {
	return r.queries.InsertStreamStatus(ctx, arg)
}
func (r *postgresRepository) InsertWatchedCountUpdate(ctx context.Context, arg db.InsertWatchedCountUpdateParams) error {
	return r.queries.InsertWatchedCountUpdate(ctx, arg)
}
func (r *postgresRepository) InsertLikeCountUpdate(ctx context.Context, arg db.InsertLikeCountUpdateParams) error {
	return r.queries.InsertLikeCountUpdate(ctx, arg)
}

// InsertOnlineRankUpdate inserts the rank update event and its associated users within a transaction.
func (r *postgresRepository) InsertOnlineRankUpdate(ctx context.Context, updateParams db.InsertOnlineRankUpdateParams, users []OnlineRankUser) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // Rollback is safe to call even if tx has been committed.

	qtx := r.queries.WithTx(tx)

	// Insert the main update event and get the generated event_id
	eventID, err := qtx.InsertOnlineRankUpdate(ctx, updateParams)
	if err != nil {
		return fmt.Errorf("failed to insert online rank update: %w", err)
	}

	// Insert each user associated with this rank update event
	for _, user := range users {
		userParams := db.InsertOnlineRankUserParams{
			RankUpdateEventID: eventID, // Use the returned event_id
			UserID:            user.UserID,
			Username:          user.Username, // <-- Corrected field name
			Rank:              user.Rank,
			Score:             pgtype.Text{String: user.Score, Valid: user.Score != ""},     // Handle optional score
			FaceUrl:           pgtype.Text{String: user.FaceURL, Valid: user.FaceURL != ""}, // Handle optional face_url
		}
		if err := qtx.InsertOnlineRankUser(ctx, userParams); err != nil {
			// It might be useful to log which user failed here
			return fmt.Errorf("failed to insert online rank user (userID: %s): %w", user.UserID, err)
		}
	}

	// Commit the transaction if all inserts were successful
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// --- 新增: 非直播期间事件插入方法实现 ---
func (r *postgresRepository) InsertNonLiveChatMessage(ctx context.Context, arg db.InsertNonLiveChatMessageParams) error {
	return r.queries.InsertNonLiveChatMessage(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveGiftSent(ctx context.Context, arg db.InsertNonLiveGiftSentParams) error {
	return r.queries.InsertNonLiveGiftSent(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveGuardPurchase(ctx context.Context, arg db.InsertNonLiveGuardPurchaseParams) error {
	return r.queries.InsertNonLiveGuardPurchase(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveSuperChatMessage(ctx context.Context, arg db.InsertNonLiveSuperChatMessageParams) error {
	return r.queries.InsertNonLiveSuperChatMessage(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveUserInteraction(ctx context.Context, arg db.InsertNonLiveUserInteractionParams) error {
	return r.queries.InsertNonLiveUserInteraction(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveUserPresence(ctx context.Context, arg db.InsertNonLiveUserPresenceParams) error {
	return r.queries.InsertNonLiveUserPresence(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveWatchedCountUpdate(ctx context.Context, arg db.InsertNonLiveWatchedCountUpdateParams) error {
	return r.queries.InsertNonLiveWatchedCountUpdate(ctx, arg)
}
func (r *postgresRepository) InsertNonLiveLikeCountUpdate(ctx context.Context, arg db.InsertNonLiveLikeCountUpdateParams) error {
	return r.queries.InsertNonLiveLikeCountUpdate(ctx, arg)
}

// InsertNonLiveOnlineRankUpdate inserts the non-live rank update event and its associated users within a transaction.
func (r *postgresRepository) InsertNonLiveOnlineRankUpdate(ctx context.Context, updateParams db.InsertNonLiveOnlineRankUpdateParams, users []NonLiveOnlineRankUser) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin non-live transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := r.queries.WithTx(tx)

	// Insert the main non-live update event and get the generated event_id
	eventID, err := qtx.InsertNonLiveOnlineRankUpdate(ctx, updateParams)
	if err != nil {
		return fmt.Errorf("failed to insert non-live online rank update: %w", err)
	}

	// Insert each user associated with this non-live rank update event
	for _, user := range users {
		userParams := db.InsertNonLiveOnlineRankUserParams{
			RankUpdateEventID: eventID, // Use the returned event_id
			UserID:            user.UserID,
			Username:          user.Username,
			Rank:              user.Rank,
			Score:             pgtype.Text{String: user.Score, Valid: user.Score != ""},
			FaceUrl:           pgtype.Text{String: user.FaceURL, Valid: user.FaceURL != ""},
		}
		if err := qtx.InsertNonLiveOnlineRankUser(ctx, userParams); err != nil {
			return fmt.Errorf("failed to insert non-live online rank user (userID: %s): %w", user.UserID, err)
		}
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit non-live transaction: %w", err)
	}

	return nil
}

// --- 聚合服务所需的查询方法实现 ---
func (r *postgresRepository) QueryGiftSentBySessionID(ctx context.Context, sessionID string) ([]db.GiftsSent, error) {
	return r.queries.QueryGiftSentBySessionID(ctx, sessionID)
}
func (r *postgresRepository) QueryChatMessageBySessionID(ctx context.Context, sessionID string) ([]db.ChatMessage, error) {
	return r.queries.QueryChatMessageBySessionID(ctx, sessionID)
}
func (r *postgresRepository) QueryUserPresenceBySessionID(ctx context.Context, sessionID string) ([]db.UserPresence, error) {
	return r.queries.QueryUserPresenceBySessionID(ctx, sessionID)
}
func (r *postgresRepository) QueryGuardPurchaseBySessionID(ctx context.Context, sessionID string) ([]db.GuardPurchase, error) {
	return r.queries.QueryGuardPurchaseBySessionID(ctx, sessionID)
}
func (r *postgresRepository) QuerySuperChatMessageBySessionID(ctx context.Context, sessionID string) ([]db.SuperChatMessage, error) {
	return r.queries.QuerySuperChatMessageBySessionID(ctx, sessionID)
}
func (r *postgresRepository) QueryUserInteractionBySessionID(ctx context.Context, sessionID string) ([]db.UserInteraction, error) {
	return r.queries.QueryUserInteractionBySessionID(ctx, sessionID)
}

// --- 获取所有事件的方法实现 ---
func (r *postgresRepository) GetAllSessionEvents(ctx context.Context, sessionID string) ([]db.GetAllSessionEventsRow, error) {
	return r.queries.GetAllSessionEvents(ctx, sessionID)
}

func (r *postgresRepository) Ping(ctx context.Context) error { return r.db.Ping(ctx) }

// --- 移除不再需要的 uuidBytesToString 辅助函数 ---
// func uuidBytesToString(uuidBytes pgtype.UUID) string { ... }

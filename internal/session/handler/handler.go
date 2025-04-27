package handler

import (
	"context"
	"errors"
	sessionv1 "go-xlive/gen/go/session/v1"

	// "go-xlive/internal/session/repository" // No longer needed here
	// db "go-xlive/internal/session/repository/db" // <-- 移除未使用的导入

	"github.com/jackc/pgx/v5" // Still needed for pgx.ErrNoRows check
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BusinessService defines the interface for the session business logic.
// The handler depends on this interface, decoupling it from the implementation.
type BusinessService interface {
	// Corresponds to CreateSession gRPC method
	CreateSessionBiz(ctx context.Context, req *sessionv1.CreateSessionRequest) (*sessionv1.Session, error)
	// Corresponds to GetSession gRPC method
	GetSessionBiz(ctx context.Context, sessionID string) (*sessionv1.Session, error)
	// Corresponds to EndSession gRPC method
	EndSessionBiz(ctx context.Context, sessionID string) (*sessionv1.Session, error)
	// Corresponds to GetLiveSessionByRoomID gRPC method
	GetLiveSessionByRoomIDBiz(ctx context.Context, roomID string) (*sessionv1.Session, error)
	// Corresponds to UpdateSessionAggregates gRPC method
	UpdateSessionAggregatesBiz(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.Session, error)
	// Corresponds to DeleteSession gRPC method
	DeleteSessionBiz(ctx context.Context, sessionID string) error
	// Corresponds to HealthCheck gRPC method
	HealthCheckBiz(ctx context.Context) (string, error)
	// Corresponds to SendChatMessage gRPC method
	SendChatMessageBiz(ctx context.Context, req *sessionv1.SendChatMessageRequest) (string, error)
	// Corresponds to CheckSessionActive gRPC method
	CheckSessionActiveBiz(ctx context.Context, roomID string) (bool, string, error)
	// Corresponds to ListSessions gRPC method
	ListSessionsBiz(ctx context.Context, req *sessionv1.ListSessionsRequest) ([]*sessionv1.Session, int32, error)
	// Corresponds to GetSessionDetails gRPC method (can reuse GetSessionBiz or have a separate one if details differ)
	GetSessionDetailsBiz(ctx context.Context, sessionID string) (*sessionv1.Session, error)
	// Corresponds to ListLiveSessions gRPC method
	ListLiveSessions(ctx context.Context, req *sessionv1.ListLiveSessionsRequest) (*sessionv1.ListLiveSessionsResponse, error)
}

// Handler implements sessionv1.SessionServiceServer interface, handling gRPC requests.
type Handler struct {
	sessionv1.UnimplementedSessionServiceServer
	logger *zap.Logger
	biz    BusinessService // Inject business logic service
}

// NewHandler creates a new Handler instance.
func NewHandler(logger *zap.Logger, biz BusinessService) *Handler {
	if biz == nil {
		panic("BusinessService cannot be nil") // Or handle more gracefully
	}
	return &Handler{
		logger: logger.Named("session_handler"),
		biz:    biz,
	}
}

// mapErrorToGrpcStatus converts business logic errors to gRPC status codes.
// It prioritizes existing gRPC status errors, then checks for common DB errors like ErrNoRows.
func (h *Handler) mapErrorToGrpcStatus(err error, logMsg string, fields ...zap.Field) error {
	if err == nil {
		return nil
	}

	logFields := append([]zap.Field{zap.Error(err)}, fields...) // Add error to log fields

	// Check if the error is already a gRPC status error
	if st, ok := status.FromError(err); ok {
		// Log based on the gRPC code
		if st.Code() >= codes.InvalidArgument && st.Code() < codes.Internal {
			h.logger.Warn(logMsg+" - gRPC Status", append(logFields, zap.String("grpc_code", st.Code().String()))...)
		} else {
			h.logger.Error(logMsg+" - gRPC Status", append(logFields, zap.String("grpc_code", st.Code().String()))...)
		}
		return err // Return the original gRPC status error
	}

	// Map specific known errors (like database not found)
	if errors.Is(err, pgx.ErrNoRows) {
		h.logger.Info(logMsg+" - Not Found", logFields...) // Log NotFound as Info or Warn
		return status.Errorf(codes.NotFound, "%s: not found", logMsg)
	}

	// --- Remove checks for undefined repository errors ---
	// if errors.Is(err, repository.ErrAlreadyExists) { ... }
	// if errors.Is(err, repository.ErrInvalidArgument) { ... }
	// if errors.Is(err, repository.ErrFailedPrecondition) { ... }

	// Default to Internal error for unmapped errors
	h.logger.Error(logMsg+" - Internal Error", logFields...) // Log unexpected errors as Error
	return status.Errorf(codes.Internal, "%s: %v", logMsg, err)
}

// CreateSession handles the gRPC request for creating a session.
func (h *Handler) CreateSession(ctx context.Context, req *sessionv1.CreateSessionRequest) (*sessionv1.CreateSessionResponse, error) {
	h.logger.Debug("Handler: Received CreateSession request", zap.String("room_id", req.GetRoomId()))

	if req.GetRoomId() == "" {
		h.logger.Warn("CreateSession validation failed: Room ID is empty")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}

	session, err := h.biz.CreateSessionBiz(ctx, req)
	if err != nil {
		// Use the updated mapErrorToGrpcStatus
		return nil, h.mapErrorToGrpcStatus(err, "CreateSession failed", zap.String("room_id", req.GetRoomId()))
	}

	h.logger.Info("Handler: CreateSession successful", zap.String("session_id", session.SessionId))
	return &sessionv1.CreateSessionResponse{Session: session}, nil
}

// GetSession handles the gRPC request for getting session information.
func (h *Handler) GetSession(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	h.logger.Debug("Handler: Received GetSession request", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		h.logger.Warn("GetSession validation failed: Session ID is empty")
		return nil, status.Error(codes.InvalidArgument, "场次 ID 不能为空")
	}

	session, err := h.biz.GetSessionBiz(ctx, req.GetSessionId())
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "GetSession failed", zap.String("session_id", req.GetSessionId()))
	}

	h.logger.Debug("Handler: GetSession successful", zap.String("session_id", session.SessionId))
	return &sessionv1.GetSessionResponse{Session: session}, nil
}

// EndSession handles the gRPC request for ending a session.
func (h *Handler) EndSession(ctx context.Context, req *sessionv1.EndSessionRequest) (*sessionv1.EndSessionResponse, error) {
	h.logger.Debug("Handler: Received EndSession request", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		h.logger.Warn("EndSession validation failed: Session ID is empty")
		return nil, status.Error(codes.InvalidArgument, "session_id 不能为空")
	}

	session, err := h.biz.EndSessionBiz(ctx, req.GetSessionId())
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "EndSession failed", zap.String("session_id", req.GetSessionId()))
	}

	h.logger.Info("Handler: EndSession successful", zap.String("session_id", session.SessionId))
	return &sessionv1.EndSessionResponse{Session: session}, nil
}

// GetLiveSessionByRoomID handles the gRPC request for getting the live session by room ID.
func (h *Handler) GetLiveSessionByRoomID(ctx context.Context, req *sessionv1.GetLiveSessionByRoomIDRequest) (*sessionv1.GetLiveSessionByRoomIDResponse, error) {
	h.logger.Debug("Handler: Received GetLiveSessionByRoomID request", zap.String("room_id", req.GetRoomId()))
	if req.GetRoomId() == "" {
		h.logger.Warn("GetLiveSessionByRoomID validation failed: Room ID is empty")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}

	session, err := h.biz.GetLiveSessionByRoomIDBiz(ctx, req.GetRoomId())
	if err != nil {
		// mapErrorToGrpcStatus will handle pgx.ErrNoRows correctly now
		return nil, h.mapErrorToGrpcStatus(err, "GetLiveSessionByRoomID failed", zap.String("room_id", req.GetRoomId()))
	}

	h.logger.Debug("Handler: GetLiveSessionByRoomID successful", zap.String("session_id", session.SessionId))
	return &sessionv1.GetLiveSessionByRoomIDResponse{Session: session}, nil
}

// UpdateSessionAggregates handles the gRPC request for updating session aggregates.
func (h *Handler) UpdateSessionAggregates(ctx context.Context, req *sessionv1.UpdateSessionAggregatesRequest) (*sessionv1.UpdateSessionAggregatesResponse, error) {
	h.logger.Debug("Handler: Received UpdateSessionAggregates request", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		h.logger.Warn("UpdateSessionAggregates validation failed: Session ID is empty")
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id")
	}

	updatedSession, err := h.biz.UpdateSessionAggregatesBiz(ctx, req)
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "UpdateSessionAggregates failed", zap.String("session_id", req.GetSessionId()))
	}

	h.logger.Info("Handler: UpdateSessionAggregates successful", zap.String("session_id", updatedSession.SessionId))
	return &sessionv1.UpdateSessionAggregatesResponse{UpdatedSession: updatedSession}, nil
}

// DeleteSession handles the gRPC request for deleting a session.
func (h *Handler) DeleteSession(ctx context.Context, req *sessionv1.DeleteSessionRequest) (*emptypb.Empty, error) {
	h.logger.Debug("Handler: Received DeleteSession request", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		h.logger.Warn("DeleteSession validation failed: Session ID is empty")
		return nil, status.Error(codes.InvalidArgument, "场次 ID 不能为空")
	}

	err := h.biz.DeleteSessionBiz(ctx, req.GetSessionId())
	if err != nil {
		// Replicate idempotency: treat NotFound as success.
		if errors.Is(err, pgx.ErrNoRows) {
			h.logger.Info("DeleteSession: Session not found, treated as success", zap.String("session_id", req.GetSessionId()))
			return &emptypb.Empty{}, nil
		}
		// Map other errors
		return nil, h.mapErrorToGrpcStatus(err, "DeleteSession failed", zap.String("session_id", req.GetSessionId()))
	}

	h.logger.Info("Handler: DeleteSession successful", zap.String("session_id", req.GetSessionId()))
	return &emptypb.Empty{}, nil
}

// HealthCheck handles the gRPC health check request.
func (h *Handler) HealthCheck(ctx context.Context, req *emptypb.Empty) (*sessionv1.HealthCheckResponse, error) {
	h.logger.Debug("Handler: Received HealthCheck request")
	healthStatus, err := h.biz.HealthCheckBiz(ctx)
	if err != nil {
		h.logger.Error("HealthCheck business logic failed", zap.Error(err))
		// Return the status determined by biz layer even on error
	}
	return &sessionv1.HealthCheckResponse{Status: healthStatus}, nil
}

// SendChatMessage handles the gRPC request for sending a chat message.
func (h *Handler) SendChatMessage(ctx context.Context, req *sessionv1.SendChatMessageRequest) (*sessionv1.SendChatMessageResponse, error) {
	h.logger.Debug("Handler: Received SendChatMessage request", zap.String("session_id", req.GetSessionId()), zap.String("user_id", req.GetUserId()))
	if req.GetSessionId() == "" || req.GetUserId() == "" || req.GetContent() == "" {
		h.logger.Warn("SendChatMessage validation failed: Missing required fields")
		return nil, status.Error(codes.InvalidArgument, "必须提供 session_id, user_id 和 content")
	}

	messageID, err := h.biz.SendChatMessageBiz(ctx, req)
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "SendChatMessage failed", zap.String("session_id", req.GetSessionId()), zap.String("user_id", req.GetUserId()))
	}

	h.logger.Info("Handler: SendChatMessage successful", zap.String("message_id", messageID))
	return &sessionv1.SendChatMessageResponse{MessageId: messageID, Success: true}, nil
}

// CheckSessionActive handles the gRPC request to check if a session is active.
func (h *Handler) CheckSessionActive(ctx context.Context, req *sessionv1.CheckSessionActiveRequest) (*sessionv1.CheckSessionActiveResponse, error) {
	h.logger.Debug("Handler: Received CheckSessionActive request", zap.String("room_id", req.GetRoomId()))
	if req.GetRoomId() == "" {
		h.logger.Warn("CheckSessionActive validation failed: Room ID is empty")
		return nil, status.Error(codes.InvalidArgument, "内部房间 ID 不能为空")
	}

	isActive, sessionID, err := h.biz.CheckSessionActiveBiz(ctx, req.GetRoomId())
	if err != nil {
		// Don't treat NotFound as gRPC error here, just return isActive=false
		if errors.Is(err, pgx.ErrNoRows) {
			h.logger.Debug("CheckSessionActive: No active session found", zap.String("room_id", req.GetRoomId()))
			return &sessionv1.CheckSessionActiveResponse{IsActive: false, SessionId: ""}, nil
		}
		// Handle other potential errors
		return nil, h.mapErrorToGrpcStatus(err, "CheckSessionActive failed", zap.String("room_id", req.GetRoomId()))
	}

	h.logger.Debug("Handler: CheckSessionActive successful", zap.Bool("is_active", isActive), zap.String("session_id", sessionID))
	return &sessionv1.CheckSessionActiveResponse{IsActive: isActive, SessionId: sessionID}, nil
}

// ListSessions handles the gRPC request for listing sessions.
func (h *Handler) ListSessions(ctx context.Context, req *sessionv1.ListSessionsRequest) (*sessionv1.ListSessionsResponse, error) {
	h.logger.Debug("Handler: Received ListSessions request",
		zap.Int32("page", req.GetPage()),
		zap.Int32("page_size", req.GetPageSize()),
		zap.String("status_filter", req.GetStatus()),
		zap.String("room_id_filter", req.GetRoomId()),
		zap.String("owner_id_filter", req.GetOwnerUserId()))

	if req.GetPage() < 1 {
		return nil, status.Error(codes.InvalidArgument, "页码必须大于 0")
	}
	if req.GetPageSize() < 1 || req.GetPageSize() > 100 {
		return nil, status.Error(codes.InvalidArgument, "每页数量必须在 1-100 之间")
	}

	sessions, total, err := h.biz.ListSessionsBiz(ctx, req)
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "ListSessions failed")
	}

	h.logger.Debug("Handler: ListSessions successful", zap.Int("count", len(sessions)), zap.Int32("total", total))
	return &sessionv1.ListSessionsResponse{
		Sessions: sessions,
		Total:    total,
		Page:     req.GetPage(),
		PageSize: req.GetPageSize(),
	}, nil
}

// GetSessionDetails handles the gRPC request for getting session details.
func (h *Handler) GetSessionDetails(ctx context.Context, req *sessionv1.GetSessionRequest) (*sessionv1.GetSessionResponse, error) {
	h.logger.Debug("Handler: Received GetSessionDetails request", zap.String("session_id", req.GetSessionId()))
	if req.GetSessionId() == "" {
		h.logger.Warn("GetSessionDetails validation failed: Session ID is empty")
		return nil, status.Error(codes.InvalidArgument, "场次 ID 不能为空")
	}

	session, err := h.biz.GetSessionDetailsBiz(ctx, req.GetSessionId())
	if err != nil {
		return nil, h.mapErrorToGrpcStatus(err, "GetSessionDetails failed", zap.String("session_id", req.GetSessionId()))
	}

	h.logger.Debug("Handler: GetSessionDetails successful", zap.String("session_id", session.SessionId))
	return &sessionv1.GetSessionResponse{Session: session}, nil
}

// ListLiveSessions handles the gRPC request for listing live session IDs.
func (h *Handler) ListLiveSessions(ctx context.Context, req *sessionv1.ListLiveSessionsRequest) (*sessionv1.ListLiveSessionsResponse, error) {
	h.logger.Debug("Handler: Received ListLiveSessions request")

	// No request parameters to validate for now

	resp, err := h.biz.ListLiveSessions(ctx, req)
	if err != nil {
		// Use the mapErrorToGrpcStatus helper
		return nil, h.mapErrorToGrpcStatus(err, "ListLiveSessions failed")
	}

	h.logger.Debug("Handler: ListLiveSessions successful", zap.Int("count", len(resp.GetSessionIds())))
	return resp, nil
}

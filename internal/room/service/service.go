package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings" // 确保 strings 包已导入
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype" // <-- 需要导入 pgtype 处理 nullable 文本
	"github.com/nats-io/nats.go"

	// "google.golang.org/protobuf/types/known/timestamppb" // 移除 Timestamp 导入，使用 Unix 时间戳

	// !!! 替换模块路径 !!!
	// eventv1 "go-xlive/gen/go/event/v1" // 移除 eventv1 导入，使用 JSON 结构体
	roomv1 "go-xlive/gen/go/room/v1"
	sessionv1 "go-xlive/gen/go/session/v1" // <-- 保持导入，HealthCheck 需要
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/internal/room/repository"
	db "go-xlive/internal/room/repository/db"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	// "google.golang.org/protobuf/proto" // 移除 proto 导入，不再用于 NATS 事件序列化
	"google.golang.org/protobuf/types/known/emptypb"
)

// NATS 主题常量
const (
	NatsRoomCreatedSubject             = "rooms.event.created"              // JSON 格式
	NatsRoomUpdatedSubject             = "rooms.event.updated"              // JSON 格式 (内部更新)
	NatsRoomDeletedSubject             = "rooms.event.deleted"              // JSON 格式 (新增)
	NatsPlatformRoomInfoUpdatedSubject = "platform.event.room.info.updated" // JSON 格式 (来自 Adapter)
)

// RoomEventData 定义 NATS 创建/更新/删除事件消息结构 (JSON) - 合并结构
type RoomEventData struct {
	RoomID         string `json:"room_id"`
	Platform       string `json:"platform"`
	PlatformRoomID string `json:"platform_room_id"`
	Status         string `json:"status"` // "active", "inactive" etc.
	OwnerUserID    string `json:"owner_user_id"`
	EventType      string `json:"event_type"`          // "created", "updated", "deleted"
	Timestamp      int64  `json:"timestamp"`           // Unix 毫秒时间戳
	RoomName       string `json:"room_name,omitempty"` // 可选，用于 updated 事件
	AreaName       string `json:"area_name,omitempty"` // 可选，用于 updated 事件
}

// PlatformRoomInfoUpdatedEventJSON 定义 NATS 平台信息更新事件消息结构 (JSON)
type PlatformRoomInfoUpdatedEventJSON struct {
	RoomID         string `json:"room_id"`
	Platform       string `json:"platform"`
	PlatformRoomID string `json:"platform_room_id"`
	RoomName       string `json:"room_name"`   // 对应旧 Protobuf 的 Title/RoomName
	AnchorName     string `json:"anchor_name"` // <-- 新增: 主播名称
	AreaName       string `json:"area_name"`
	Timestamp      int64  `json:"timestamp"` // Unix 毫秒时间戳 (可选，由发布者提供)
}

type RoomService struct {
	roomv1.UnimplementedRoomServiceServer
	repo          repository.Repository
	logger        *zap.Logger
	userClient    userv1.UserServiceClient
	sessionClient sessionv1.SessionServiceClient // <-- 保持依赖，用于 HealthCheck
	natsConn      *nats.Conn
	natsSubs      []*nats.Subscription // <-- 存储 NATS 订阅
}

// NewRoomService - 构造函数
// <-- 保持 sessionClient 参数，因为 HealthCheck 仍需要
func NewRoomService(logger *zap.Logger, repo repository.Repository, userClient userv1.UserServiceClient, sessionClient sessionv1.SessionServiceClient, natsConn *nats.Conn) *RoomService {
	if logger == nil {
		log.Fatal("RoomService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("RoomService 需要 Repository")
	}
	if userClient == nil {
		log.Fatal("RoomService 需要 UserServiceClient")
	}
	if sessionClient == nil { // <-- 保持检查
		log.Fatal("RoomService 需要 SessionServiceClient")
	}
	if natsConn == nil {
		log.Fatal("RoomService 需要 NATS Connection")
	}
	s := &RoomService{
		logger:        logger.Named("room_service"), // 添加名字空间
		repo:          repo,
		userClient:    userClient,
		sessionClient: sessionClient, // <-- 保持存储
		natsConn:      natsConn,
		natsSubs:      make([]*nats.Subscription, 0, 1), // 初始化订阅列表
	}

	// --- 启动 NATS 订阅 ---
	s.subscribeToPlatformUpdates()

	return s
}

// --- Helper: 将 db.Room 转换为 roomv1.Room ---
func mapDBRoomToProto(dbRoom db.Room) *roomv1.Room {
	var areaName string
	if dbRoom.AreaName.Valid {
		areaName = dbRoom.AreaName.String
	}
	// <-- 新增: 映射 AnchorName -->
	var anchorName string
	if dbRoom.AnchorName.Valid { // 假设 db.Room 结构体现在有 AnchorName 字段 (需要 sqlc 更新)
		anchorName = dbRoom.AnchorName.String
	}
	return &roomv1.Room{
		RoomId:         dbRoom.RoomID,
		RoomName:       dbRoom.RoomName,
		OwnerUserId:    dbRoom.OwnerUserID,
		Status:         dbRoom.Status,
		Platform:       dbRoom.Platform,
		PlatformRoomId: dbRoom.PlatformRoomID,
		AreaName:       areaName,
		AnchorName:     anchorName, // <-- 新增
	}
}

// --- Helper: 发布 NATS 事件 (JSON 格式) ---
func (s *RoomService) publishNatsJSONEvent(subject string, event interface{}) {
	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.Error("序列化 NATS JSON 事件失败", zap.String("subject", subject), zap.Error(err))
		return // 仅记录错误
	}
	if err := s.natsConn.Publish(subject, payload); err != nil {
		s.logger.Error("发布 NATS JSON 事件失败", zap.String("subject", subject), zap.Error(err))
	} else {
		s.logger.Info("已发布 NATS JSON 事件", zap.String("subject", subject)) // 可以添加事件详情日志
	}
}

// CreateRoom 创建房间 - 改进版：只记录意图，发布事件
func (s *RoomService) CreateRoom(ctx context.Context, req *roomv1.CreateRoomRequest) (*roomv1.CreateRoomResponse, error) {
	s.logger.Info("收到 CreateRoom 请求 (改进版)",
		zap.String("owner_id", req.GetOwnerUserId()),
		zap.String("platform", req.GetPlatform()),
		zap.String("platform_room_id", req.GetPlatformRoomId()),
	)
	// <-- 修改: 移除对 room_name 的检查 -->
	if req.GetOwnerUserId() == "" || req.GetPlatform() == "" || req.GetPlatformRoomId() == "" {
		s.logger.Warn("CreateRoom 失败：缺少必要参数 (owner_id, platform, platform_room_id)")
		return nil, status.Error(codes.InvalidArgument, "房主ID, 平台, 平台房间ID 均不能为空")
	}

	// 验证房主 ID (保持不变)
	s.logger.Debug("正在验证房主 ID", zap.String("owner_id", req.GetOwnerUserId()))
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := s.userClient.GetUser(callCtx, &userv1.GetUserRequest{UserId: req.GetOwnerUserId()})
	if err != nil {
		s.logger.Error("验证房主 ID 失败", zap.String("owner_id", req.GetOwnerUserId()), zap.Error(err))
		st, _ := status.FromError(err)
		if st.Code() == codes.NotFound {
			return nil, status.Errorf(codes.InvalidArgument, "房主 ID %s 不存在", req.GetOwnerUserId())
		} else if st.Code() == codes.DeadlineExceeded {
			return nil, status.Error(codes.Unavailable, "验证房主超时，请稍后重试")
		}
		return nil, status.Errorf(codes.Internal, "验证房主失败: %v", err)
	}
	s.logger.Info("房主 ID 验证成功", zap.String("owner_id", req.GetOwnerUserId()))

	roomID := uuid.NewString()
	roomStatus := "active" // 默认创建后为 active
	// <-- 修改: RoomName 使用 platform_room_id 作为初始占位符 -->
	initialRoomName := req.GetPlatformRoomId()

	params := db.CreateRoomParams{
		RoomID:         roomID,
		RoomName:       initialRoomName, // <-- 使用占位符
		OwnerUserID:    req.GetOwnerUserId(),
		Status:         roomStatus,
		Platform:       req.GetPlatform(),
		PlatformRoomID: req.GetPlatformRoomId(),
		AreaName:       pgtype.Text{Valid: false}, // 初始为空
		// AnchorName:     pgtype.Text{Valid: false}, // <-- 移除: CreateRoomParams 没有此字段
	}
	createdDBRoom, err := s.repo.CreateRoom(ctx, params)
	if err != nil {
		s.logger.Error("在数据库中创建房间失败", zap.Error(err))
		// 检查是否是唯一约束冲突 (平台+平台ID)
		if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
			s.logger.Warn("创建房间失败：平台和平台房间 ID 组合已存在",
				zap.String("platform", req.GetPlatform()),
				zap.String("platform_room_id", req.GetPlatformRoomId()),
			)
			// 尝试获取现有房间并返回
			existingRoom, getErr := s.GetRoomByPlatformID(ctx, &roomv1.GetRoomByPlatformIDRequest{
				Platform:       req.GetPlatform(),
				PlatformRoomId: req.GetPlatformRoomId(),
			})
			if getErr == nil && existingRoom != nil && existingRoom.Room != nil {
				s.logger.Info("由于房间已存在，返回现有房间信息", zap.String("existing_room_id", existingRoom.Room.RoomId))
				// --- 即使房间已存在，也发布一次 created 事件，确保适配器会检查并启动监听 ---
				existingEventData := RoomEventData{
					RoomID:         existingRoom.Room.RoomId,
					Platform:       existingRoom.Room.Platform,
					PlatformRoomID: existingRoom.Room.PlatformRoomId,
					Status:         existingRoom.Room.Status,
					OwnerUserID:    existingRoom.Room.OwnerUserId,
					EventType:      "created", // 即使是获取现有，也当作 created 事件通知下游
					Timestamp:      time.Now().UnixMilli(),
				}
				s.publishNatsJSONEvent(NatsRoomCreatedSubject, existingEventData)
				// --- 事件发布结束 ---
				return &roomv1.CreateRoomResponse{Room: existingRoom.Room}, nil // 返回现有房间
			}
			// 如果获取现有房间也失败，则返回冲突错误
			return nil, status.Errorf(codes.AlreadyExists, "房间已存在 (平台: %s, 平台ID: %s)，但获取信息失败", req.GetPlatform(), req.GetPlatformRoomId())
		}
		return nil, status.Errorf(codes.Internal, "创建房间失败: %v", err)
	}
	protoRoom := mapDBRoomToProto(createdDBRoom) // 映射包含占位符的房间信息

	// --- 修改: 移除直接调用 Session Service 创建会话 ---

	// --- 修改: 发布 NATS 房间创建事件 ---
	eventData := RoomEventData{
		RoomID:         protoRoom.RoomId,
		Platform:       protoRoom.Platform,
		PlatformRoomID: protoRoom.PlatformRoomId,
		Status:         protoRoom.Status,
		OwnerUserID:    protoRoom.OwnerUserId,
		EventType:      "created",
		Timestamp:      time.Now().UnixMilli(),
	}
	s.publishNatsJSONEvent(NatsRoomCreatedSubject, eventData)
	// --- 事件发布结束 ---

	s.logger.Info("房间创建意图记录成功，已发布创建事件", zap.String("room_id", protoRoom.RoomId))
	return &roomv1.CreateRoomResponse{Room: protoRoom}, nil // 返回包含占位符的房间信息
}

// GetRoom 实现 (保持不变，但返回的 Room 可能包含占位符名称，直到被平台信息更新)
func (s *RoomService) GetRoom(ctx context.Context, req *roomv1.GetRoomRequest) (*roomv1.GetRoomResponse, error) {
	s.logger.Info("收到 GetRoom 请求", zap.String("room_id", req.GetRoomId()))
	if req.GetRoomId() == "" {
		s.logger.Warn("GetRoom 失败：房间 ID 不能为空")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}
	dbRoom, err := s.repo.GetRoomByID(ctx, req.GetRoomId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("房间未找到", zap.String("room_id", req.GetRoomId()))
			return nil, status.Errorf(codes.NotFound, "未找到房间，ID: %s", req.GetRoomId())
		}
		s.logger.Error("从数据库获取房间失败", zap.Error(err), zap.String("room_id", req.GetRoomId()))
		return nil, status.Errorf(codes.Internal, "获取房间信息失败: %v", err)
	}
	protoRoom := mapDBRoomToProto(dbRoom)

	var ownerInfo *userv1.User
	s.logger.Debug("正在获取房主信息", zap.String("owner_id", protoRoom.OwnerUserId))
	callCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	userResp, err := s.userClient.GetUser(callCtx, &userv1.GetUserRequest{UserId: protoRoom.OwnerUserId})
	if err != nil {
		st, _ := status.FromError(err)
		switch st.Code() {
		case codes.NotFound:
			s.logger.Error("获取房主信息失败：用户不存在 (数据可能不一致)", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Internal, "房间关联的房主信息丢失")
		case codes.Unavailable, codes.DeadlineExceeded:
			s.logger.Warn("获取房主信息暂时失败 (依赖服务不可用)", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Unavailable, "获取房主信息失败，请稍后重试: %v", err)
		default:
			s.logger.Error("获取房主信息时发生内部错误", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Internal, "获取房主信息失败: %v", err)
		}
	} else {
		ownerInfo = userResp.GetUser()
		s.logger.Debug("获取房主信息成功", zap.String("owner_id", protoRoom.OwnerUserId))
	}
	s.logger.Info("房间信息检索成功", zap.String("room_id", protoRoom.RoomId))
	return &roomv1.GetRoomResponse{Room: protoRoom, Owner: ownerInfo}, nil
}

// UpdateRoom 实现 (只允许更新 status)
func (s *RoomService) UpdateRoom(ctx context.Context, req *roomv1.UpdateRoomRequest) (*roomv1.UpdateRoomResponse, error) {
	roomToUpdate := req.GetRoom()
	mask := req.GetUpdateMask()

	if roomToUpdate == nil || roomToUpdate.GetRoomId() == "" {
		return nil, status.Error(codes.InvalidArgument, "必须提供要更新的房间信息和房间 ID")
	}
	if mask == nil || len(mask.GetPaths()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "必须提供 update_mask 来指定要更新的字段")
	}

	s.logger.Info("收到 UpdateRoom 请求 (只允许更新 status)",
		zap.String("room_id", roomToUpdate.GetRoomId()),
		zap.Strings("update_mask", mask.GetPaths()),
	)

	// 1. 获取现有房间信息
	existingDBRoom, err := s.repo.GetRoomByID(ctx, roomToUpdate.GetRoomId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("尝试更新未找到的房间", zap.String("room_id", roomToUpdate.GetRoomId()))
			return nil, status.Errorf(codes.NotFound, "未找到要更新的房间: %s", roomToUpdate.GetRoomId())
		}
		s.logger.Error("更新房间前获取房间信息失败", zap.Error(err), zap.String("room_id", roomToUpdate.GetRoomId()))
		return nil, status.Errorf(codes.Internal, "获取房间信息失败: %v", err)
	}

	// 2. 根据 FieldMask 更新字段 (只处理 status)
	updatedDBRoom := existingDBRoom // 复制一份用于修改
	needsDBUpdate := false
	originalStatus := existingDBRoom.Status // 记录原始状态用于事件发布

	for _, path := range mask.GetPaths() {
		switch path {
		case "status":
			if updatedDBRoom.Status != roomToUpdate.GetStatus() {
				updatedDBRoom.Status = roomToUpdate.GetStatus()
				needsDBUpdate = true
			}
		default:
			s.logger.Warn("UpdateRoom 请求中包含不支持更新的字段", zap.String("field", path), zap.String("room_id", roomToUpdate.GetRoomId()))
		}
	}

	// 如果没有字段实际发生变化，则直接返回现有信息
	if !needsDBUpdate {
		s.logger.Info("UpdateRoom 请求没有导致任何字段变化", zap.String("room_id", roomToUpdate.GetRoomId()))
		protoRoom := mapDBRoomToProto(existingDBRoom)
		return &roomv1.UpdateRoomResponse{Room: protoRoom}, nil
	}

	// 3. 构造更新参数并执行更新 (只更新 status)
	updateParams := db.UpdateRoomStatusParams{ // <-- 使用专门的 UpdateRoomStatusParams
		RoomID: updatedDBRoom.RoomID,
		Status: updatedDBRoom.Status,
	}
	finalUpdatedDBRoom, err := s.repo.UpdateRoomStatus(ctx, updateParams) // <-- 调用 UpdateRoomStatus
	if err != nil {
		s.logger.Error("更新数据库中的房间状态失败", zap.Error(err), zap.String("room_id", roomToUpdate.GetRoomId()))
		return nil, status.Errorf(codes.Internal, "更新房间状态失败: %v", err)
	}

	// 4. 发布 NATS 房间更新事件 (如果状态改变)
	if finalUpdatedDBRoom.Status != originalStatus {
		eventData := RoomEventData{
			RoomID:         finalUpdatedDBRoom.RoomID,
			Platform:       finalUpdatedDBRoom.Platform,
			PlatformRoomID: finalUpdatedDBRoom.PlatformRoomID,
			Status:         finalUpdatedDBRoom.Status, // 使用更新后的状态
			OwnerUserID:    finalUpdatedDBRoom.OwnerUserID,
			EventType:      "updated",
			Timestamp:      time.Now().UnixMilli(),
		}
		s.publishNatsJSONEvent(NatsRoomUpdatedSubject, eventData)
	}

	// 5. 返回更新后的房间信息
	protoRoom := mapDBRoomToProto(finalUpdatedDBRoom)
	s.logger.Info("房间状态更新成功 (通过 API)", zap.String("room_id", protoRoom.RoomId), zap.String("new_status", protoRoom.Status))
	return &roomv1.UpdateRoomResponse{Room: protoRoom}, nil
}

// ListRooms 列出房间列表 (保持不变)
func (s *RoomService) ListRooms(ctx context.Context, req *roomv1.ListRoomsRequest) (*roomv1.ListRoomsResponse, error) {
	// ... (代码保持不变) ...
	pageSize := req.GetPageSize()
	if pageSize <= 0 {
		pageSize = 20 // 默认每页 20 条
	}
	pageOffset := req.GetPageOffset()
	if pageOffset < 0 {
		pageOffset = 0
	}

	s.logger.Info("收到 ListRooms 请求",
		zap.String("status_filter", req.GetStatusFilter()),
		zap.String("platform_filter", req.GetPlatformFilter()),
		zap.Int32("page_size", pageSize),
		zap.Int32("page_offset", pageOffset),
	)

	params := db.ListRoomsParams{
		StatusFilter:   pgtype.Text{String: req.GetStatusFilter(), Valid: req.GetStatusFilter() != ""},
		PlatformFilter: pgtype.Text{String: req.GetPlatformFilter(), Valid: req.GetPlatformFilter() != ""},
		PageLimit:      pageSize,
		PageOffset:     pageOffset,
	}

	dbRooms, err := s.repo.ListRooms(ctx, params)
	if err != nil {
		s.logger.Error("从数据库列出房间失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "列出房间失败: %v", err)
	}

	protoRooms := make([]*roomv1.Room, 0, len(dbRooms))
	for _, dbRoom := range dbRooms {
		protoRooms = append(protoRooms, mapDBRoomToProto(dbRoom))
	}

	s.logger.Info("房间列表检索成功", zap.Int("retrieved_count", len(protoRooms)))
	return &roomv1.ListRoomsResponse{Rooms: protoRooms}, nil
}

// GetRoomByPlatformID 实现 (保持不变)
func (s *RoomService) GetRoomByPlatformID(ctx context.Context, req *roomv1.GetRoomByPlatformIDRequest) (*roomv1.GetRoomResponse, error) {
	// ... (代码保持不变) ...
	s.logger.Info("收到 GetRoomByPlatformID 请求",
		zap.String("platform", req.GetPlatform()),
		zap.String("platform_room_id", req.GetPlatformRoomId()),
	)
	if req.GetPlatform() == "" || req.GetPlatformRoomId() == "" {
		s.logger.Warn("GetRoomByPlatformID 失败：平台和平台房间 ID 均不能为空")
		return nil, status.Error(codes.InvalidArgument, "平台和平台房间 ID 均不能为空")
	}

	params := db.GetRoomByPlatformIDParams{
		Platform:       req.GetPlatform(),
		PlatformRoomID: req.GetPlatformRoomId(),
	}
	dbRoom, err := s.repo.GetRoomByPlatformID(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("未找到指定平台的房间", zap.String("platform", req.GetPlatform()), zap.String("platform_room_id", req.GetPlatformRoomId()))
			return nil, status.Errorf(codes.NotFound, "未找到房间，平台: %s, 平台ID: %s", req.GetPlatform(), req.GetPlatformRoomId())
		}
		s.logger.Error("根据平台 ID 从数据库获取房间失败", zap.Error(err), zap.String("platform", req.GetPlatform()), zap.String("platform_room_id", req.GetPlatformRoomId()))
		return nil, status.Errorf(codes.Internal, "获取房间信息失败: %v", err)
	}
	protoRoom := mapDBRoomToProto(dbRoom)

	var ownerInfo *userv1.User
	s.logger.Debug("正在获取房主信息 (GetRoomByPlatformID)", zap.String("owner_id", protoRoom.OwnerUserId))
	callCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	userResp, err := s.userClient.GetUser(callCtx, &userv1.GetUserRequest{UserId: protoRoom.OwnerUserId})
	if err != nil {
		st, _ := status.FromError(err)
		switch st.Code() {
		case codes.NotFound:
			s.logger.Error("获取房主信息失败 (GetRoomByPlatformID)：用户不存在", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Internal, "房间关联的房主信息丢失")
		case codes.Unavailable, codes.DeadlineExceeded:
			s.logger.Warn("获取房主信息暂时失败 (GetRoomByPlatformID)", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Unavailable, "获取房主信息失败，请稍后重试: %v", err)
		default:
			s.logger.Error("获取房主信息时发生内部错误 (GetRoomByPlatformID)", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
			return nil, status.Errorf(codes.Internal, "获取房主信息失败: %v", err)
		}
	} else {
		ownerInfo = userResp.GetUser()
		s.logger.Debug("获取房主信息成功 (GetRoomByPlatformID)", zap.String("owner_id", protoRoom.OwnerUserId))
	}

	s.logger.Info("根据平台 ID 检索房间信息成功", zap.String("room_id", protoRoom.RoomId))
	return &roomv1.GetRoomResponse{Room: protoRoom, Owner: ownerInfo}, nil
}

// DeleteRoom 实现 (软删除，结束活跃会话，发布删除事件)
func (s *RoomService) DeleteRoom(ctx context.Context, req *roomv1.DeleteRoomRequest) (*emptypb.Empty, error) {
	roomID := req.GetRoomId()
	s.logger.Info("收到 DeleteRoom (软删除) 请求", zap.String("room_id", roomID))

	if roomID == "" {
		s.logger.Warn("DeleteRoom 失败：房间 ID 不能为空")
		return nil, status.Error(codes.InvalidArgument, "房间 ID 不能为空")
	}

	// 1. 获取房间信息 (GetRoomByID 现在只返回未删除的)
	dbRoom, err := s.repo.GetRoomByID(ctx, roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("尝试软删除的房间未找到或已被软删除，视为成功", zap.String("room_id", roomID))
			return &emptypb.Empty{}, nil // 房间不存在或已删除，幂等处理
		}
		s.logger.Error("软删除房间前获取房间信息失败", zap.Error(err), zap.String("room_id", roomID))
		return nil, status.Errorf(codes.Internal, "获取房间信息失败: %v", err)
	}

	// 2. 检查并结束活跃会话
	s.logger.Debug("正在检查房间是否有活跃会话", zap.String("room_id", roomID))
	sessionCtx, sessionCancel := context.WithTimeout(ctx, 5*time.Second) // 给 Session Service 调用设置超时
	defer sessionCancel()
	liveSessionResp, getSessionErr := s.sessionClient.GetLiveSessionByRoomID(sessionCtx, &sessionv1.GetLiveSessionByRoomIDRequest{RoomId: roomID})

	if getSessionErr == nil && liveSessionResp != nil && liveSessionResp.GetSession() != nil && liveSessionResp.GetSession().GetSessionId() != "" {
		liveSessionID := liveSessionResp.GetSession().GetSessionId()
		s.logger.Info("发现活跃会话，将尝试结束", zap.String("room_id", roomID), zap.String("session_id", liveSessionID))
		endSessionCtx, endSessionCancel := context.WithTimeout(ctx, 5*time.Second)
		defer endSessionCancel()
		_, endSessionErr := s.sessionClient.EndSession(endSessionCtx, &sessionv1.EndSessionRequest{SessionId: liveSessionID})
		if endSessionErr != nil {
			// 记录错误，但继续执行软删除房间的操作
			st, ok := status.FromError(endSessionErr)
			if ok && (st.Code() == codes.NotFound || st.Code() == codes.FailedPrecondition) { // 会话可能已被结束
				s.logger.Warn("尝试结束会话失败 (可能已结束或不存在)，继续软删除房间", zap.String("room_id", roomID), zap.String("session_id", liveSessionID), zap.Error(endSessionErr))
			} else {
				s.logger.Error("尝试结束活跃会话时发生错误，但仍将继续软删除房间", zap.String("room_id", roomID), zap.String("session_id", liveSessionID), zap.Error(endSessionErr))
			}
		} else {
			s.logger.Info("成功结束了关联的活跃会话", zap.String("room_id", roomID), zap.String("session_id", liveSessionID))
		}
	} else if getSessionErr != nil {
		st, ok := status.FromError(getSessionErr)
		if !ok || st.Code() != codes.NotFound { // 只记录非 NotFound 的错误
			s.logger.Error("检查活跃会话时出错，但仍将继续软删除房间", zap.Error(getSessionErr), zap.String("room_id", roomID))
		} else {
			s.logger.Info("房间没有活跃会话", zap.String("room_id", roomID))
		}
	} else {
		s.logger.Info("房间没有活跃会话", zap.String("room_id", roomID))
	}

	// 3. 执行软删除 (调用已修改的 DeleteRoom，现在是 UPDATE)
	err = s.repo.DeleteRoom(ctx, roomID) // DeleteRoom 现在执行 UPDATE rooms SET deleted_at = NOW() ...
	if err != nil {
		// 注意：因为 GetRoomByID 已经确认房间存在且未删除，这里的 ErrNoRows 理论上不应该发生，除非并发删除
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("尝试软删除时房间未找到 (可能被并发删除?)，视为成功", zap.String("room_id", roomID))
			return &emptypb.Empty{}, nil
		}
		s.logger.Error("软删除房间数据库操作失败", zap.Error(err), zap.String("room_id", roomID))
		return nil, status.Errorf(codes.Internal, "软删除房间失败: %v", err)
	}

	// 4. 发布删除事件 (使用从 Get 获取的 dbRoom 信息)
	eventData := RoomEventData{
		RoomID:         dbRoom.RoomID,
		Platform:       dbRoom.Platform,
		PlatformRoomID: dbRoom.PlatformRoomID,
		Status:         "deleted", // 状态标记为 deleted
		OwnerUserID:    dbRoom.OwnerUserID,
		EventType:      "deleted",
		Timestamp:      time.Now().UnixMilli(),
	}
	s.publishNatsJSONEvent(NatsRoomDeletedSubject, eventData)

	s.logger.Info("房间软删除成功，已发布删除事件", zap.String("room_id", roomID))
	return &emptypb.Empty{}, nil
}

// HealthCheck 实现 (保持不变)
func (s *RoomService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*roomv1.HealthCheckResponse, error) {
	// ... (代码保持不变) ...
	s.logger.Debug("执行健康检查 (RoomService)")
	healthStatus := "SERVING"
	var firstError error

	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("依赖健康检查失败: Database", zap.Error(err))
		healthStatus = "NOT_SERVING"
		firstError = err
	}
	if !s.natsConn.IsConnected() {
		s.logger.Error("依赖健康检查失败: NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}

	userCheckCtx, userCancel := context.WithTimeout(ctx, 1*time.Second)
	defer userCancel()
	_, err := s.userClient.HealthCheck(userCheckCtx, &emptypb.Empty{})
	if err != nil {
		s.logger.Error("依赖健康检查失败: UserService", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// 增加 SessionService 健康检查的超时时间
	sessionCheckCtx, sessionCancel := context.WithTimeout(ctx, 3*time.Second) // <-- 增加到 3 秒
	defer sessionCancel()
	_, err = s.sessionClient.HealthCheck(sessionCheckCtx, &emptypb.Empty{})
	if err != nil {
		s.logger.Error("依赖健康检查失败: SessionService", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	if firstError != nil {
		return &roomv1.HealthCheckResponse{Status: "NOT_SERVING"}, nil
	}
	return &roomv1.HealthCheckResponse{Status: healthStatus}, nil
}

// --- NATS 订阅处理 ---

// subscribeToPlatformUpdates 订阅平台房间信息更新事件 (JSON 格式)
func (s *RoomService) subscribeToPlatformUpdates() {
	sub, err := s.natsConn.Subscribe(NatsPlatformRoomInfoUpdatedSubject, s.handlePlatformRoomInfoUpdate)
	if err != nil {
		s.logger.Fatal("订阅 NATS PlatformRoomInfoUpdated 事件失败", zap.String("subject", NatsPlatformRoomInfoUpdatedSubject), zap.Error(err))
	}
	s.natsSubs = append(s.natsSubs, sub)
	s.logger.Info("已成功订阅 NATS PlatformRoomInfoUpdated 事件 (JSON)", zap.String("subject", NatsPlatformRoomInfoUpdatedSubject))
}

// handlePlatformRoomInfoUpdate 处理接收到的平台房间信息更新事件 (JSON 格式)
func (s *RoomService) handlePlatformRoomInfoUpdate(msg *nats.Msg) {
	// <-- 移除未使用的 ctx 和 cancel -->
	// ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// defer cancel()

	var event PlatformRoomInfoUpdatedEventJSON
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		s.logger.Error("解析 NATS PlatformRoomInfoUpdated JSON 事件失败", zap.Error(err), zap.String("subject", msg.Subject))
		return
	}

	s.logger.Info("收到 PlatformRoomInfoUpdated JSON 事件",
		zap.String("room_id", event.RoomID),
		zap.String("platform", event.Platform),
		zap.String("platform_room_id", event.PlatformRoomID),
		zap.String("new_room_name", event.RoomName),
		zap.String("new_anchor_name", event.AnchorName),
		zap.String("new_area", event.AreaName),
	)

	// --- 取消注释并实现数据库更新逻辑 ---
	// 需要一个 context
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 5*time.Second) // 使用独立的 context
	defer updateCancel()

	updateParams := db.UpdateRoomPlatformInfoParams{ // 使用 sqlc 生成的结构体
		RoomID:     event.RoomID,
		RoomName:   event.RoomName,                                                       // 直接使用事件中的 RoomName
		AnchorName: pgtype.Text{String: event.AnchorName, Valid: event.AnchorName != ""}, // 处理 nullable AnchorName
		AreaName:   pgtype.Text{String: event.AreaName, Valid: event.AreaName != ""},     // 处理 nullable AreaName
	}

	_, err := s.repo.UpdateRoomPlatformInfo(updateCtx, updateParams) // 调用 sqlc 生成的方法
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 如果房间未找到 (可能已被软删除)，则忽略此更新事件
			s.logger.Warn("处理 PlatformRoomInfoUpdated 事件：未找到对应的房间 (可能已被删除)，忽略更新", zap.String("room_id", event.RoomID))
			return
		}
		// 其他数据库错误
		s.logger.Error("处理 PlatformRoomInfoUpdated 事件：更新数据库失败", zap.Error(err), zap.String("room_id", event.RoomID))
		return
	}

	s.logger.Info("处理 PlatformRoomInfoUpdated 事件：成功更新数据库", zap.String("room_id", event.RoomID))
	// --- 移除临时日志 ---
	// s.logger.Warn("收到 PlatformRoomInfoUpdated 事件，但数据库更新逻辑尚未实现 (TODO)", zap.String("room_id", event.RoomID))

}

// Close 用于优雅关闭 NATS 订阅 (如果服务需要关闭)
func (s *RoomService) Close() {
	for _, sub := range s.natsSubs {
		if err := sub.Unsubscribe(); err != nil {
			s.logger.Error("取消 NATS 订阅失败", zap.Error(err), zap.String("subject", sub.Subject))
		}
	}
	s.logger.Info("所有 NATS 订阅已取消")
}

package service

import (
	"context"
	// "encoding/json" // 不再需要
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	// !!! 替换模块路径 !!!
	eventv1 "go-xlive/gen/go/event/v1" // 导入事件 proto
	roomv1 "go-xlive/gen/go/room/v1"
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/internal/room/repository"
	db "go-xlive/internal/room/repository/db"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto" // 导入 Protobuf 库
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type RoomService struct {
	roomv1.UnimplementedRoomServiceServer
	repo       repository.Repository
	logger     *zap.Logger
	userClient userv1.UserServiceClient
	natsConn   *nats.Conn
}

func NewRoomService(logger *zap.Logger, repo repository.Repository, userClient userv1.UserServiceClient, natsConn *nats.Conn) *RoomService {
	if logger == nil {
		log.Fatal("RoomService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("RoomService 需要 Repository")
	}
	if userClient == nil {
		log.Fatal("RoomService 需要 UserServiceClient")
	}
	if natsConn == nil {
		log.Fatal("RoomService 需要 NATS Connection")
	}
	return &RoomService{
		logger:     logger.Named("room_service"), // 添加名字空间
		repo:       repo,
		userClient: userClient,
		natsConn:   natsConn,
	}
}

func (s *RoomService) CreateRoom(ctx context.Context, req *roomv1.CreateRoomRequest) (*roomv1.CreateRoomResponse, error) {
	s.logger.Info("收到 CreateRoom 请求", zap.String("name", req.GetRoomName()), zap.String("owner_id", req.GetOwnerUserId()))
	if req.GetRoomName() == "" || req.GetOwnerUserId() == "" {
		s.logger.Warn("CreateRoom 失败：房间名和房主 ID 不能为空")
		return nil, status.Error(codes.InvalidArgument, "房间名和房主ID不能为空")
	}

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
	roomStatus := "active" // 假设新房间默认为活跃状态
	params := db.CreateRoomParams{
		RoomID:      roomID,
		RoomName:    req.GetRoomName(),
		OwnerUserID: req.GetOwnerUserId(),
		Status:      roomStatus,
	}
	createdDBRoom, err := s.repo.CreateRoom(ctx, params)
	if err != nil {
		s.logger.Error("在数据库中创建房间失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "创建房间失败: %v", err)
	}
	protoRoom := &roomv1.Room{
		RoomId:      createdDBRoom.RoomID,
		RoomName:    createdDBRoom.RoomName,
		OwnerUserId: createdDBRoom.OwnerUserID,
		Status:      createdDBRoom.Status,
	}

	// --- 发布房间创建/状态事件到 NATS (使用 Protobuf) ---
	roomEvent := &eventv1.RoomStatusChanged{
		RoomId:    protoRoom.RoomId,
		NewStatus: protoRoom.Status, // 状态为 "active"
		Timestamp: timestamppb.Now(),
	}
	payload, err := proto.Marshal(roomEvent)
	if err != nil {
		s.logger.Error("序列化房间事件失败", zap.Error(err), zap.String("room_id", protoRoom.RoomId))
	} else {
		// 使用更具体的原始事件主题
		subject := fmt.Sprintf("events.raw.room.status.%s", protoRoom.Status) // 主题反映了当前状态
		if err := s.natsConn.Publish(subject, payload); err != nil {
			s.logger.Error("发布房间状态事件到 NATS 失败", zap.Error(err), zap.String("room_id", protoRoom.RoomId))
		} else {
			s.logger.Info("已发布房间状态事件到 NATS", zap.String("subject", subject), zap.String("room_id", protoRoom.RoomId))
		}
	}
	// --- NATS 事件结束 ---

	s.logger.Info("房间创建成功", zap.String("room_id", protoRoom.RoomId))
	return &roomv1.CreateRoomResponse{Room: protoRoom}, nil
}

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
	protoRoom := &roomv1.Room{
		RoomId:      dbRoom.RoomID,
		RoomName:    dbRoom.RoomName,
		OwnerUserId: dbRoom.OwnerUserID,
		Status:      dbRoom.Status,
	}

	var ownerInfo *userv1.User
	s.logger.Debug("正在获取房主信息", zap.String("owner_id", protoRoom.OwnerUserId))
	callCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	userResp, err := s.userClient.GetUser(callCtx, &userv1.GetUserRequest{UserId: protoRoom.OwnerUserId})
	if err != nil {
		s.logger.Warn("获取房主信息失败 (将返回部分房间数据)", zap.String("owner_id", protoRoom.OwnerUserId), zap.Error(err))
	} else {
		ownerInfo = userResp.GetUser()
		s.logger.Debug("获取房主信息成功", zap.String("owner_id", protoRoom.OwnerUserId))
	}

	s.logger.Info("房间信息检索成功", zap.String("room_id", protoRoom.RoomId))
	return &roomv1.GetRoomResponse{Room: protoRoom, Owner: ownerInfo}, nil
}

// HealthCheck 实现
func (s *RoomService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*roomv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (RoomService)")
	healthStatus := "SERVING"
	var firstError error

	// 1. 检查数据库连接
	dbCheckCtx, dbCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dbCancel()
	if err := s.repo.Ping(dbCheckCtx); err != nil {
		s.logger.Error("依赖健康检查失败: Database", zap.Error(err))
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = err
		}
	}

	// 2. 检查 NATS 连接
	if !s.natsConn.IsConnected() {
		s.logger.Error("依赖健康检查失败: NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}

	// 3. 检查 User Service 连接
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

	// 根据检查结果返回状态
	if firstError != nil {
		// 如果有任何依赖失败，整体返回 NOT_SERVING，但不暴露具体内部错误
		return &roomv1.HealthCheckResponse{Status: "NOT_SERVING"}, nil
	}
	return &roomv1.HealthCheckResponse{Status: healthStatus}, nil // 全部通过则返回 SERVING
}

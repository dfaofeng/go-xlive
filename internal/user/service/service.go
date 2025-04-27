package service

import (
	"context"
	"errors"
	"go-xlive/pkg/observability" // <-- 新增: 导入 observability
	"log"
	"time" // 导入 time 包

	"github.com/google/uuid" // 导入 pgconn 用于错误检查
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"                         // <-- 导入 NATS
	"google.golang.org/protobuf/types/known/timestamppb" // <-- 导入 Timestamp

	// !!! 替换模块路径 !!!
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/internal/user/repository"
	db "go-xlive/internal/user/repository/db"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"               // <-- 导入 proto
	"google.golang.org/protobuf/types/known/emptypb" // 导入 Empty
)

// NATS 主题常量
const (
	NatsUserUpdatedSubject = "users.event.updated" // <-- 新增: User 更新事件主题
)

type UserService struct {
	userv1.UnimplementedUserServiceServer
	repo     repository.Repository
	logger   *zap.Logger
	natsConn *nats.Conn // <-- 添加 NATS 连接
}

func NewUserService(logger *zap.Logger, repo repository.Repository, natsConn *nats.Conn) *UserService { // <-- 添加 natsConn 参数
	if logger == nil {
		log.Fatal("UserService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("UserService 需要 Repository")
	}
	if natsConn == nil { // <-- 添加 NATS 连接检查
		log.Fatal("UserService 需要 NATS Connection")
	}
	return &UserService{
		logger:   logger.Named("user_service"), // 添加名字空间
		repo:     repo,
		natsConn: natsConn, // <-- 存储 NATS 连接
	}
}

// --- Helper: 将 db.User 转换为 userv1.User ---
func mapDBUserToProto(dbUser db.User) *userv1.User {
	return &userv1.User{
		UserId:        dbUser.UserID,
		Username:      dbUser.Username,
		FollowerCount: dbUser.FollowerCount, // 映射 FollowerCount
	}
}

// --- Helper: 发布 NATS 事件 (Protobuf 格式) ---
// --- 修改: 修正 InjectNATSHeaders 调用方式 ---
func (s *UserService) publishNatsEvent(ctx context.Context, subject string, event proto.Message) {
	payload, err := proto.Marshal(event)
	if err != nil {
		s.logger.Error("序列化 NATS 事件失败", zap.String("subject", subject), zap.Error(err))
		return // 仅记录错误
	}

	// --- 注入追踪头 (修正) ---
	// InjectNATSHeaders 只接受 ctx，返回包含追踪信息的 map
	headersMap := observability.InjectNATSHeaders(ctx)
	header := nats.Header{}
	// 遍历 map 并设置到 nats.Header
	for k, v := range headersMap {
		header.Set(k, v)
	}
	// --- 注入结束 ---

	// --- 使用 PublishMsg 发布带 Header 的消息 ---
	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header:  header,
	}
	if err := s.natsConn.PublishMsg(msg); err != nil {
		s.logger.Error("发布 NATS 事件失败", zap.String("subject", subject), zap.Error(err))
	} else {
		s.logger.Info("已发布 NATS 事件", zap.String("subject", subject), zap.Any("headers", header)) // 记录 Header 用于调试
	}
	// --- 修改结束 ---
}

func (s *UserService) CreateUser(ctx context.Context, req *userv1.CreateUserRequest) (*userv1.CreateUserResponse, error) {
	s.logger.Info("收到 CreateUser 请求", zap.String("username", req.GetUsername()))
	if req.GetUsername() == "" {
		s.logger.Warn("CreateUser 失败：用户名不能为空")
		return nil, status.Error(codes.InvalidArgument, "用户名不能为空")
	}
	userID := uuid.NewString()
	params := db.CreateUserParams{
		UserID:        userID,
		Username:      req.GetUsername(),
		FollowerCount: 0, // 新用户粉丝数默认为 0
	}
	createdDBUser, err := s.repo.CreateUser(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		// 检查是否为 PostgreSQL 的唯一约束冲突错误 (错误码 23505)
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			s.logger.Warn("创建用户失败：用户名已存在", zap.String("username", req.GetUsername()), zap.Error(err))
			return nil, status.Errorf(codes.AlreadyExists, "用户名 '%s' 已存在", req.GetUsername())
		}
		// 其他数据库错误
		s.logger.Error("在数据库中创建用户失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "创建用户失败: %v", err)
	}
	protoUser := mapDBUserToProto(createdDBUser) // 使用 helper 函数
	s.logger.Info("用户创建成功", zap.String("user_id", protoUser.UserId))
	return &userv1.CreateUserResponse{User: protoUser}, nil
}

func (s *UserService) GetUser(ctx context.Context, req *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	s.logger.Info("收到 GetUser 请求", zap.String("user_id", req.GetUserId()))
	if req.GetUserId() == "" {
		s.logger.Warn("GetUser 失败：用户 ID 不能为空")
		return nil, status.Error(codes.InvalidArgument, "用户 ID 不能为空")
	}
	dbUser, err := s.repo.GetUserByID(ctx, req.GetUserId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("用户未找到", zap.String("user_id", req.GetUserId()))
			return nil, status.Errorf(codes.NotFound, "未找到用户，ID: %s", req.GetUserId())
		}
		s.logger.Error("从数据库获取用户失败", zap.Error(err), zap.String("user_id", req.GetUserId()))
		return nil, status.Errorf(codes.Internal, "获取用户信息失败: %v", err)
	}
	protoUser := mapDBUserToProto(dbUser) // 使用 helper 函数
	s.logger.Info("用户检索成功", zap.String("user_id", protoUser.UserId))
	return &userv1.GetUserResponse{User: protoUser}, nil
}

// --- 新增: UpdateUser 实现 ---
func (s *UserService) UpdateUser(ctx context.Context, req *userv1.UpdateUserRequest) (*userv1.UpdateUserResponse, error) {
	userToUpdate := req.GetUser()
	mask := req.GetUpdateMask()

	if userToUpdate == nil || userToUpdate.GetUserId() == "" {
		return nil, status.Error(codes.InvalidArgument, "必须提供要更新的用户信息和用户 ID")
	}
	if mask == nil || len(mask.GetPaths()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "必须提供 update_mask 来指定要更新的字段")
	}

	s.logger.Info("收到 UpdateUser 请求",
		zap.String("user_id", userToUpdate.GetUserId()),
		zap.Strings("update_mask", mask.GetPaths()),
	)

	// 1. 获取现有用户信息
	existingDBUser, err := s.repo.GetUserByID(ctx, userToUpdate.GetUserId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("尝试更新未找到的用户", zap.String("user_id", userToUpdate.GetUserId()))
			return nil, status.Errorf(codes.NotFound, "未找到要更新的用户: %s", userToUpdate.GetUserId())
		}
		s.logger.Error("更新用户前获取用户信息失败", zap.Error(err), zap.String("user_id", userToUpdate.GetUserId()))
		return nil, status.Errorf(codes.Internal, "获取用户信息失败: %v", err)
	}

	// 2. 根据 FieldMask 更新字段
	updatedDBUser := existingDBUser // 复制一份用于修改
	needsUpdate := false
	for _, path := range mask.GetPaths() {
		switch path {
		case "username":
			if updatedDBUser.Username != userToUpdate.GetUsername() {
				updatedDBUser.Username = userToUpdate.GetUsername()
				needsUpdate = true
			}
		case "follower_count":
			// 注意：follower_count 通常不由外部直接更新，而是通过关注/取关操作间接更新
			// 但如果业务逻辑允许直接修改，可以在这里处理
			if updatedDBUser.FollowerCount != userToUpdate.GetFollowerCount() {
				updatedDBUser.FollowerCount = userToUpdate.GetFollowerCount()
				needsUpdate = true
			}
		default:
			s.logger.Warn("UpdateUser 请求中包含不支持更新的字段", zap.String("field", path), zap.String("user_id", userToUpdate.GetUserId()))
			// return nil, status.Errorf(codes.InvalidArgument, "不支持更新字段: %s", path)
		}
	}

	// 如果没有字段实际发生变化，则直接返回现有信息
	if !needsUpdate {
		s.logger.Info("UpdateUser 请求没有导致任何字段变化", zap.String("user_id", userToUpdate.GetUserId()))
		protoUser := mapDBUserToProto(existingDBUser)
		return &userv1.UpdateUserResponse{User: protoUser}, nil
	}

	// 3. 构造更新参数并执行更新
	updateParams := db.UpdateUserParams{
		UserID:        updatedDBUser.UserID,
		Username:      updatedDBUser.Username,
		FollowerCount: updatedDBUser.FollowerCount,
	}
	finalUpdatedDBUser, err := s.repo.UpdateUser(ctx, updateParams)
	if err != nil {
		// 检查是否是用户名唯一约束冲突
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			s.logger.Warn("更新用户失败：用户名已存在", zap.String("username", updateParams.Username), zap.Error(err))
			return nil, status.Errorf(codes.AlreadyExists, "用户名 '%s' 已被占用", updateParams.Username)
		}
		// 其他错误
		s.logger.Error("更新数据库中的用户信息失败", zap.Error(err), zap.String("user_id", userToUpdate.GetUserId()))
		return nil, status.Errorf(codes.Internal, "更新用户信息失败: %v", err)
	}

	// 4. 发布 UserUpdatedEvent 事件 (使用 Protobuf)
	updatedEvent := &userv1.UserUpdatedEvent{
		UserId:        finalUpdatedDBUser.UserID,
		Username:      finalUpdatedDBUser.Username,
		FollowerCount: finalUpdatedDBUser.FollowerCount,
		Timestamp:     timestamppb.Now(),
	}
	// --- 修改: 传递 ctx ---
	s.publishNatsEvent(ctx, NatsUserUpdatedSubject, updatedEvent) // 使用 helper 发布
	// --- 修改结束 ---

	// 5. 返回更新后的用户信息
	protoUser := mapDBUserToProto(finalUpdatedDBUser)
	s.logger.Info("用户信息更新成功", zap.String("user_id", protoUser.UserId))
	return &userv1.UpdateUserResponse{User: protoUser}, nil
}

// HealthCheck 实现 (添加 NATS 检查)
func (s *UserService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*userv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (UserService)")
	healthStatus := "SERVING"
	var firstError error

	// 1. 检查数据库连接
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second) // 设置检查超时
	defer cancel()
	if err := s.repo.Ping(checkCtx); err != nil { // 调用 Repo 的 Ping
		s.logger.Error("数据库健康检查失败", zap.Error(err))
		healthStatus = "NOT_SERVING"
		firstError = err
	}

	// 2. 检查 NATS 连接
	if !s.natsConn.IsConnected() {
		s.logger.Error("NATS 未连接")
		healthStatus = "NOT_SERVING"
		if firstError == nil {
			firstError = errors.New("NATS not connected")
		}
	}

	if firstError != nil {
		return &userv1.HealthCheckResponse{Status: healthStatus}, nil // 返回 NOT_SERVING，但不返回具体错误给外部
	}
	return &userv1.HealthCheckResponse{Status: "SERVING"}, nil
}

// --- 新增: Bilibili Cookie 管理实现 ---

const systemBilibiliCookieKey = "bilibili_cookie" // 定义数据库中的 key

// SetSystemBilibiliCookie 实现设置系统 Bilibili Cookie 的 RPC
func (s *UserService) SetSystemBilibiliCookie(ctx context.Context, req *userv1.SetSystemBilibiliCookieRequest) (*userv1.SetSystemBilibiliCookieResponse, error) {
	cookie := req.GetCookie()
	if cookie == "" {
		s.logger.Warn("SetSystemBilibiliCookie 失败：Cookie 不能为空")
		return nil, status.Error(codes.InvalidArgument, "Cookie 不能为空")
	}

	s.logger.Info("收到 SetSystemBilibiliCookie 请求") // 不记录 cookie 内容

	// 调用 repository 设置或更新设置项
	err := s.repo.SetSystemSetting(ctx, db.SetSystemSettingParams{
		SettingKey:   systemBilibiliCookieKey,
		SettingValue: cookie,
	})

	if err != nil {
		s.logger.Error("设置系统 Bilibili Cookie 失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "设置 Cookie 失败: %v", err)
	}

	s.logger.Info("系统 Bilibili Cookie 设置成功")
	return &userv1.SetSystemBilibiliCookieResponse{Success: true}, nil
}

// GetSystemBilibiliCookie 实现获取系统 Bilibili Cookie 的 RPC
func (s *UserService) GetSystemBilibiliCookie(ctx context.Context, req *userv1.GetSystemBilibiliCookieRequest) (*userv1.GetSystemBilibiliCookieResponse, error) {
	s.logger.Info("收到 GetSystemBilibiliCookie 请求 (内部调用)")

	// 调用 repository 获取设置项
	cookie, err := s.repo.GetSystemSetting(ctx, systemBilibiliCookieKey)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Info("系统 Bilibili Cookie 未设置")
			return nil, status.Errorf(codes.NotFound, "系统 Bilibili Cookie 未设置")
		}
		s.logger.Error("获取系统 Bilibili Cookie 失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "获取 Cookie 失败: %v", err)
	}

	s.logger.Info("成功获取系统 Bilibili Cookie") // 不记录 cookie 内容
	return &userv1.GetSystemBilibiliCookieResponse{Cookie: cookie}, nil
}

package service

import (
	"context"
	"errors"
	"log"
	"time" // 导入 time 包

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	// !!! 替换模块路径 !!!
	userv1 "go-xlive/gen/go/user/v1"
	"go-xlive/internal/user/repository"
	db "go-xlive/internal/user/repository/db"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb" // 导入 Empty
)

type UserService struct {
	userv1.UnimplementedUserServiceServer
	repo   repository.Repository
	logger *zap.Logger
}

func NewUserService(logger *zap.Logger, repo repository.Repository) *UserService {
	if logger == nil {
		log.Fatal("UserService 需要 Logger")
	}
	if repo == nil {
		log.Fatal("UserService 需要 Repository")
	}
	return &UserService{
		logger: logger,
		repo:   repo,
	}
}

func (s *UserService) CreateUser(ctx context.Context, req *userv1.CreateUserRequest) (*userv1.CreateUserResponse, error) {
	s.logger.Info("收到 CreateUser 请求", zap.String("username", req.GetUsername()))
	if req.GetUsername() == "" {
		s.logger.Warn("CreateUser 失败：用户名不能为空")
		return nil, status.Error(codes.InvalidArgument, "用户名不能为空")
	}
	userID := uuid.NewString()
	params := db.CreateUserParams{
		UserID:   userID,
		Username: req.GetUsername(),
	}
	createdDBUser, err := s.repo.CreateUser(ctx, params)
	if err != nil {
		// TODO: Check for unique constraint violation
		s.logger.Error("在数据库中创建用户失败", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "创建用户失败: %v", err)
	}
	protoUser := &userv1.User{
		UserId:   createdDBUser.UserID,
		Username: createdDBUser.Username,
	}
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
	protoUser := &userv1.User{
		UserId:   dbUser.UserID,
		Username: dbUser.Username,
	}
	s.logger.Info("用户检索成功", zap.String("user_id", protoUser.UserId))
	return &userv1.GetUserResponse{User: protoUser}, nil
}

// HealthCheck 实现
func (s *UserService) HealthCheck(ctx context.Context, req *emptypb.Empty) (*userv1.HealthCheckResponse, error) {
	s.logger.Debug("执行健康检查 (UserService)")
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second) // 设置检查超时
	defer cancel()
	if err := s.repo.Ping(checkCtx); err != nil { // 调用 Repo 的 Ping
		s.logger.Error("数据库健康检查失败", zap.Error(err))
		return &userv1.HealthCheckResponse{Status: "NOT_SERVING"}, nil // 返回 NOT_SERVING，但不返回具体错误给外部
	}
	return &userv1.HealthCheckResponse{Status: "SERVING"}, nil
}

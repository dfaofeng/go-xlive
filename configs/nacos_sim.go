package configs

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go-xlive/pkg/nacosclient" // <--- 导入新的包

	"github.com/spf13/viper"
	// "google.golang.org/grpc" // 如果使用 gRPC
	// "google.golang.org/grpc/credentials/insecure" // 用于 gRPC 示例
	// "google.golang.org/grpc/resolver" // 用于 gRPC Nacos 解析器
)

// --- Viper 配置加载 (需要更新 Config 结构体) ---
type NConfig struct {
	NacosClient nacosclient.ComponentConfig `mapstructure:",squash"` // <--- 嵌入组件配置
	// 添加其他应用层配置（如果它们不由 Nacos 管理）
	LogLevel string `mapstructure:"log_level"`
}

func loadConfig() (*NConfig, error) {
	vip := viper.New()
	vip.SetConfigName("config")
	vip.SetConfigType("yaml")
	vip.AddConfigPath("./configs")
	vip.AddConfigPath(".")
	vip.AutomaticEnv()
	vip.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	if err := vip.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Println("未找到配置文件 'configs/config.yaml'。")
		} else {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	var cfg NConfig
	if err := vip.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	return &cfg, nil
}

// --- 配置加载结束 ---

// --- (可选) gRPC Nacos 解析器集成 ---
//var grpcNacosResolverBuilder *nacosclient.NacosResolverBuilder // 定义全局变量

func main() {
	log.Println("启动 my-service...")

	// 1. 加载总配置 (包含 NacosClient 所需的配置)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. 创建 NacosClient 实例
	// 注意：现在 NewNacosClient 只创建实例，不执行网络操作
	client, err := nacosclient.NewNacosClient(cfg.NacosClient) // <--- 使用新的构造函数
	if err != nil {
		log.Fatalf("创建 Nacos 客户端失败: %v", err)
	}

	// 3. 加载初始配置并开始监听变更
	// 这是一个阻塞操作（直到初始加载完成），并启动后台监听 goroutine
	if err := client.LoadAndWatchAppConfig(); err != nil {
		// 根据错误类型决定是否继续启动
		log.Printf("警告: 加载或监听应用配置失败: %v。服务可能无法正常工作。", err)
		// os.Exit(1) // 或者选择退出
	}

	// --- (可选) 初始化 gRPC Nacos 解析器 ---
	//grpcNacosResolverBuilder = nacosclient.NewNacosResolverBuilder(client)
	// resolver.Register(grpcNacosResolverBuilder)
	// log.Println("信息: gRPC Nacos 解析器已注册。")
	// --- gRPC 解析器结束 ---

	// 4. 注册服务实例 (在加载配置之后，启动服务之前)
	if err := client.Register(); err != nil {
		log.Fatalf("向 Nacos 注册服务失败: %v", err)
	}

	// 5. 设置优雅停机
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 6. 启动你的微服务核心逻辑
	//    现在可以安全地访问配置了
	appCfg := client.GetAppConfig() // 获取配置指针

	// --- 示例：使用配置启动 HTTP 服务器 ---
	appCfg.RLock()                              // 读取配置时加读锁
	servicePort := cfg.NacosClient.Service.Port // 从原始配置获取本服务端口
	redisAddr := appCfg.Redis.Address
	pgDSN := appCfg.Postgres.DSN
	userServiceName := appCfg.GRPC.UserServiceName // 获取目标 gRPC 服务名
	appCfg.RUnlock()                               // 释放读锁

	// 打印示例配置
	log.Printf("信息: 读取到的 Redis 地址: %s", redisAddr)
	log.Printf("信息: 读取到的 PostgreSQL DSN (部分): %s...", pgDSN[:min(len(pgDSN), 30)]) // 注意脱敏
	log.Printf("信息: 目标用户服务名 (用于 gRPC 发现): %s", userServiceName)

	// 配置数据库连接池等...
	// setupDatabasePool(pgDSN, appCfg.Postgres.MaxOpenConns, appCfg.Postgres.MaxIdleConns)
	// setupRedisClient(redisAddr, appCfg.Redis.Password, appCfg.Redis.DB)

	// 启动 HTTP 服务器
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	})
	mux.HandleFunc("/config-test", func(w http.ResponseWriter, r *http.Request) {
		// 读取最新配置并返回
		currentAppCfg := client.GetAppConfig()
		currentAppCfg.RLock()
		defer currentAppCfg.RUnlock()
		fmt.Fprintf(w, "Current Redis Addr: %s\n", currentAppCfg.Redis.Address)
		fmt.Fprintf(w, "Feature Flag: %v\n", currentAppCfg.SomeFeatureFlag)
	})
	// 示例：调用用户服务 (需要 gRPC 和 Nacos Resolver)
	/*
		mux.HandleFunc("/call-user", func(w http.ResponseWriter, r *http.Request) {
			appCfg := client.GetAppConfig()
			appCfg.RLock()
			targetUserService := appCfg.GRPC.UserServiceName
			targetGroup := cfg.NacosClient.Nacos.GroupName // 使用默认 Group
			appCfg.RUnlock()

			if targetUserService == "" {
				http.Error(w, "User service name not configured", http.StatusInternalServerError)
				return
			}

			// 使用 Nacos Resolver 构建 target string
			// 格式: nacos://{groupName}@{serviceName}
			grpcTarget := fmt.Sprintf("nacos://%s@%s", targetGroup, targetUserService)
			log.Printf("Dialing gRPC target: %s", grpcTarget)

			// 创建 gRPC 连接 (需要 grpc 库)
			// 注意: "withInsecure" 在生产中不推荐，应使用 TLS
			// 注意: 需要设置负载均衡策略 "round_robin" 或其他
			conn, err := grpc.DialContext(r.Context(), grpcTarget,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`), // 启用负载均衡
				// grpc.WithResolvers(grpcNacosResolverBuilder), // 确保解析器已注册
			)
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to dial user service: %v", err), http.StatusInternalServerError)
				return
			}
			defer conn.Close()

			// 创建 gRPC 客户端并发起调用...
			// userClient := userpb.NewUserServiceClient(conn)
			// resp, err := userClient.GetUser(r.Context(), &userpb.GetUserRequest{UserId: "123"})
			// ... 处理响应和错误 ...

			fmt.Fprintf(w, "Successfully dialed user service (implementation pending).\n")
		})
	*/

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", servicePort),
		Handler: mux,
	}

	go func() {
		log.Printf("服务正在监听端口 %d", servicePort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务器 ListenAndServe 错误: %v", err)
		}
	}()

	// 7. 等待关闭信号
	<-ctx.Done()

	// 8. 执行优雅停机
	log.Println("收到关闭信号。开始优雅停机...")
	stop()

	// 先注销服务
	client.Close() // Close 会调用 Deregister

	// 再关闭 HTTP 服务器
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 服务器优雅关闭出错: %v", err)
	} else {
		log.Println("HTTP 服务器已优雅关闭。")
	}

	log.Println("清理工作完成。程序退出。")
}

// min 是一个简单的辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

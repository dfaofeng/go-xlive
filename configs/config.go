package configs

import (
	"log"
	"strings"

	"github.com/spf13/viper"
)

// --- 使用 Viper 加载配置的示例 ---

// Config 定义了整个应用的配置结构
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Client   ClientConfig   `mapstructure:"client"`
	Database DatabaseConfig `mapstructure:"database"`
	Redis    RedisConfig    `mapstructure:"redis"`
	Nats     NatsConfig     `mapstructure:"nats"`
	Adapter  AdapterConfig  `mapstructure:"adapter"` // <-- 新增 Adapter 配置
	Otel     OtelConfig     `mapstructure:"otel"`    // <-- 新增: OTel 配置
}

// ServerConfig 定义了各服务的端口
type ServerConfig struct {
	User        ServicePortConfig `mapstructure:"user"`
	Room        ServicePortConfig `mapstructure:"room"`
	Session     ServicePortConfig `mapstructure:"session"`
	Event       ServicePortConfig `mapstructure:"event"`       // 新增
	Aggregation ServicePortConfig `mapstructure:"aggregation"` // 新增
	Realtime    ServicePortConfig `mapstructure:"realtime"`    // 新增
	Gateway     ServicePortConfig `mapstructure:"gateway"`
}

// ServicePortConfig 定义服务的端口
type ServicePortConfig struct {
	GrpcPort int `mapstructure:"grpc_port"`
	HttpPort int `mapstructure:"http_port"` // 主要用于网关和指标端口
}

// ClientConfig 定义客户端连接信息
type ClientConfig struct {
	UserService        ServiceAddresses `mapstructure:"user_service"`
	RoomService        ServiceAddresses `mapstructure:"room_service"`
	SessionService     ServiceAddresses `mapstructure:"session_service"`
	EventService       ServiceAddresses `mapstructure:"event_service"`       // 新增
	AggregationService ServiceAddresses `mapstructure:"aggregation_service"` // 新增
	RealtimeService    ServiceAddresses `mapstructure:"realtime_service"`    // 新增
}

// ServiceAddresses 包含一个服务的多个地址
type ServiceAddresses struct {
	Addresses []string `mapstructure:"addresses"`
}

// DatabaseConfig 定义数据库连接信息
type DatabaseConfig struct {
	PostgresDSN string `mapstructure:"postgres_dsn"`
}

// RedisConfig 定义 Redis 连接信息
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// NatsConfig 定义 NATS 连接信息
type NatsConfig struct {
	URL string `mapstructure:"url"`
}

// --- 新增: Adapter 配置 ---
type AdapterConfig struct {
	Bilibili BilibiliAdapterConfig `mapstructure:"bilibili"`
}

type BilibiliAdapterConfig struct {
	ListenRoomIDs []int `mapstructure:"listen_room_ids"` // 要监听的 Bilibili 房间 ID 列表
}

// --- Adapter 配置结束 ---

// --- 新增: OtelConfig 定义 OpenTelemetry 配置 ---
type OtelConfig struct {
	OtlpEndpoint string `mapstructure:"otlp_endpoint"` // OTLP Exporter 的 gRPC endpoint 地址
}

// --- OtelConfig 配置结束 ---

// LoadConfig 从文件和环境变量加载配置
func LoadConfig(path string) (config Config, err error) {
	viper.AddConfigPath(path)
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 设置默认值
	viper.SetDefault("database.postgres_dsn", "postgres://user:password@localhost:5432/liveroomdb?sslmode=disable")
	viper.SetDefault("redis.addr", "localhost:6379")
	viper.SetDefault("nats.url", "nats://localhost:4222")
	viper.SetDefault("server.user.grpc_port", 50051)
	viper.SetDefault("server.room.grpc_port", 50052)
	viper.SetDefault("server.session.grpc_port", 50053)
	viper.SetDefault("server.event.grpc_port", 50054)
	viper.SetDefault("server.aggregation.grpc_port", 50055)
	viper.SetDefault("server.realtime.grpc_port", 50056) // 新增
	viper.SetDefault("server.gateway.http_port", 8080)
	viper.SetDefault("client.user_service.addresses", []string{"localhost:50051"})
	viper.SetDefault("client.room_service.addresses", []string{"localhost:50052"})
	viper.SetDefault("client.session_service.addresses", []string{"localhost:50053"})
	viper.SetDefault("client.event_service.addresses", []string{"localhost:50054"})
	viper.SetDefault("client.aggregation_service.addresses", []string{"localhost:50055"})
	viper.SetDefault("client.realtime_service.addresses", []string{"localhost:50056"}) // 新增
	viper.SetDefault("adapter.bilibili.listen_room_ids", []int{})                      // <-- 新增默认值 (空列表)
	viper.SetDefault("otel.otlp_endpoint", "localhost:4317")                           // <-- 新增: OTel OTLP Endpoint 默认值

	err = viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// 如果不是文件未找到错误，则报告错误
			log.Printf("读取配置文件出错: %s\n", err)
			return
		}
		log.Println("未找到配置文件，将依赖环境变量和默认值。")
	}

	err = viper.Unmarshal(&config) // 解析到结构体
	if err != nil {
		log.Printf("无法将配置解码到结构体中: %v\n", err)
	}
	return
}

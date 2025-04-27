package nacosclient

import (
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"sync"
)

// NacosConfig 保存连接 Nacos 服务器所需的配置 (与之前类似)
type NacosConfig struct {
	ServerAddrs []string `mapstructure:"server_addrs" yaml:"server_addrs"`
	NamespaceID string   `mapstructure:"namespace_id" yaml:"namespace_id"`
	GroupName   string   `mapstructure:"group_name" yaml:"group_name"` // 服务的默认 Group
	Username    string   `mapstructure:"username" yaml:"username"`
	Password    string   `mapstructure:"password" yaml:"password"`
	TimeoutMs   uint64   `mapstructure:"timeout_ms" yaml:"timeout_ms"`
	LogLevel    string   `mapstructure:"log_level" yaml:"log_level"`
	LogDir      string   `mapstructure:"log_dir" yaml:"log_dir"`
	CacheDir    string   `mapstructure:"cache_dir" yaml:"cache_dir"`
}

// ServiceInfo 保存当前要注册的服务实例的信息 (与之前类似)
type ServiceInfo struct {
	Name        string            `mapstructure:"name" yaml:"name"`
	Port        uint64            `mapstructure:"port" yaml:"port"`
	IP          string            `mapstructure:"ip" yaml:"ip"`
	Weight      float64           `mapstructure:"weight" yaml:"weight"`
	Metadata    map[string]string `mapstructure:"metadata" yaml:"metadata"`
	ClusterName string            `mapstructure:"cluster_name" yaml:"cluster_name"`
	Ephemeral   bool              `mapstructure:"ephemeral" yaml:"ephemeral"`
}

// AppConfig 定义了从 Nacos 配置中心获取的应用业务配置结构。
// !!! 这个结构需要根据你的实际配置内容来定义 !!!
type AppConfig struct {
	sync.RWMutex // 用于保护配置的并发读写

	GRPC struct {
		// 示例：存储目标服务的 Nacos 服务名，用于后续发现
		UserServiceName    string `yaml:"user_service_name"`
		RoomServiceName    string `yaml:"order_service_name"`
		ProductServiceName string `yaml:"product_service_name"`
		// 或直接存储服务地址（如果地址固定或通过其他方式管理）
		// SomeFixedServiceAddr string `yaml:"some_fixed_service_addr"`
	} `yaml:"grpc"`

	Redis struct {
		Address  string `yaml:"address"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:"redis"`

	Postgres struct {
		DSN          string `yaml:"dsn"` // 例如: "host=localhost user=user password=pass dbname=mydb port=5432 sslmode=disable"
		MaxOpenConns int    `yaml:"max_open_conns"`
		MaxIdleConns int    `yaml:"max_idle_conns"`
	} `yaml:"postgres"`

	// 可以添加其他配置项...
	SomeFeatureFlag bool `yaml:"some_feature_flag"`
}

// ComponentConfig 是传递给 NewClient 函数的总配置。
type ComponentConfig struct {
	Nacos           NacosConfig `mapstructure:"nacos"`
	Service         ServiceInfo `mapstructure:"service"`            // 用于服务注册的信息
	AppConfigDataID string      `mapstructure:"app_config_data_id"` // 应用主配置的 Data ID
	AppConfigGroup  string      `mapstructure:"app_config_group"`   // 应用主配置的 Group
}

// InstanceInfo (保持不变)
type InstanceInfo struct {
	ID          string
	ServiceName string
	IP          string
	Port        uint64
	Weight      float64
	Healthy     bool
	Enabled     bool
	Ephemeral   bool
	Metadata    map[string]string
	ClusterName string
}

// --- 默认值设置辅助函数 (保持不变) ---
func (cfg *NacosConfig) setDefaults() {
	if cfg.GroupName == "" {
		cfg.GroupName = constant.DEFAULT_GROUP
	}
	if cfg.TimeoutMs == 0 {
		cfg.TimeoutMs = 5000
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "/tmp/nacos/log"
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = "/tmp/nacos/cache"
	}
}

func (si *ServiceInfo) setDefaults() {
	if si.Weight == 0 {
		si.Weight = 10.0
	}
	si.Ephemeral = true // 强制默认为 true
	if si.ClusterName == "" {
		si.ClusterName = constant.DEFAULT_GROUP
	}
}

func (cc *ComponentConfig) setDefaults() {
	if cc.AppConfigGroup == "" {
		cc.AppConfigGroup = constant.DEFAULT_GROUP // 应用配置也使用默认 Group
	}
	cc.Nacos.setDefaults()
	cc.Service.setDefaults()
}

package configs

import (
	"fmt"
	"github.com/spf13/viper"
	"go-xlive/pkg/nacosclient"
	"log"
	"strings"
)

// NacConfig --- Viper 配置加载 (需要更新 Config 结构体) ---
type NacConfig struct {
	NacosClient nacosclient.ComponentConfig `mapstructure:",squash"` // <--- 嵌入组件配置
	// 添加其他应用层配置（如果它们不由 Nacos 管理）
	LogLevel string `mapstructure:"log_level"`
}

func NacosloadConfig(path string) (*NacConfig, error) {
	vip := viper.New()
	vip.SetConfigName("nacos")                           // 配置文件名 (不带扩展名)
	vip.SetConfigType("yaml")                            // 如果配置文件名没有扩展名，则需要指定类型
	vip.AddConfigPath(path)                              // 查找配置文件的路径
	vip.AddConfigPath(".")                               // 可选：也在工作目录中查找
	vip.AutomaticEnv()                                   // 读取匹配的环境变量
	vip.SetEnvKeyReplacer(strings.NewReplacer(".", "_")) // 将配置键中的 '.' 替换为 '_' 以匹配环境变量

	// 读取配置文件
	if err := vip.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// 配置文件未找到；如果允许，可以忽略错误并依赖默认值/环境变量
			log.Println("未找到配置文件 'configs/config.yaml'，将依赖默认值/环境变量。")
		} else {
			// 找到配置文件但发生了其他错误
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	var cfg NacConfig
	// 将配置 unmarshal 到结构体
	if err := vip.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	return &cfg, nil
}

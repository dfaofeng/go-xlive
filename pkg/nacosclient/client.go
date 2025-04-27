package nacosclient

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client" // 引入配置客户端
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client" // 引入命名客户端
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
	"gopkg.in/yaml.v3" // 引入 YAML 解析库
)

// NacosClient 定义了统一的 Nacos 客户端接口
type NacosClient interface {
	// GetAppConfig --- 配置中心方法 ---
	// GetAppConfig 返回当前加载的应用配置的只读指针。
	// 注意：直接修改返回的指针是不安全的，应通过 Nacos 更新。
	// 如需在读取配置期间进行复杂操作，请使用 AppConfig 内的 RWMutex。
	GetAppConfig() *AppConfig
	// LoadAndWatchAppConfig 加载初始配置并开始监听变更。通常在初始化时调用。
	LoadAndWatchAppConfig() error

	// Register --- 服务注册与发现方法 ---
	Register() error
	Deregister() error
	DiscoverInstances(serviceName, groupName string, healthyOnly bool) ([]InstanceInfo, error)
	DiscoverOneInstance(serviceName, groupName string) (*InstanceInfo, error)
	Subscribe(serviceName, groupName string, callback func(instances []InstanceInfo, err error)) error
	Unsubscribe(serviceName, groupName string) error

	// Close 关闭客户端，执行清理操作（例如注销服务）。
	Close()
}

// nacosClient 实现了 NacosClient 接口
type nacosClient struct {
	configClient  config_client.IConfigClient // 配置客户端
	namingClient  naming_client.INamingClient // 命名客户端
	compConfig    ComponentConfig             // 组件的总配置
	appConfig     *AppConfig                  // 应用的业务配置实例 (需要保护)
	registerParam vo.RegisterInstanceParam    // 注册参数缓存
	isRegistered  bool
	mu            sync.Mutex // 保护 isRegistered
}

// NewNacosClient 创建并初始化一个新的 Nacos 客户端组件。
// 注意：此函数只创建客户端，需要额外调用 LoadAndWatchAppConfig 和 Register 来完成初始化。
func NewNacosClient(compCfg ComponentConfig) (NacosClient, error) {
	// --- 验证和设置默认值 ---
	if compCfg.AppConfigDataID == "" {
		return nil, errors.New("必须提供应用配置的 Data ID (app_config_data_id)")
	}
	compCfg.setDefaults() // 设置 Nacos, Service, AppConfigGroup 的默认值

	// 验证 ServiceInfo (端口和名称)
	if compCfg.Service.Name == "" {
		return nil, errors.New("必须提供服务名称 (service.name)")
	}
	if compCfg.Service.Port == 0 || compCfg.Service.Port > 65535 {
		return nil, fmt.Errorf("无效的服务端口号 (service.port): %d", compCfg.Service.Port)
	}
	// 自动检测 IP (如果需要)
	if compCfg.Service.IP == "" {
		ip, err := getOutboundIP()
		if err != nil {
			log.Printf("警告: 自动检测出站 IP 失败: %v。将回退到 127.0.0.1", err)
			compCfg.Service.IP = "127.0.0.1"
		} else {
			compCfg.Service.IP = ip
			log.Printf("信息: 自动检测到服务 IP: %s", compCfg.Service.IP)
		}
	}

	// --- Nacos 客户端设置 ---
	serverConfigs := make([]constant.ServerConfig, 0, len(compCfg.Nacos.ServerAddrs))
	for _, addr := range compCfg.Nacos.ServerAddrs {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
			portStr = "8848"
			log.Printf("警告: 无法从 Nacos 地址 '%s' 解析 host/port，假设端口为 8848: %v", addr, err)
		}
		port, err := strconv.ParseUint(portStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Nacos 服务器地址 '%s' 中的端口无效: %w", addr, err)
		}
		serverConfigs = append(serverConfigs, constant.ServerConfig{
			IpAddr: host,
			Port:   port,
		})
	}

	clientConfig := constant.ClientConfig{
		NamespaceId:         compCfg.Nacos.NamespaceID,
		TimeoutMs:           compCfg.Nacos.TimeoutMs,
		NotLoadCacheAtStart: true,
		LogDir:              compCfg.Nacos.LogDir,
		CacheDir:            compCfg.Nacos.CacheDir,
		LogLevel:            compCfg.Nacos.LogLevel,
		Username:            compCfg.Nacos.Username,
		Password:            compCfg.Nacos.Password,
	}

	nacosParam := vo.NacosClientParam{
		ClientConfig:  &clientConfig,
		ServerConfigs: serverConfigs,
	}

	// 创建配置客户端
	cfgClient, err := clients.NewConfigClient(nacosParam)
	if err != nil {
		return nil, fmt.Errorf("创建 Nacos 配置客户端失败: %w", err)
	}
	log.Println("信息: Nacos 配置客户端已创建。")

	// 创建命名客户端
	namClient, err := clients.NewNamingClient(nacosParam)
	if err != nil {
		// 如果创建命名客户端失败，最好也关闭配置客户端（如果可能）
		// cfgClient.Close() // SDK v2 ConfigClient 也没有 Close 方法
		return nil, fmt.Errorf("创建 Nacos 命名客户端失败: %w", err)
	}
	log.Println("信息: Nacos 命名客户端已创建。")
	// 准备注册参数 (但不在此处注册)
	registerParam := vo.RegisterInstanceParam{
		Ip:          compCfg.Service.IP,
		Port:        compCfg.Service.Port,
		ServiceName: compCfg.Service.Name,
		GroupName:   compCfg.Nacos.GroupName, // 使用 Nacos 配置中的默认 Group
		Weight:      compCfg.Service.Weight,
		ClusterName: compCfg.Service.ClusterName,
		Metadata:    compCfg.Service.Metadata,
		Enable:      true,
		Healthy:     true,
		Ephemeral:   compCfg.Service.Ephemeral,
	}

	// 初始化 AppConfig 结构体
	appCfg := &AppConfig{} // 初始化为空结构体，稍后加载

	nc := &nacosClient{
		configClient:  cfgClient,
		namingClient:  namClient,
		compConfig:    compCfg,
		appConfig:     appCfg,
		registerParam: registerParam,
		isRegistered:  false,
	}

	return nc, nil
}

// --- 配置中心方法实现 ---

func (nc *nacosClient) GetAppConfig() *AppConfig {
	return nc.appConfig // 直接返回指针，调用方负责通过 RWMutex 保护访问
}

func (nc *nacosClient) LoadAndWatchAppConfig() error {
	dataId := nc.compConfig.AppConfigDataID
	group := nc.compConfig.AppConfigGroup

	// 1. 加载初始配置
	log.Printf("信息: 正在从 Nacos 加载初始应用配置 (DataID: %s, Group: %s)...", dataId, group)
	content, err := nc.configClient.GetConfig(vo.ConfigParam{
		DataId: dataId,
		Group:  group,
	})
	if err != nil {
		// 如果是配置不存在错误，可以接受，使用默认空配置启动
		if strings.Contains(err.Error(), "config data not exist") || strings.Contains(err.Error(), "data not exist") {
			log.Printf("警告: 应用配置 (DataID: %s, Group: %s) 在 Nacos 中未找到，将使用空配置启动。", dataId, group)
			// nc.appConfig 已经是空结构体，无需操作
		} else {
			return fmt.Errorf("加载初始应用配置失败: %w", err)
		}
	} else {
		// 解析初始配置
		if err := nc.parseAndUpdateAppConfig(content); err != nil {
			log.Printf("警告: 解析初始应用配置失败 (DataID: %s, Group: %s)，将使用空配置启动: %v", dataId, group, err)
			// 出错则保持 appConfig 为空
		} else {
			log.Printf("信息: 初始应用配置加载并解析成功。")
		}
	}

	// 2. 监听配置变更
	log.Printf("信息: 开始监听应用配置变更 (DataID: %s, Group: %s)...", dataId, group)
	err = nc.configClient.ListenConfig(vo.ConfigParam{
		DataId: dataId,
		Group:  group,
		OnChange: func(namespace, group, dataId, data string) {
			log.Printf("信息: 监听到应用配置变更 (Namespace: %s, Group: %s, DataID: %s)", namespace, group, dataId)
			if err := nc.parseAndUpdateAppConfig(data); err != nil {
				log.Printf("错误: 处理应用配置变更失败: %v", err)
			} else {
				log.Printf("信息: 应用配置已成功更新。")
				// 这里可以添加通知机制，告知应用的其他部分配置已更新
				// (例如通过 channel 或回调函数)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("监听应用配置失败: %w", err)
	}

	return nil
}

// parseAndUpdateAppConfig 解析配置内容并更新内部 AppConfig (线程安全)
func (nc *nacosClient) parseAndUpdateAppConfig(content string) error {
	var tempConfig AppConfig // 解析到临时变量

	// 注意：这里直接 Unmarshal 到新变量，如果 Nacos 配置中缺少某些字段，
	// tempConfig 中对应的字段会是零值。如果希望保留旧值，需要先拷贝。
	// 但通常配置应该是完整的。
	err := yaml.Unmarshal([]byte(content), &tempConfig)
	if err != nil {
		return fmt.Errorf("YAML 解析配置失败: %w", err)
	}

	// 获取写锁并更新全局配置
	nc.appConfig.Lock()
	defer nc.appConfig.Unlock()
	// 简单替换指针指向的内容 (如果 AppConfig 很大，考虑字段级更新或原子替换指针)
	*(nc.appConfig) = tempConfig // 结构体赋值
	// 重置锁状态，因为结构体赋值会复制锁的状态，可能导致死锁
	nc.appConfig.RWMutex = sync.RWMutex{}

	log.Printf("调试: 更新后的应用配置内部状态: %+v", nc.appConfig) // Debug log
	return nil
}

// --- 服务注册发现方法实现 (与之前 nacosregistry 类似) ---

func (nc *nacosClient) Register() error {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if nc.isRegistered {
		return nil
	}

	log.Printf("信息: 正在注册服务实例 %s:%d (服务名: %s, 分组: %s)...",
		nc.registerParam.Ip, nc.registerParam.Port, nc.registerParam.ServiceName, nc.registerParam.GroupName)

	success, err := nc.namingClient.RegisterInstance(nc.registerParam)
	if err != nil {
		return fmt.Errorf("Nacos 注册实例失败: %w", err)
	}
	if !success {
		return errors.New("Nacos 注册调用返回 false")
	}

	nc.isRegistered = true
	log.Printf("信息: 服务实例注册成功。")
	return nil
}

func (nc *nacosClient) Deregister() error {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if !nc.isRegistered {
		return nil
	}

	log.Printf("信息: 正在注销服务实例 %s:%d (服务名: %s, 分组: %s)...",
		nc.registerParam.Ip, nc.registerParam.Port, nc.registerParam.ServiceName, nc.registerParam.GroupName)

	deregisterParam := vo.DeregisterInstanceParam{
		Ip:          nc.registerParam.Ip,
		Port:        nc.registerParam.Port,
		ServiceName: nc.registerParam.ServiceName,
		GroupName:   nc.registerParam.GroupName,
		Ephemeral:   nc.registerParam.Ephemeral,
		Cluster:     nc.registerParam.ClusterName,
	}

	success, err := nc.namingClient.DeregisterInstance(deregisterParam)
	if err != nil {
		// 记录错误，但不一定阻止程序关闭
		log.Printf("错误: 从 Nacos 注销实例失败: %v", err)
		// return fmt.Errorf("从 Nacos 注销实例失败: %w", err)
	} else if !success {
		log.Printf("警告: Nacos 注销调用返回 false 但没有错误。")
	} else {
		log.Printf("信息: 服务实例注销成功。")
	}

	nc.isRegistered = false
	return nil // 允许关闭继续
}

func (nc *nacosClient) DiscoverInstances(serviceName, groupName string, healthyOnly bool) ([]InstanceInfo, error) {
	if groupName == "" {
		groupName = nc.compConfig.Nacos.GroupName
	}
	instances, err := nc.namingClient.SelectInstances(vo.SelectInstancesParam{
		ServiceName: serviceName, GroupName: groupName, HealthyOnly: healthyOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("发现服务 '%s'@'%s' 实例失败: %w", serviceName, groupName, err)
	}
	result := make([]InstanceInfo, 0, len(instances))
	for _, inst := range instances {
		result = append(result, mapNacosInstance(inst))
	}
	return result, nil
}

func (nc *nacosClient) DiscoverOneInstance(serviceName, groupName string) (*InstanceInfo, error) {
	if groupName == "" {
		groupName = nc.compConfig.Nacos.GroupName
	}
	instance, err := nc.namingClient.SelectOneHealthyInstance(vo.SelectOneHealthInstanceParam{
		ServiceName: serviceName, GroupName: groupName,
	})
	if err != nil {
		errMsgLower := strings.ToLower(err.Error())
		if strings.Contains(errMsgLower, "instance list is empty") || strings.Contains(errMsgLower, "no healthy instance") {
			return nil, nil
		}
		return nil, fmt.Errorf("发现服务 '%s'@'%s' 单个健康实例失败: %w", serviceName, groupName, err)
	}
	if instance == nil {
		return nil, nil
	}
	result := mapNacosInstance(*instance)
	return &result, nil
}

func (nc *nacosClient) Subscribe(serviceName, groupName string, callback func(instances []InstanceInfo, err error)) error {
	if groupName == "" {
		groupName = nc.compConfig.Nacos.GroupName
	}
	wrappedCallback := func(services []model.Instance, err error) {
		if err != nil {
			callback(nil, fmt.Errorf("Nacos 订阅回调错误: %w", err))
			return
		}
		result := make([]InstanceInfo, 0, len(services))
		for _, inst := range services {
			result = append(result, mapNacosInstance(inst))
		}
		callback(result, nil)
	}
	err := nc.namingClient.Subscribe(&vo.SubscribeParam{
		ServiceName: serviceName, GroupName: groupName, SubscribeCallback: wrappedCallback,
	})
	if err != nil {
		return fmt.Errorf("订阅服务 '%s'@'%s' 失败: %w", serviceName, groupName, err)
	}
	log.Printf("信息: 已订阅服务实例变更 '%s' (分组: %s)", serviceName, groupName)
	return nil
}

func (nc *nacosClient) Unsubscribe(serviceName, groupName string) error {
	if groupName == "" {
		groupName = nc.compConfig.Nacos.GroupName
	}
	err := nc.namingClient.Unsubscribe(&vo.SubscribeParam{
		ServiceName: serviceName, GroupName: groupName,
		SubscribeCallback: func(services []model.Instance, err error) {},
	})
	if err != nil {
		return fmt.Errorf("取消订阅服务 '%s'@'%s' 失败: %w", serviceName, groupName, err)
	}
	log.Printf("信息: 已取消订阅服务实例变更 '%s' (分组: %s)", serviceName, groupName)
	return nil
}

func (nc *nacosClient) Close() {
	log.Println("信息: 正在关闭 Nacos 客户端...")
	// 优先尝试注销服务
	nc.Deregister()
	// SDK v2 没有提供显式的 Close 方法给客户端实例
	log.Println("信息: Nacos 客户端清理完成 (隐式连接关闭)。")
}

// mapNacosInstance (保持不变)
func mapNacosInstance(ni model.Instance) InstanceInfo {
	return InstanceInfo{
		ID: ni.InstanceId, ServiceName: ni.ServiceName, IP: ni.Ip, Port: ni.Port,
		Weight: ni.Weight, Healthy: ni.Healthy, Enabled: ni.Enable, Ephemeral: ni.Ephemeral,
		Metadata: ni.Metadata, ClusterName: ni.ClusterName,
	}
}

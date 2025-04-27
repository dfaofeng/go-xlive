// pkg/nacosclient/grpc_resolver.go (或者 pkg/nacosresolver/resolver.go)
package nacosclient

import (
	"context"
	"fmt"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"google.golang.org/grpc/resolver"
	"log"
	"strings"
	"sync"
)

const (
	// NacosScheme 定义了 gRPC 连接 Nacos 服务的 Scheme
	NacosScheme = "nacos"
)

// NacosResolverBuilder 实现了 gRPC 的 resolver.Builder 接口
type NacosResolverBuilder struct {
	client NacosClient // 引用我们之前封装的 NacosClient 接口
}

// NewNacosResolverBuilder 创建一个新的 NacosResolverBuilder
// !!! 这就是你需要调用的函数，它不是 SDK 自带的 !!!
func NewNacosResolverBuilder(client NacosClient) *NacosResolverBuilder {
	return &NacosResolverBuilder{
		client: client,
	}
}

// Build 当 gRPC Dial 一个 Scheme 为 NacosScheme 的 target 时被调用
func (b *NacosResolverBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	log.Printf("gRPC Resolver: Build target: %+v", target)

	// 1. 解析 target 获取服务名和分组名
	// 推荐格式: nacos://{groupName}@{serviceName}
	// 或者 nacos://{serviceName} (使用默认分组)
	var serviceName, groupName string
	if target.URL.User != nil { // 包含 @，则 @ 前面是 groupName
		groupName = target.URL.User.Username()
		serviceName = target.URL.Host
	} else {
		// 如果 Endpoint 不含 @，则将其视为 serviceName，使用默认 group
		serviceName = target.URL.Path // 或者 target.Endpoint() 取决于你的 target 格式
		if strings.HasPrefix(serviceName, "/") {
			serviceName = serviceName[1:] // 移除前导斜杠
		}
		appCfg := b.client.GetAppConfig() // 假设能访问 AppConfig 来获取默认 group
		if appCfg != nil {
			appCfg.RLock()
			// 这里假设 ComponentConfig 中存储了 Nacos 的默认 Group
			// groupName = b.client.compConfig.Nacos.GroupName // 假设可以访问内部字段
			// 如果不能访问内部字段，需要在 NacosClient 接口加方法或 Builder 创建时传入
			// 暂时硬编码或从已知配置获取
			// TODO: 改进获取默认 Group 的方式
			compConfig := b.client.(*nacosClient).compConfig // 不推荐，破坏封装，最好加接口
			groupName = compConfig.Nacos.GroupName
			appCfg.RUnlock()
		}
		if groupName == "" {
			groupName = "DEFAULT_GROUP" // 回退到硬编码默认值
			log.Printf("gRPC Resolver: 未指定 GroupName 且无法获取默认值，使用 'DEFAULT_GROUP' for service '%s'", serviceName)
		}

	}

	if serviceName == "" {
		return nil, fmt.Errorf("gRPC Resolver: target '%s' 未能解析出有效的 serviceName", target.URL.String())
	}

	log.Printf("gRPC Resolver: Parsed ServiceName=%s, GroupName=%s", serviceName, groupName)

	// 2. 创建实际的 Resolver
	r := &nacosResolver{
		client:        b.client,
		cc:            cc,
		serviceName:   serviceName,
		groupName:     groupName,
		ctx:           context.Background(),      // 使用后台 Context
		cancel:        func() {},                 // 初始化为空函数
		disableSvcCfg: opts.DisableServiceConfig, // 是否禁用服务配置
	}
	r.ctx, r.cancel = context.WithCancel(context.Background()) // 创建可取消的 Context

	// 3. 启动后台 watcher
	go r.watch()

	return r, nil
}

// Scheme 返回此 Builder 支持的 Scheme
func (b *NacosResolverBuilder) Scheme() string {
	return NacosScheme
}

// nacosResolver 实现了 gRPC 的 resolver.Resolver 接口
type nacosResolver struct {
	client        NacosClient
	cc            resolver.ClientConn
	serviceName   string
	groupName     string
	ctx           context.Context
	cancel        context.CancelFunc
	watchCh       chan []model.Instance // 用于 Nacos 回调传递数据 (可选，也可以直接在回调处理)
	mu            sync.Mutex
	disableSvcCfg bool // 是否禁用服务配置
}

// watch 启动对 Nacos 服务的监听
func (r *nacosResolver) watch() {
	log.Printf("gRPC Resolver: Starting watch for %s@%s", r.serviceName, r.groupName)

	// Nacos 订阅回调函数
	callback := func(instances []InstanceInfo, err error) {
		if err != nil {
			log.Printf("错误: Nacos 订阅回调出错 for %s@%s: %v", r.serviceName, r.groupName, err)
			// 可以考虑向 gRPC 报告错误状态，但这比较复杂，通常做法是保持旧地址列表
			// r.cc.ReportError(err)
			return
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		select {
		case <-r.ctx.Done(): // 检查 resolver 是否已关闭
			log.Printf("gRPC Resolver: Watcher stopped for %s@%s because context is done.", r.serviceName, r.groupName)
			return
		default:
		}

		log.Printf("gRPC Resolver: Received update for %s@%s, %d instances", r.serviceName, r.groupName, len(instances))

		// 将 Nacos 实例列表转换为 gRPC 地址列表
		newAddrs := make([]resolver.Address, 0, len(instances))
		for _, inst := range instances {
			// 只使用健康的实例 (Nacos Subscribe 通常只返回健康的，但最好确认下)
			// DiscoverInstances 方法可以控制是否只获取健康实例
			if inst.Healthy && inst.Enabled {
				addr := resolver.Address{
					Addr: fmt.Sprintf("%s:%d", inst.IP, inst.Port),
					// 可以将 Nacos 元数据、权重等信息放入 Attributes
					//Attributes: attributes.New(),
				}
				newAddrs = append(newAddrs, addr)
			} else {
				log.Printf("gRPC Resolver: Skipping unhealthy/disabled instance %s:%d for %s@%s", inst.IP, inst.Port, r.serviceName, r.groupName)
			}
		}

		if len(newAddrs) == 0 {
			log.Printf("警告: 服务 %s@%s 没有找到健康的实例，gRPC 连接可能会失败。", r.serviceName, r.groupName)
			// 更新为空列表，让 gRPC 知道没有可用地址
		}

		// 更新 gRPC ClientConn 的状态
		state := resolver.State{Addresses: newAddrs}
		// 如果需要，可以在这里解析和设置服务配置 (Service Config)
		// if !r.disableSvcCfg {
		//     // 从实例元数据或其他来源获取服务配置 JSON 字符串
		//     scJSON := parseServiceConfig(instances)
		//     if scJSON != "" {
		//        state.ServiceConfig = r.cc.ParseServiceConfig(scJSON)
		//        if state.ServiceConfig.Err != nil {
		//            log.Printf("WARN: Failed to parse service config for %s: %v", r.serviceName, state.ServiceConfig.Err)
		//        }
		//     }
		// }
		err = r.cc.UpdateState(state)
		if err != nil {
			log.Printf("错误: gRPC ClientConn UpdateState 失败 for %s@%s: %v", r.serviceName, r.groupName, err)
		} else {
			// log.Printf("gRPC Resolver: Successfully updated state for %s@%s with %d addresses", r.serviceName, r.groupName, len(newAddrs))
		}
	}

	// 启动 Nacos 订阅
	err := r.client.Subscribe(r.serviceName, r.groupName, callback)
	if err != nil {
		log.Printf("错误: Nacos Subscribe 失败 for %s@%s: %v", r.serviceName, r.groupName, err)
		// 向 gRPC 报告错误，可能导致 Dial 失败
		r.cc.ReportError(fmt.Errorf("nacos subscribe failed: %w", err))
		return
	}

	// 阻塞直到 context 被取消 (Resolver 被关闭)
	<-r.ctx.Done()
	log.Printf("gRPC Resolver: Watch loop exiting for %s@%s.", r.serviceName, r.groupName)

	// 在退出前取消订阅
	err = r.client.Unsubscribe(r.serviceName, r.groupName)
	if err != nil {
		log.Printf("错误: Nacos Unsubscribe 失败 for %s@%s: %v", r.serviceName, r.groupName, err)
	} else {
		log.Printf("信息: Nacos Unsubscribe 成功 for %s@%s.", r.serviceName, r.groupName)
	}
}

// ResolveNow gRPC 要求实现，但对于基于推送的模型（如 Nacos Subscribe），通常为空
func (r *nacosResolver) ResolveNow(options resolver.ResolveNowOptions) {
	log.Printf("gRPC Resolver: ResolveNow called for %s@%s (usually no-op for push-based resolvers)", r.serviceName, r.groupName)
	// 可以选择性地触发一次主动查询，但不推荐频繁调用
	// go r.queryInstances() // 如果需要主动查询逻辑
}

// Close 关闭 Resolver，取消订阅
func (r *nacosResolver) Close() {
	log.Printf("gRPC Resolver: Closing resolver for %s@%s", r.serviceName, r.groupName)
	r.cancel() // 这会停止 watch goroutine 中的阻塞，并触发 Unsubscribe
}

# 系统架构图

\`\`\`mermaid
graph LR
    subgraph "数据源"
        A["外部平台 如 Bilibili"];
    end

    subgraph "适配层 (Adapter)"
        B["Adapter 服务"];
        B_desc("职责: 从外部平台接收原始事件，转换为内部统一事件格式，例如 BilibiliAdapter");
    end

    subgraph "消息队列 (NATS)"
        C["NATS 主题: events.raw.*"];
        C_desc("职责: 原始事件消息队列，解耦适配层和事件处理层");
    end

    subgraph "事件服务 (Event Service)"
        D["Event Service"];
        D_desc("职责: 接收原始事件，存储到数据库 (events 表)，更新 Redis 实时统计");
        E["PostgreSQL (events 表)"];
        F["Redis (live:stats:{session_id} 哈希)"];
    end

    subgraph "聚合服务 (Aggregation Service)"
        G["Aggregation Service"];
        G_desc("职责: 聚合计算各种指标，如礼物，UV等，并调用 Session Service 更新聚合结果");
        H["触发器 (如 关播事件/手动)"];
        I["PostgreSQL (events 表)"];
        J["Redis (live:stats:{session_id} 哈希)"];
    end

    subgraph "场次服务 (Session Service)"
        K["Session Service"];
        K_desc("职责: 场次管理，存储场次信息 (sessions 表)，管理场次缓存，提供场次信息查询接口");
        L["PostgreSQL (sessions 表)"];
        M["Redis (cache:session:{session_id} 缓存)"];
    end

    subgraph "网关服务 (Gateway Service)"
        N["Gateway Service"];
        N_desc("职责: API 网关，对外提供 gRPC 和 WebSocket 接口，路由到内部服务");
    end

    subgraph "用户服务 (User Service)"
        O["User Service"];
        O_desc("职责: 用户管理服务 (users 表)");
        P["PostgreSQL (users 表)"];
    end

    subgraph "实时服务 (Realtime Service)"
        Q["Realtime Service"];
        Q_desc("职责: 实时消息推送服务，**通过 SubscriptionHub 管理客户端连接，根据 Session ID 将消息推送到对应会话的客户端**");
    end

    subgraph "房间服务 (Room Service)"
        R["Room Service"];
        R_desc("职责: 房间管理服务 (rooms 表)");
        S["PostgreSQL (rooms 表)"];
    end

    subgraph "数据存储"
        DB["(PostgreSQL)"];
        Cache["(Redis)"];
    end
    %% 调用链
    style A fill:#ccf,stroke:#333,stroke-width:2px
    style N fill:#ccf,stroke:#333,stroke-width:2px
    style O fill:#ccf,stroke:#333,stroke-width:2px
    style Q fill:#ccf,stroke:#333,stroke-width:2px
    style R fill:#ccf,stroke:#333,stroke-width:2px

    A -- "原始事件" --> B;
    B -- "发布原始事件" --> C;
    C -- "订阅原始事件" --> D;
    D -- "存储所有原始事件" --> E;
    D -- "更新实时计数 (弹幕/点赞/观看)" --> F;

    H -- "触发聚合" --> G;
    G -- "查询所有事件 (计算礼物/UV等)" --> I;
    G -- "查询最终实时计数" --> J;
    G -- "调用 UpdateSessionAggregates" --> K;

    K -- "更新聚合数据 (包括点赞/观看)" --> L;
    K -- "更新/写入 Session 缓存" --> M;
    K -- "读取 Session 缓存" --> M;
    K -- "(缓存未命中) 读取 Session" --> L;

    N -- "调用 Session Service" --> K;
    N -- "调用 User Service" --> O;
    N -- "调用 Realtime Service (传递 Session ID)" --> Q;
    N -- "调用 Room Service" --> R;

    %% 存储关联
    E --> DB;
    F --> Cache;
    I --> DB;
    J --> Cache;
    L --> DB;
    M --> Cache;
    P --> DB;
    S --> DB;

    %% 样式
    classDef service fill:#f9f,stroke:#333,stroke-width:2px;
    classDef db fill:#lightgrey,stroke:#333,stroke-width:2px;
    classDef cache fill:#lightblue,stroke:#333,stroke-width:2px;
    classDef mq fill:#orange,stroke:#333,stroke-width:2px;
    classDef desc fill:#eee,stroke:#333,stroke-width:1px,stroke-dasharray: 5 5;

    class B,D,G,K,N,O,Q,R service;
    class E,I,L,P,S db;
    class F,J,M cache;
    class C mq;
    class DB db;
    class Cache cache;
    class B_desc,D_desc,G_desc,K_desc,C_desc,N_desc,O_desc,Q_desc,R_desc desc;
\`\`\`

**服务职责和调用链说明 (更新后):**

1.  **数据源**: 外部直播平台，例如 Bilibili 等。
2.  **适配层 (Adapter)**:
    *   **Adapter 服务**:
        *   **职责**:  **系统与外部直播平台对接的桥梁。** 从外部平台接收原始直播事件，例如弹幕、点赞、礼物、用户进入/退出直播间等。针对不同的平台可能有不同的适配器实现，例如 `BilibiliAdapter`。**核心职责是将外部平台的事件格式转换为系统内部统一的事件格式 (protobuf)**，并发布到消息队列 `events.raw.*`。
        *   **调用链**:  接收外部平台事件 -> **(事件解析和转换)** -> 发布到 NATS `events.raw.*`.
3.  **消息队列 (NATS)**:
    *   **NATS 主题: `events.raw.*`**:
        *   **职责**:  **构建事件驱动架构的核心组件。** 作为原始事件的中心消息管道，**实现适配层和事件处理层 (Event Service) 的解耦**。 确保事件的可靠传递，为后续的事件处理和消费提供保障。
        *   **调用链**:  接收 Adapter 服务发布的原始事件 ->  被 Event Service 订阅和消费。
4.  **事件服务 (Event Service)**:
    *   **Event Service**:
        *   **职责**:  **原始事件的存储和初步处理中心。** 订阅 NATS `events.raw.*` 主题的原始事件。**对原始事件进行初步处理和分类**。将所有原始事件**按照原始格式**存储到 PostgreSQL 的 `events` 表中，用于后续的数据分析、回溯和聚合。同时，针对特定类型的实时事件（如弹幕、点赞、观看、用户进入/退出等），**快速更新 Redis 中存储的实时统计数据**，用于实时看板展示和业务告警。
        *   **调用链**:  订阅 NATS `events.raw.*` -> 存储原始事件到 PostgreSQL (events 表) -> 更新 Redis 实时统计 (live:stats:{session_id} 哈希)。
5.  **聚合服务 (Aggregation Service)**:
    *   **Aggregation Service**:
        *   **职责**:  **核心数据聚合和计算服务。** 负责对直播场次数据进行**深度聚合计算**。例如，在直播结束后或手动触发时，从 PostgreSQL 的 `events` 表中**查询指定场次的所有原始事件**，**进行复杂的业务指标计算**，例如总礼物价值、UV、付费用户数、用户观看时长分布等聚合指标。同时，从 Redis 中读取最终的实时统计数据（弹幕总数、点赞总数、总观看人数等）。将**最终的聚合结果**调用 Session Service 的 `UpdateSessionAggregates` gRPC 接口进行存储，**更新场次的聚合数据**。
        *   **调用链**:  接收聚合触发信号 (例如：**关播事件，定时任务，人工触发** ) -> 查询 PostgreSQL (events 表) 原始事件 -> 查询 Redis 实时统计 (live:stats:{session_id} 哈希) ->  **[gRPC 调用]** Session Service `UpdateSessionAggregates` **(更新场次聚合数据)**.
6.  **场次服务 (Session Service)**:
    *   **Session Service**:
        *   **职责**:  **直播场次 (Session) 管理中心**，**是业务核心服务之一**。 管理直播场次 (Session) 的生命周期和状态。**处理场次的创建 (CreateSession), 查询 (GetSession, ListSessions), 更新 (UpdateSession, EndSession, DeleteSession) 等核心操作**。 存储场次的基本信息和聚合数据到 PostgreSQL 的 `sessions` 表中。**维护场次状态机**。 管理 Redis 中的场次缓存 (`cache:session:{session_id}`)，**提升场次信息查询效率**。 提供 gRPC 接口，**供 API 网关 (Gateway Service) 和 聚合服务 (Aggregation Service) 调用**，用于场次管理和数据更新。**依赖 Room Service 和 Event Service**。
        *   **调用链**:
            *   **[gRPC 调用]** 接收 Aggregation Service 的聚合结果 -> 更新 PostgreSQL (sessions 表) -> 更新/写入 Redis 场次缓存 (cache:session:{session_id})。
            *   **[gRPC 调用]** API 网关 (Gateway Service)  调用 Session Service 的 gRPC 接口 **(CreateSession, GetSession, UpdateSession 等)** 查询和管理场次信息。
7.  **网关服务 (Gateway Service)**:
    *   **Gateway Service**:
        *   **职责**:  **系统流量入口**，**面向外部客户端的 API 网关**。 作为系统的 API 网关，对外提供统一的访问入口。**处理客户端的 gRPC 和 WebSocket 连接**。 将外部请求路由到内部的各个服务，例如 Session Service, User Service, Realtime Service, Room Service。**实现请求鉴权、流量控制、监控等网关通用功能**。
        *   **调用链**:  接收外部 API 请求 (gRPC/WebSocket) -> **(鉴权, 路由, 负载均衡)** -> 路由到内部服务 (Session Service, User Service, Realtime Service, Room Service)。
8.  **用户服务 (User Service)**:
    *   **User Service**:
        *   **职责**:  **用户账户和信息管理服务**。 负责用户管理，例如用户注册、登录、用户信息查询、更新等。用户信息存储在 PostgreSQL 的 `users` 表中。**提供用户相关的 gRPC 接口，供 API 网关 (Gateway Service) 调用**。
        *   **调用链**:  **[gRPC 调用]** 被 Gateway Service 调用，处理用户相关的请求 **(GetUser, CreateUser, UpdateUser 等)**。
9.  **实时服务 (Realtime Service)**:
    *   **Realtime Service**:
        *   **职责**:  **构建实时互动体验的核心服务**，**负责直播间实时消息的推送**，例如直播间弹幕、礼物消息、用户进场消息等。**Realtime Service 通过 SubscriptionHub 高效管理 WebSocket 客户端连接**，**维护 Session ID 到客户端连接的映射关系**，根据 Session ID 将消息推送到对应会话的客户端。  **依赖 Session Service 获取场次信息**。
        *   **数据来源**:
            *   **NATS 消息队列**:  `Realtime Service` 订阅 NATS `events.raw.*` 主题，接收来自 `Event Service` 和 `Aggregation Service` 的事件数据 (**例如：弹幕事件, 礼物事件, 用户状态变更事件**)。
            *   **Gateway Service gRPC 流**:  `Realtime Service` 接收来自 `Gateway Service` 的 **双向 gRPC 流**，用于和 WebSocket 客户端建立长连接，进行实时消息的推送和接收。
        *   **调用链**:
            *   **[双向 gRPC 流]** 接收 Gateway Service 的实时消息推送请求 (gRPC 流): Gateway Service 调用 Realtime Service，建立 **双向 gRPC 流**，用于 WebSocket 客户端的实时消息推送。请求中会包含 **Session ID**。 Realtime Service 根据 **Session ID** 和 `SubscriptionHub` 将消息推送给对应的客户端 (通过 gRPC 流推送到 Gateway Service, 再由 Gateway Service 推送到 WebSocket 客户端)。
            *   **[NATS 订阅]** 订阅 NATS `events.raw.*` 主题:  接收 NATS `events.raw.*` 主题的事件，并根据事件中的 Session ID，通过 `SubscriptionHub` 广播给客户端 (通过 gRPC 流推送到 Gateway Service, 再由 Gateway Service 推送到 WebSocket 客户端)。
10. **房间服务 (Room Service)**:
    *   **Room Service**:
        *   **职责**:  **直播房间管理服务**。 负责房间管理，例如创建房间、查询房间信息、更新房间信息、删除房间等。房间信息存储在 PostgreSQL 的 `rooms` 表中。**提供房间管理的 gRPC 接口，供 API 网关 (Gateway Service) 和 Session Service 等服务调用**。
        *   **调用链**:  **[gRPC 调用]** 被 Gateway Service 调用，处理房间相关的请求 **(GetRoom, CreateRoom, UpdateRoom 等)**。 **[gRPC 调用]** 被 Session Service 调用，例如在创建 Session 时，Session Service 需要调用 Room Service 检查房间状态。
11. **数据存储**:
    *   **PostgreSQL**:  **主要的数据持久化存储**。 关系型数据库，用于持久化存储结构化数据，例如事件原始数据、场次信息、用户信息、房间信息等。
    *   **Redis**:  **高性能缓存**。 内存数据库，用于缓存热点数据，例如实时统计数据、场次信息缓存等，**提升数据访问速度，降低数据库压力**。
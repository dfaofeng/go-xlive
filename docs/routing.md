# 服务路由与调用图

```mermaid
graph TD
    subgraph "外部客户端"
        Client_HTTP["HTTP/REST Client"];
        Client_WS["WebSocket Client"];
    end

    subgraph "API 网关 (Gateway Service)"
        GW("Gateway");
        GW_Router["Router"];
        GW_gRPC_Mux["gRPC-Gateway Mux"];
        GW_WS_Handler["WebSocket Handler"];
    end

    subgraph "后端服务"
        UserService("User Service");
        RoomService("Room Service");
        SessionService("Session Service");
        EventService("Event Service");
        AggregationService("Aggregation Service");
        RealtimeService("Realtime Service");
        AdapterService("Adapter Service");
    end

    subgraph "消息队列"
        NATS["NATS"];
    end

    subgraph "数据存储"
        Postgres["(PostgreSQL)"];
        Redis["(Redis)"];
    end

    %% 网关路由
    Client_HTTP -- "/v1/*" --> GW_Router;
    Client_WS -- "/ws/sessions/*" --> GW_Router;
    GW_Router -- "/v1/*" --> GW_gRPC_Mux;
    GW_Router -- "/ws/sessions/*" --> GW_WS_Handler;

    GW_gRPC_Mux -- "/v1/users/*" --> UserService;
    GW_gRPC_Mux -- "/v1/rooms/*" --> RoomService;
    GW_gRPC_Mux -- "/v1/sessions/*" --> SessionService;
    GW_gRPC_Mux -- "/v1/events/*" --> EventService;
    GW_gRPC_Mux -- "/v1/aggregation/*" --> AggregationService;
    GW_gRPC_Mux -- "/v1/realtime/healthz" --> RealtimeService;

    GW_WS_Handler -- "gRPC StreamEvents" --> RealtimeService;

    %% 服务间调用
    SessionService -- "gRPC GetRoom" --> RoomService;
    AggregationService -- "gRPC UpdateSessionAggregates" --> SessionService;
    RealtimeService -- "gRPC SignalUserPresence" --> SessionService;

    %% NATS 事件流
    AdapterService -- "events.raw.*" --> NATS;
    EventService -- "events.raw.* (订阅)" --> NATS; %% Clarified link text
    SessionService -- "events.raw.stream.status" --> NATS;
    SessionService -- "events.raw.chat.message" --> NATS;
    SessionService -- "events.raw.user.presence" --> NATS;

    NATS -- "events.raw.*" --> EventService;
    NATS -- "events.raw.user.presence" --> RealtimeService;
    NATS -- "events.stored.* (可选)" --> RealtimeService; %% Clarified link text
    NATS -- "events.raw.stream.status" --> AggregationService; %% Clarified link text

    %% 数据库/缓存访问
    UserService --> Postgres;
    RoomService --> Postgres;
    SessionService --> Postgres;
    SessionService --> Redis;
    EventService --> Postgres;
    EventService --> Redis;
    AggregationService --> Postgres;
    AggregationService --> Redis;
    RealtimeService --> Redis; %% 可能用于维护连接状态

    %% 样式
    classDef service fill:#f9f,stroke:#333,stroke-width:2px;
    classDef gateway fill:#ccf,stroke:#333,stroke-width:2px;
    classDef mq fill:#orange,stroke:#333,stroke-width:2px;
    classDef db fill:#lightgrey,stroke:#333,stroke-width:2px;
    classDef cache fill:#lightblue,stroke:#333,stroke-width:2px;

    class GW,GW_Router,GW_gRPC_Mux,GW_WS_Handler gateway;
    class UserService,RoomService,SessionService,EventService,AggregationService,RealtimeService,AdapterService service;
    class NATS mq;
    class Postgres db;
    class Redis cache;

```

**说明:**

*   **实线箭头**: 表示直接的网络调用（HTTP, WebSocket, gRPC）。
*   **虚线箭头**: 表示通过消息队列 (NATS) 的异步事件流。
*   **方框/圆角方框**: 代表服务、组件或外部实体。
*   **颜色**: 用于区分不同类型的组件。

此图展示了请求如何通过 API 网关路由到后端服务，以及服务之间如何通过 gRPC 和 NATS 进行通信。
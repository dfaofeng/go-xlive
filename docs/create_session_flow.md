# 创建房间场次 (Session) HTTP 请求流程

**注意:** 下方的流程图使用 Mermaid 语法编写。你需要使用支持 Mermaid 的 Markdown 查看器（例如 VS Code 内置预览或带有 Mermaid 插件的浏览器扩展）才能正确渲染为图形。

```mermaid
sequenceDiagram
    participant Client as HTTP Client
    participant Gateway as API Gateway
    participant SessionSvc as Session Service (gRPC)
    participant RoomSvc as Room Service (gRPC)
    participant DB as Database (Postgres)
    participant NATS as NATS Broker
    participant Redis as Redis Cache

    Note over Client, Gateway: 1. Client initiates request
    Client->>+Gateway: POST /v1/sessions <br> Body: { "room_id": "internal_room_123" }
    Note over Gateway: 2. Gateway receives HTTP, converts to gRPC
    Gateway->>+SessionSvc: gRPC CreateSession(req={ room_id: "internal_room_123" })

    Note over SessionSvc, RoomSvc: 3. Session Svc validates Room & gets Owner ID
    SessionSvc->>+RoomSvc: gRPC GetRoom(req={ room_id: "internal_room_123" })

    alt Room Exists & Active
        RoomSvc-->>-SessionSvc: gRPC GetRoomResponse <br> (room={..., owner_user_id: "user_abc", status: "active"}, owner={...})
        Note over SessionSvc: 4. Generate Session ID & Prepare Data
        SessionSvc->>SessionSvc: Generate session_id (e.g., "session_xyz")

        Note over SessionSvc, DB: 5. Store Session in Database
        SessionSvc->>+DB: INSERT INTO sessions <br> (session_id="session_xyz", room_id="internal_room_123", owner_user_id="user_abc", status="live", ...)
        DB-->>-SessionSvc: Success (Return created session row)

        Note over SessionSvc, NATS: 6. Publish Stream Start Event
        SessionSvc->>NATS: PUBLISH events.raw.stream.status <br> (msg={session_id:"session_xyz", room_id:"internal_room_123", is_live:true, ...})

        Note over SessionSvc, Redis: 7. Cache the new Session
        SessionSvc->>+Redis: SET cache:session:session_xyz <br> (value=serialized session data)
        Redis-->>-SessionSvc: OK

        Note over SessionSvc, Gateway: 8. Session Svc responds to Gateway
        SessionSvc-->>-Gateway: gRPC CreateSessionResponse <br> (session={session_id:"session_xyz", room_id:"internal_room_123", owner_user_id:"user_abc", status:"live", ...})

        Note over Gateway, Client: 9. Gateway converts gRPC to HTTP and responds
        Gateway-->>-Client: HTTP 200 OK <br> Body: { "session": { ... } }
    else Room Not Found or Not Active
        RoomSvc-->>SessionSvc: gRPC Error (e.g., NotFound)
        Note over SessionSvc, Gateway: 8a. Session Svc responds with error
        SessionSvc-->>Gateway: gRPC Error (e.g., InvalidArgument or FailedPrecondition)
        Note over Gateway, Client: 9a. Gateway converts gRPC error to HTTP error
        Gateway-->>Client: HTTP Error (e.g., 400 Bad Request or 412 Precondition Failed)
    end

```

## 流程解释

1.  **客户端发起 HTTP 请求**:
    *   客户端向 API Gateway 发送 `POST /v1/sessions` 请求。
    *   请求体包含内部房间 ID: `{ "room_id": "your_internal_room_id" }`。
    *   这个 `room_id` 需要客户端预先获取（例如，通过浏览列表或搜索）。

2.  **API Gateway 处理请求**:
    *   Gateway 接收 HTTP 请求。
    *   使用 `grpc-gateway` 将 HTTP 请求转换为 gRPC `CreateSessionRequest`。
    *   将 gRPC 请求代理给 Session Service。

3.  **Session Service 执行逻辑**:
    *   接收 gRPC 请求。
    *   **验证房间**: 调用 Room Service 的 `GetRoom` 方法，传入 `room_id`，获取房间状态和 `owner_user_id`。如果房间无效或状态不允许，返回错误。
    *   **生成 Session ID**: 创建一个新的 UUID 作为 `session_id`。
    *   **数据库操作**: 将新 Session 信息（包括 `session_id`, `room_id`, `owner_user_id`, `status='live'` 等）存入数据库。
    *   **发布 NATS 事件**: 发布 `events.raw.stream.status` 事件，通知直播开始。
    *   **更新 Redis 缓存**: 将新 Session 数据缓存到 Redis。
    *   **返回 gRPC 响应**: 将包含完整 Session 信息的 `CreateSessionResponse` 返回给 Gateway。

4.  **API Gateway 返回响应**:
    *   接收 gRPC 响应。
    *   将 gRPC 响应转换为 JSON。
    *   向客户端发送 HTTP `200 OK` 响应，响应体为包含新 Session 信息的 JSON 对象。

5.  **客户端接收 HTTP 响应**: 客户端收到成功响应和新创建的 Session 数据。
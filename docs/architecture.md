# Sider 架构文档（完整版）

本文档描述 Sider 服务发现系统的整体架构、数据模型、核心机制、代码组织与演进路线。当前版本已完成 M2 阶段，引入了 Raft 共识协议和持久化能力，实现高可用服务发现。

---

## 目录

1. [背景与目标](#1-背景与目标)
2. [总体架构](#2-总体架构)
3. [核心组件详解](#3-核心组件详解)
4. [数据模型](#4-数据模型)
5. [一致性与索引模型](#5-一致性与索引模型)
6. [Raft 集群架构](#6-raft-集群架构)
7. [健康检查机制](#7-健康检查机制)
8. [Watch 与长轮询](#8-watch-与长轮询)
9. [HTTP API 设计](#9-http-api-设计)
10. [代码组织结构](#10-代码组织结构)
11. [数据流与交互](#11-数据流与交互)
12. [并发与锁策略](#12-并发与锁策略)
13. [部署拓扑](#13-部署拓扑)
14. [可观测性](#14-可观测性)
15. [安全机制](#15-安全机制)
16. [性能与可扩展性](#16-性能与可扩展性)
17. [测试策略](#17-测试策略)
18. [路线图](#18-路线图)

---

## 1. 背景与目标

### 1.1 设计目标

Sider 是一个受 Consul 启发的轻量级服务发现系统，旨在为微服务架构提供：

- **服务注册与发现**：服务实例动态注册、健康检查、实时查询
- **高可用性**：基于 Raft 的多节点强一致性，容忍节点故障
- **实时变更通知**：支持长轮询，客户端可订阅服务变更
- **健康检查**：TTL、HTTP、TCP、命令多种检查方式
- **多租户隔离**：基于 Namespace 的逻辑隔离

### 1.2 适用场景

- 微服务集群内部寻址
- 灰度发布与多版本管理
- 客户端负载均衡
- 服务健康状态监控

### 1.3 里程碑划分

- **M1（已完成）**：内存注册表、HTTP API、Agent、TTL 健康检查、长轮询
- **M2（当前）**：Raft 共识、持久化、Leader 选举、集群管理、TTL 过期分布式化
- **M3（规划）**：DNS 接口、ACL/mTLS、CLI/UI、Prometheus 指标
- **M4（规划）**：跨机房复制、事件聚合优化、备份恢复

---

## 2. 总体架构

### 2.1 架构概览

```
┌─────────────────────────────────────────────────────────────────┐
│                         Client Applications                      │
│                    (Service Consumers & Providers)               │
└──────────────────────┬──────────────────────────────────────────┘
                       │
                       │ HTTP API
                       ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Sider Cluster (3-5 nodes)                   │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │   Node 1     │  │   Node 2     │  │   Node 3     │          │
│  │  (Leader)    │  │  (Follower)  │  │  (Follower)  │          │
│  ├──────────────┤  ├──────────────┤  ├──────────────┤          │
│  │ HTTP Server  │  │ HTTP Server  │  │ HTTP Server  │          │
│  ├──────────────┤  ├──────────────┤  ├──────────────┤          │
│  │RaftRegistry  │  │RaftRegistry  │  │RaftRegistry  │          │
│  ├──────────────┤  ├──────────────┤  ├──────────────┤          │
│  │   Raft FSM   │◄─┼────Raft──────┼─►│   Raft FSM   │          │
│  ├──────────────┤  │  Protocol     │  ├──────────────┤          │
│  │MemoryRegistry│  │               │  │MemoryRegistry│          │
│  ├──────────────┤  ├──────────────┤  ├──────────────┤          │
│  │ BoltDB + WAL │  │ BoltDB + WAL │  │ BoltDB + WAL │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
                       ▲
                       │ Register & Heartbeat
                       │
┌─────────────────────────────────────────────────────────────────┐
│                        Sider Agents                              │
│  (Deployed on each service host, managing local services)       │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 核心组件

- **sds-server**：服务端进程，承载 HTTP API 和 Raft 节点
- **sds-agent**：轻量级 Agent，部署在服务主机上，负责注册和健康检查
- **RaftRegistry**：Raft 封装层，协调写入复制和读取
- **MemoryRegistry**：内存状态机，维护服务注册表、检查状态、索引
- **Raft FSM**：Raft 状态机实现，将日志命令映射到内存操作
- **HTTP Server**：提供 RESTful API，支持长轮询

---

## 3. 核心组件详解

### 3.1 sds-server

**职责**：
- 接收客户端注册/查询请求
- 作为 Raft 集群的一个节点参与共识
- 维护服务注册表的内存快照
- 提供 HTTP API 和集群管理接口

**启动参数**：
```bash
sds-server \
  -http :8500 \              # HTTP API 监听地址
  -raft-id node1 \           # Raft 节点唯一 ID
  -raft-bind :8501 \         # Raft 通信地址
  -raft-dir data/node1 \     # 数据目录（WAL + 快照）
  -raft-bootstrap true       # 是否作为引导节点
```

**内部模块**：
1. **HTTPServer**：处理客户端 HTTP 请求
2. **RaftRegistry**：写操作提交到 Raft，读操作直接访问内存
3. **raftFSM**：实现 Raft FSM 接口，将日志应用到内存状态
4. **memoryRegistry**：内存数据结构，维护服务实例、检查、索引
5. **Cluster Manager**：处理节点加入、配置变更

### 3.2 sds-agent

**职责**：
- 注册本地服务实例到 Sider 集群
- 执行健康检查（TTL、HTTP、TCP、命令）
- 定期续约（TTL）或上报检查结果
- 优雅退出时自动注销

**配置示例**（examples/agent.demo.json）：
```json
{
  "services": [
    {
      "name": "api-gateway",
      "id": "api-gateway-1",
      "address": "192.168.1.10",
      "port": 8080,
      "checks": [
        {
          "type": "ttl",
          "ttl": "15s"
        },
        {
          "type": "http",
          "path": "/health",
          "interval": "10s",
          "timeout": "3s"
        }
      ]
    }
  ]
}
```

### 3.3 RaftRegistry

**职责**：统一写入和读取的接口层

**代码位置**：`internal/registry/raftreg.go`

**核心方法**：
```go
// 写操作 - 通过 Raft 复制
RegisterInstance(inst, specs) -> (index, checkIDs, error)
DeregisterInstance(ns, svc, id) -> (index, error)
RenewTTL(checkID) -> (index, error)
ReportCheck(checkID, status, output) -> (index, error)

// 读操作 - 直接从内存读取
ListHealthyInstances(ns, svc, opts) -> (instances, index, error)
ListServices(ns) -> (services, index, error)
WatchService(ns, svc, lastIndex) -> (index, <-chan struct{})
```

**设计要点**：
- 写操作构建 Raft 命令并提交，等待应用后返回结果
- 读操作直接从内存状态机读取（强一致性，因为读的是 Leader 的最新状态）
- 统一的命令提交方法 `applyCommand()`，消除重复代码

### 3.4 raftFSM

**职责**：Raft 状态机实现，将日志命令转换为内存操作

**代码位置**：`internal/registry/raftfsm.go`

**实现的接口**：
```go
type FSM interface {
    Apply(log *raft.Log) interface{}         // 应用日志命令
    Snapshot() (FSMSnapshot, error)          // 创建快照
    Restore(io.ReadCloser) error             // 从快照恢复
}
```

**命令处理流程**：
1. 解析命令封装（commandEnvelope）
2. 根据操作类型分发到对应处理器
3. 调用 memoryRegistry 的方法执行操作
4. 返回编码后的响应

**支持的命令**：
- `register`：注册服务实例
- `deregister`：注销服务实例
- `renew_ttl`：续约 TTL 检查
- `report_check`：报告健康检查结果

### 3.5 memoryRegistry

**职责**：内存数据结构，维护所有服务状态

**代码位置**：`internal/registry/memory.go`

**核心数据结构**：
```go
type memoryRegistry struct {
    mu sync.RWMutex

    // 核心数据
    instances map[string]*instanceRecord  // ns/svc/id -> 实例
    checks    map[string]*checkRecord     // checkID -> 检查

    // 索引与查询
    idToKeys  map[string][]string         // id -> [keys...]
    svcIndex  map[string]uint64           // ns/svc -> 索引
    watchers  map[string][]chan struct{}  // ns/svc -> 通知通道

    // 全局状态
    index          uint64
    stopCh         chan struct{}
    expirerStarted bool
}
```

**设计特点**：
- 读写锁保护并发访问
- 全局索引和服务索引分离，支持精确的变更通知
- TTL 过期器仅在 Leader 上运行
- Watchers 采用边缘触发，避免 goroutine 泄漏

---

## 4. 数据模型

### 4.1 核心类型

#### ServiceInstance
```go
type ServiceInstance struct {
    Namespace   string            // 命名空间（租户隔离）
    Service     string            // 服务名
    ID          string            // 实例唯一标识
    Address     string            // IP/域名
    Port        int               // 端口
    Tags        []string          // 标签（用于过滤）
    Meta        map[string]string // 元数据（自定义键值）
    Weights     Weights           // 负载均衡权重

    // 内部字段
    CreateIndex uint64            // 创建索引
    ModifyIndex uint64            // 最后修改索引
}
```

#### CheckSpec
```go
type CheckSpec struct {
    Type     CheckType     // ttl | http | tcp | cmd
    TTL      time.Duration // TTL 检查的超时时间
    HTTP     string        // HTTP 检查的路径
    Interval time.Duration // 检查间隔
    Timeout  time.Duration // 检查超时
}
```

#### Check
```go
type Check struct {
    ID         string        // 检查 ID（自动生成）
    Spec       CheckSpec     // 检查配置
    Status     CheckStatus   // passing | warning | critical | unknown
    Output     string        // 检查输出
    LastUpdate time.Time     // 最后更新时间
    LastPass   time.Time     // 最后通过时间（TTL）
}
```

### 4.2 健康聚合规则

实例的健康状态由所有检查聚合而成，采用"最坏优先"策略：

```
无检查         → passing  （默认健康）
有 critical    → critical （任何检查失败则失败）
有 unknown     → unknown  （未知状态）
有 warning     → warning  （告警但可用）
全 passing     → passing  （完全健康）
```

---

## 5. 一致性与索引模型

### 5.1 Raft 共识保证

- **写入线性化**：所有写操作通过 Raft 日志复制，保证全局顺序
- **多数派确认**：写入需要超过半数节点确认（3 节点容忍 1 故障，5 节点容忍 2 故障）
- **自动选主**：Leader 失效后，Follower 自动发起选举（秒级恢复）

### 5.2 索引机制

#### 全局索引（index）
- 每次写操作自增
- 用于追踪全局变更顺序
- 客户端通过 `X-Index` 响应头获取

#### 服务索引（svcIndex）
- 每个服务维护独立索引
- 记录该服务最后一次变更的全局索引
- 用于精确的变更通知和长轮询

#### 索引推进时机
1. 注册实例
2. 注销实例
3. 续约 TTL（检查状态变更）
4. 报告检查结果
5. TTL 过期（仅 Leader）

### 5.3 读一致性

**当前实现（M2）**：
- 所有读操作访问 Leader 的内存状态
- 等价于线性化读（Linearizable Read）
- 客户端始终读取到最新已提交的状态

**未来增强（M3）**：
- 支持 `stale=true` 参数，允许 Follower 提供陈旧读
- 降低 Leader 负载，提高读吞吐量

---

## 6. Raft 集群架构

### 6.1 节点角色

- **Leader**：处理所有写请求，复制日志到 Follower
- **Follower**：接收日志复制，参与选举投票
- **Candidate**：选举期间的临时状态

### 6.2 数据持久化

每个节点维护三种存储：

1. **StableStore**（raft-stable.db）
   - 存储当前任期、投票记录
   - 使用 BoltDB

2. **LogStore**（raft-log.db）
   - 存储 Raft 日志条目（WAL）
   - 使用 BoltDB
   - 支持日志压缩

3. **SnapshotStore**（目录）
   - 存储状态机快照
   - JSON 格式全量序列化
   - 保留最近 2 个快照

### 6.3 快照策略

**触发条件**（满足任一）：
- 时间间隔：20 秒
- 日志条目数：8192

**快照内容**：
```json
{
  "instances": {...},  // 所有服务实例
  "checks": {...},     // 所有健康检查
  "id_to_keys": {...}, // ID 索引
  "svc_index": {...},  // 服务索引
  "index": 12345       // 全局索引
}
```

**快照恢复**：
- 节点重启后自动加载最新快照
- 恢复内存状态，清空 watchers
- 继续接收新的日志条目

### 6.4 集群管理

#### 引导集群（Bootstrap）
```bash
# 第一个节点
sds-server -raft-id node1 -raft-bind :8501 -raft-bootstrap=true
```

#### 加入集群（Join）
```bash
# 启动新节点
sds-server -raft-id node2 -raft-bind :9501 -raft-bootstrap=false

# 向 Leader 发送加入请求
curl -X POST http://leader:8500/v1/raft/join \
  -H 'Content-Type: application/json' \
  -d '{"ID":"node2","Addr":"127.0.0.1:9501"}'
```

#### 节点移除
```bash
# 通过 Raft API（未来实现）
curl -X POST http://leader:8500/v1/raft/remove \
  -d '{"ID":"node2"}'
```

---

## 7. 健康检查机制

### 7.1 TTL 检查

**工作原理**：
1. Agent 注册服务时声明 TTL（如 `15s`）
2. 初始状态为 `critical`（避免未就绪即对外可见）
3. Agent 每 `2/3 * TTL` 续约一次
4. Leader 每秒扫描，超时未续约的检查标记为 `critical`

**续约方式**：
```bash
PUT /v1/agent/check/pass/{check_id}
```

**优势**：
- 简单高效，客户端控制节奏
- 适合快速心跳场景

### 7.2 HTTP 检查

**配置示例**：
```json
{
  "type": "http",
  "path": "/health",
  "interval": "10s",
  "timeout": "3s"
}
```

**健康判定**：
- 2xx/3xx → `passing`
- 4xx → `warning`
- 其他/超时 → `critical`

### 7.3 TCP 检查

**配置示例**：
```json
{
  "type": "tcp",
  "interval": "10s",
  "timeout": "3s"
}
```

**健康判定**：
- 连接成功 → `passing`
- 连接失败 → `critical`

### 7.4 命令检查

**配置示例**：
```json
{
  "type": "cmd",
  "cmd": ["/usr/bin/check-app.sh"],
  "interval": "30s",
  "timeout": "10s"
}
```

**健康判定**：
- 退出码 0 → `passing`
- 退出码非 0 → `critical`
- 输出作为 `Output` 字段

---

## 8. Watch 与长轮询

### 8.1 设计目标

- 客户端能实时感知服务变更，无需轮询
- 支持超时控制，避免无限阻塞
- 高效的内存管理，避免 goroutine 泄漏

### 8.2 实现机制

#### WatchService
```go
func WatchService(ns, service string, lastIndex uint64) (uint64, <-chan struct{})
```

**行为**：
1. 如果 `svcIndex > lastIndex`，立即返回已关闭的通道（边缘触发）
2. 否则创建带缓冲的通道，加入 watchers 列表
3. 下次该服务变更时，统一关闭所有通道并清空列表

#### 长轮询使用
```bash
GET /v1/health/service/api?ns=default&index=100&wait=30s
```

**流程**：
1. 解析 `index` 和 `wait` 参数
2. 调用 `WatchService()` 注册监听
3. 阻塞等待通道或超时
4. 返回最新服务实例列表和当前索引

### 8.3 超时处理

- 客户端指定 `wait` 时长（如 `30s`）
- 服务端使用 `context.WithTimeout()` 包装
- 超时后返回当前状态（即使未变更）
- 客户端根据 `X-Index` 判断是否有变更

---

## 9. HTTP API 设计

### 9.1 API 列表

| 路径 | 方法 | 功能 | 写/读 |
|------|------|------|------|
| `/v1/agent/service/register` | PUT/POST | 注册服务实例 | 写 |
| `/v1/agent/service/deregister/{id}` | PUT/POST | 注销服务实例（路径式） | 写 |
| `/v1/agent/service/deregister` | PUT/POST | 注销服务实例（JSON） | 写 |
| `/v1/agent/check/pass/{check_id}` | PUT/POST | 标记检查通过/续约 TTL | 写 |
| `/v1/agent/check/warn/{check_id}` | PUT/POST | 标记检查告警 | 写 |
| `/v1/agent/check/fail/{check_id}` | PUT/POST | 标记检查失败 | 写 |
| `/v1/catalog/services` | GET | 列出所有服务 | 读 |
| `/v1/health/service/{name}` | GET | 查询健康实例（支持长轮询） | 读 |
| `/v1/raft/join` | POST | 加入 Raft 集群 | 管理 |

详细接口文档请参见 `docs/api.md`

---

## 10. 代码组织结构

### 10.1 目录树

```
sider/
├── cmd/
│   ├── sds-server/       # 服务端入口
│   │   └── main.go
│   └── sds-agent/        # Agent 入口
│       └── main.go
├── internal/
│   ├── api/              # HTTP API 层
│   │   ├── http.go       # 路由和处理器
│   │   └── types.go      # 请求/响应类型
│   ├── registry/         # 注册表核心
│   │   ├── types.go      # 数据模型
│   │   ├── registry.go   # Registry 接口
│   │   ├── memory.go     # 内存实现（442 行）
│   │   ├── raftcmd.go    # Raft 命令定义（181 行）
│   │   ├── raftreg.go    # RaftRegistry 封装（135 行）
│   │   └── raftfsm.go    # Raft FSM 实现（238 行）
│   ├── raft/             # Raft 抽象（已弃用）
│   │   └── node.go       # 本地桩实现
│   ├── server/           # 服务端装配
│   │   ├── server.go     # 主服务器
│   │   └── cluster.go    # Raft 集群管理
│   └── agent/            # Agent 实现
│       └── agent.go
├── docs/                 # 文档
│   ├── architecture.md   # 架构文档（本文件）
│   ├── api.md            # API 文档
│   ├── dev_guide.md      # 开发者指南
│   └── m2_design.md      # M2 设计文档
├── examples/
│   └── agent.demo.json   # Agent 配置示例
├── Makefile
├── go.mod
└── README.md
```

### 10.2 模块职责

#### internal/registry - 核心注册表

| 文件 | 行数 | 职责 |
|------|------|------|
| `types.go` | 89 | 数据模型定义 |
| `registry.go` | ~50 | Registry 接口 |
| `memory.go` | 442 | 内存存储、索引、TTL 过期 |
| `raftcmd.go` | 181 | 命令构建、解析、编解码 |
| `raftreg.go` | 135 | Raft 封装，写入复制、读取转发 |
| `raftfsm.go` | 238 | FSM 实现，命令处理、快照 |

**设计亮点**：
- 命令与响应类型统一管理（`raftcmd.go`）
- 消除重复代码，提取通用方法
- 清晰的职责分离，便于测试

#### internal/api - HTTP 接口层

| 文件 | 行数 | 职责 |
|------|------|------|
| `http.go` | 304 | 路由、处理器、长轮询 |
| `types.go` | ~100 | 请求/响应类型 |

**设计亮点**：
- 统一错误处理
- 请求日志中间件
- 优雅关闭支持

#### internal/server - 服务端装配

| 文件 | 行数 | 职责 |
|------|------|------|
| `server.go` | 68 | 组装组件、启动服务 |
| `cluster.go` | 71 | Raft 集群管理、节点加入 |

**设计亮点**：
- 监听 Leader 变更，动态启停 TTL 过期器
- 集群加入接口实现

---

## 11. 数据流与交互

### 11.1 写操作流程（注册实例）

```
┌──────────┐
│  Client  │
└────┬─────┘
     │ PUT /v1/agent/service/register (JSON)
     ▼
┌────────────────────┐
│   HTTP Server      │
│ handleRegister()   │
└────┬───────────────┘
     │ Reg.RegisterInstance()
     ▼
┌────────────────────┐
│  RaftRegistry      │
│ RegisterInstance() │
└────┬───────────────┘
     │ 1. BuildRegisterCommand()  # 构建命令
     │ 2. applyCommand()          # 提交到 Raft
     ▼
┌────────────────────┐
│  hashicorp/raft    │
│ Apply(cmdData)     │
└────┬───────────────┘
     │ 复制到多数派节点
     │ 应用到所有节点的 FSM
     ▼
┌────────────────────┐
│   raftFSM          │
│ Apply()            │
└────┬───────────────┘
     │ 1. 解析命令封装
     │ 2. applyRegister()         # 分发到处理器
     ▼
┌────────────────────┐
│  memoryRegistry    │
│ RegisterInstance() │
└────┬───────────────┘
     │ 1. 写入 instances
     │ 2. 创建 checks
     │ 3. 推进索引
     │ 4. 唤醒 watchers
     ▼
┌────────────────────┐
│    返回响应         │
│ index, checkIDs    │
└────────────────────┘
```

### 11.2 读操作流程（查询健康实例）

```
┌──────────┐
│  Client  │
└────┬─────┘
     │ GET /v1/health/service/api?passing=1&index=100&wait=30s
     ▼
┌────────────────────┐
│   HTTP Server      │
│ handleHealthService│
└────┬───────────────┘
     │ 1. 解析参数
     │ 2. WatchService(ns, svc, lastIndex)  # 注册监听
     ▼
┌────────────────────┐
│  RaftRegistry      │
│ WatchService()     │
└────┬───────────────┘
     │ 转发到 mem.WatchService()
     ▼
┌────────────────────┐
│  memoryRegistry    │
│ WatchService()     │
└────┬───────────────┘
     │ 如果 svcIndex > lastIndex: 立即返回
     │ 否则: 创建通道，加入 watchers 列表
     ▼
┌────────────────────┐
│  等待变更或超时     │
│ select {           │
│   case <-ch:       │ # 服务变更
│   case <-timeout:  │ # 30s 超时
│ }                  │
└────┬───────────────┘
     │ ListHealthyInstances()
     ▼
┌────────────────────┐
│  返回实例列表       │
│ X-Index: 105       │
└────────────────────┘
```

### 11.3 TTL 过期流程

```
┌────────────────────┐
│  Leader 节点       │
│  TTL Expirer       │
└────┬───────────────┘
     │ 每秒触发 Ticker
     ▼
┌────────────────────┐
│  memoryRegistry    │
│ expireOnce()       │
└────┬───────────────┘
     │ 1. 扫描所有 TTL 检查
     │ 2. 判断是否超时 (now - LastPass > TTL)
     │ 3. 标记为 critical
     │ 4. 推进服务索引
     │ 5. 唤醒 watchers
     ▼
┌────────────────────┐
│  等待中的客户端     │
│  收到变更通知       │
└────────────────────┘
```

---

## 12. 并发与锁策略

### 12.1 读写锁

**memoryRegistry**：
```go
mu sync.RWMutex

// 读操作 - 共享锁
func (m *memoryRegistry) ListHealthyInstances(...) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    // 读取 instances, checks
}

// 写操作 - 排他锁
func (m *memoryRegistry) RegisterInstance(...) {
    m.mu.Lock()
    defer m.mu.Unlock()
    // 修改 instances, checks, index
}
```

**优势**：
- 读操作并发执行，提高查询吞吐量
- 写操作串行，保证索引单调递增

### 12.2 快照并发

**raftFSM.Snapshot()**：
```go
f.mu.Lock()         // FSM 快照锁
defer f.mu.Unlock()

f.mem.mu.RLock()    // 内存只读锁
defer f.mem.mu.RUnlock()

// 复制数据到快照
```

**设计要点**：
- 快照期间允许读操作
- 快照期间阻止写操作（通过 FSM 锁）
- 避免快照数据不一致

### 12.3 Watchers 生命周期

**创建**：
```go
ch := make(chan struct{}, 1)  // 带缓冲，避免阻塞通知
m.watchers[svc] = append(m.watchers[svc], ch)
```

**触发**：
```go
for _, ch := range m.watchers[svc] {
    select {
    case ch <- struct{}{}:  // 尝试发送
    default:                // 已通知过，跳过
    }
    close(ch)               // 关闭通道，通知客户端
}
m.watchers[svc] = nil       // 清空列表
```

**优势**：
- 边缘触发，避免重复通知
- 关闭通道释放资源，避免泄漏
- 客户端取消后在下次变更时清理

---

## 13. 部署拓扑

### 13.1 单机房部署（推荐）

```
┌─────────────────────────────────────────┐
│           Datacenter 1                  │
│                                         │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐ │
│  │  Node1  │  │  Node2  │  │  Node3  │ │
│  │ Leader  │  │Follower │  │Follower │ │
│  └─────────┘  └─────────┘  └─────────┘ │
│                                         │
│  ┌────────────────────────────────┐    │
│  │   Service Hosts (with Agents)  │    │
│  └────────────────────────────────┘    │
└─────────────────────────────────────────┘
```

**特点**：
- 3-5 节点 Raft 集群
- 强一致性，秒级故障恢复
- Agents 部署在服务主机上

### 13.2 跨机房部署（未来）

```
┌──────────────────┐         ┌──────────────────┐
│   Datacenter 1   │         │   Datacenter 2   │
│   (Primary)      │         │   (Standby)      │
│  ┌─────────────┐ │  Async  │  ┌─────────────┐ │
│  │ Raft Cluster│◄┼─────────┼─►│ Raft Cluster│ │
│  │  3 nodes    │ │ Repl    │  │  3 nodes    │ │
│  └─────────────┘ │         │  └─────────────┘ │
└──────────────────┘         └──────────────────┘
```

**特点**：
- 每个机房独立 Raft 集群
- 异步复制（最终一致）
- 就近读取，降低延迟

---

## 14. 可观测性

### 14.1 日志

**当前实现**（M2）：
- 请求日志：`[Method] [Path] [Duration]`
- 注册日志：`Registered: ns/service/id`
- Raft 日志：hashicorp/raft 内置日志

**规划**（M3）：
- 结构化日志（JSON）
- 日志级别控制（DEBUG/INFO/WARN/ERROR）
- 关键操作审计（注册/注销）

### 14.2 指标（M3 规划）

**Prometheus 指标**：
```
# 注册表指标
sider_registry_instances_total{namespace="default",service="api"}
sider_registry_checks_total{status="passing"}
sider_registry_index

# Raft 指标
sider_raft_leader{node="node1"}
sider_raft_commit_index
sider_raft_last_log_index

# HTTP 指标
sider_http_requests_total{path="/v1/health/service/api",status="200"}
sider_http_request_duration_seconds

# Watch 指标
sider_watchers_active{service="api"}
```

### 14.3 分布式追踪（M3 规划）

- OpenTelemetry 集成
- 追踪注册→复制→查询全链路
- 关联请求 ID

---

## 15. 安全机制

### 15.1 当前状态（M2）

- ❌ 无认证
- ❌ 无加密
- ❌ 无 ACL

### 15.2 规划（M3）

#### mTLS
- Raft 节点间 mTLS
- Agent ↔ Server mTLS
- 自动证书轮换

#### ACL/RBAC
```
Token: <jwt-token>
Permissions:
  - namespace: "default"
    services: ["api", "web"]
    actions: ["read", "write"]
```

#### 审计日志
- 记录所有写操作
- 包含用户身份、操作类型、时间戳

---

## 16. 性能与可扩展性

### 16.1 性能指标（预期）

| 指标 | 目标 | 备注 |
|------|------|------|
| 注册 TPS | 1000+ | 单 Leader |
| 查询 QPS | 10000+ | 多节点并发读 |
| 长轮询并发 | 10000+ | 内存占用 < 1GB |
| TTL 续约延迟 | < 10ms | 本地内存操作 |
| Leader 选举时间 | 1-3s | hashicorp/raft 默认配置 |

### 16.2 可扩展性

**垂直扩展**：
- 增加 CPU/内存，提高单节点性能
- 适用于服务数 < 10000 的场景

**水平扩展**：
- 增加 Follower 节点，分担读负载
- 引入陈旧读（stale=true），降低 Leader 压力

**分片**（M4）：
- 按 Namespace 分片，多个独立集群
- 服务网格（Service Mesh）集成

### 16.3 瓶颈与优化

**瓶颈**：
1. Leader 写入吞吐量受 Raft 复制延迟限制
2. Watch 风暴（热点服务频繁变更）
3. TTL 过期扫描（O(n) 复杂度）

**优化方向**：
1. Batch 写入（批量注册）
2. 事件聚合/去抖（防抖动）
3. 增量快照（仅记录变更）
4. checkID → svcKey 索引（加速查找）

---

## 17. 测试策略

### 17.1 单元测试

**memoryRegistry**：
- 注册/注销/查询
- 索引推进
- TTL 续约与过期
- Watch 触发
- 健康聚合

**raftFSM**：
- 命令应用
- 快照创建与恢复
- 错误处理

**raftcmd**：
- 命令构建与解析
- 编解码正确性

### 17.2 集成测试

**单节点**：
- 启动 server
- Agent 注册服务
- 查询健康实例
- 停止 Agent，验证 TTL 过期

**多节点**：
- 启动 3 节点集群
- 注册到 Leader
- Follower 查询一致性
- 停止 Leader，验证选举
- 新 Leader 继续提供服务

### 17.3 性能测试

**负载场景**：
- 1000 服务 × 10 实例 = 10000 实例
- 10000 并发长轮询
- 1000 TPS 注册/注销
- 10000 TPS TTL 续约

**测试工具**：
- `wrk` / `hey`：HTTP 压测
- 自定义脚本：模拟 Agent 行为

---

## 18. 路线图

### M1（已完成）
- ✅ 内存注册表
- ✅ HTTP API
- ✅ Agent（TTL、HTTP、TCP、Cmd 检查）
- ✅ 长轮询

### M2（当前）
- ✅ Raft 共识（hashicorp/raft）
- ✅ 持久化（BoltDB + 快照）
- ✅ 集群管理（join/remove）
- ✅ Leader TTL 过期
- ✅ 代码重构（raftcmd、raftreg、raftfsm 分离）

### M3（规划中）
- 🔲 DNS 接口（A/SRV 记录）
- 🔲 ACL/mTLS
- 🔲 Prometheus 指标
- 🔲 CLI 工具
- 🔲 Web UI
- 🔲 陈旧读（stale=true）

### M4（远期）
- 🔲 跨机房复制
- 🔲 事件聚合/去抖
- 🔲 增量快照
- 🔲 备份/恢复
- 🔲 多租户配额

---

## 附录

### A. 术语表

| 术语 | 含义 |
|------|------|
| Namespace | 命名空间，逻辑隔离单元 |
| Service | 服务名，同一服务可有多个实例 |
| Instance | 服务实例，唯一标识为 ns/service/id |
| Check | 健康检查，可附加到实例上 |
| TTL | Time To Live，心跳超时时间 |
| ModifyIndex | 修改索引，全局单调递增 |
| Watch | 监听服务变更，阻塞等待 |
| Leader | Raft 集群主节点，处理所有写请求 |
| Follower | Raft 集群从节点，接收日志复制 |
| FSM | Finite State Machine，有限状态机 |
| WAL | Write-Ahead Log，预写日志 |

### B. 参考资料

- [Consul 官方文档](https://www.consul.io/docs)
- [hashicorp/raft](https://github.com/hashicorp/raft)
- [Raft 论文](https://raft.github.io/raft.pdf)
- [服务发现最佳实践](https://microservices.io/patterns/service-registry.html)

### C. 源码导航

| 功能 | 文件位置 | 行号 |
|------|----------|------|
| 数据模型 | `internal/registry/types.go` | 1-89 |
| Registry 接口 | `internal/registry/registry.go` | 1-50 |
| 内存实现 | `internal/registry/memory.go` | 1-442 |
| Raft 命令 | `internal/registry/raftcmd.go` | 1-181 |
| Raft 封装 | `internal/registry/raftreg.go` | 1-135 |
| Raft FSM | `internal/registry/raftfsm.go` | 1-238 |
| HTTP API | `internal/api/http.go` | 1-304 |
| 服务端装配 | `internal/server/server.go` | 1-68 |
| 集群管理 | `internal/server/cluster.go` | 1-71 |
| Agent | `internal/agent/agent.go` | 1-500+ |

---

**文档版本**：v2.0
**最后更新**：2025-01-15
**维护者**：Sider Team

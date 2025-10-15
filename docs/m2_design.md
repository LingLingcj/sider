# M2 设计文档（同城高可用 + 代码重构）

本文档描述 Sider 在 M2 阶段的高可用设计、代码重构方案与当前落地情况。

---

## 目录

1. [设计目标](#1-设计目标)
2. [技术方案](#2-技术方案)
3. [代码重构](#3-代码重构)
4. [数据持久化](#4-数据持久化)
5. [TTL 过期分布式化](#5-ttl-过期分布式化)
6. [集群管理](#6-集群管理)
7. [升级演进路径](#7-升级演进路径)
8. [测试验证](#8-测试验证)

---

## 1. 设计目标

### 1.1 高可用目标

- **容错能力**：3/5 节点容忍 1/2 故障，写与强读连续可用（存在多数派）
- **一致性保证**：注册/注销/健康变更线性化；TTL 过期也以事件方式复制
- **持久化能力**：重启不丢数据（WAL + 快照）
- **自动恢复**：Leader 失效自动选举，秒级恢复服务

### 1.2 代码质量目标

- **模块化**：清晰的职责分离，便于测试和维护
- **可扩展性**：平滑演进到真实多节点部署
- **可读性**：消除重复代码，统一命名和注释风格
- **兼容性**：保持 HTTP API 不变，客户端无感知

---

## 2. 技术方案

### 2.1 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                    Client (Agent/Service)                   │
└──────────────────────────┬──────────────────────────────────┘
                           │ HTTP API
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                      Sider Cluster                          │
│                                                             │
│  ┌───────────────────────────────────────────────────┐     │
│  │            HTTP Server (api.HTTPServer)           │     │
│  │  - 路由请求                                        │     │
│  │  - 长轮询处理                                      │     │
│  │  - 集群管理接口 (POST /v1/raft/join)             │     │
│  └───────────────┬───────────────────────────────────┘     │
│                  │                                          │
│                  ▼                                          │
│  ┌───────────────────────────────────────────────────┐     │
│  │        RaftRegistry (registry.RaftRegistry)       │     │
│  │  写操作：                                          │     │
│  │    1. BuildXxxCommand() - 构建命令                │     │
│  │    2. applyCommand()    - 提交到 Raft             │     │
│  │    3. ParseXxxResponse() - 解析响应               │     │
│  │  读操作：                                          │     │
│  │    - 直接转发到 memoryRegistry                    │     │
│  └───────────────┬───────────────────────────────────┘     │
│                  │                                          │
│                  ▼                                          │
│  ┌───────────────────────────────────────────────────┐     │
│  │         hashicorp/raft (Raft Protocol)            │     │
│  │  - Leader 选举                                     │     │
│  │  - 日志复制                                        │     │
│  │  - 持久化 (BoltDB)                                 │     │
│  └───────────────┬───────────────────────────────────┘     │
│                  │ Apply Log                                │
│                  ▼                                          │
│  ┌───────────────────────────────────────────────────┐     │
│  │          raftFSM (registry.raftFSM)               │     │
│  │  Apply():                                          │     │
│  │    - 解析命令封装 (commandEnvelope)               │     │
│  │    - 分发到命令处理器:                            │     │
│  │      · applyRegister()                             │     │
│  │      · applyDeregister()                           │     │
│  │      · applyRenewTTL()                             │     │
│  │      · applyReportCheck()                          │     │
│  │  Snapshot() / Restore():                           │     │
│  │    - 全量 JSON 序列化/反序列化                    │     │
│  └───────────────┬───────────────────────────────────┘     │
│                  │                                          │
│                  ▼                                          │
│  ┌───────────────────────────────────────────────────┐     │
│  │      memoryRegistry (registry.memoryRegistry)     │     │
│  │  - 维护服务实例 (instances)                       │     │
│  │  - 维护健康检查 (checks)                          │     │
│  │  - 管理索引 (index, svcIndex)                     │     │
│  │  - 管理 Watchers                                  │     │
│  │  - TTL 过期器 (仅 Leader)                         │     │
│  └───────────────────────────────────────────────────┘     │
│                                                             │
│  ┌───────────────────────────────────────────────────┐     │
│  │            raftcmd (registry.raftcmd)             │     │
│  │  - 命令类型定义 (registerCommand, etc.)           │     │
│  │  - 命令构建函数 (BuildRegisterCommand, etc.)      │     │
│  │  - 响应解析函数 (ParseRegisterResponse, etc.)     │     │
│  │  - 状态转换工具 (statusString, parseStatus)       │     │
│  └───────────────────────────────────────────────────┘     │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 数据流设计

#### 写路径（注册实例）
```
Client
  ↓ PUT /v1/agent/service/register
HTTPServer.handleRegister()
  ↓ Reg.RegisterInstance()
RaftRegistry.RegisterInstance()
  ↓ 1. BuildRegisterCommand(inst, specs) → cmdData
  ↓ 2. applyCommand(cmdData) → raft.Apply()
hashicorp/raft
  ↓ 复制到多数派节点
  ↓ 应用到所有节点的 FSM
raftFSM.Apply(log)
  ↓ 1. 解析 commandEnvelope
  ↓ 2. applyRegister(envelope.Data)
  ↓ 3. mem.RegisterInstance(inst, specs)
memoryRegistry.RegisterInstance()
  ↓ 1. 写入 instances, checks
  ↓ 2. 推进索引 (index, svcIndex)
  ↓ 3. 唤醒 watchers
  ↓ 4. 返回 (index, checkIDs, error)
RaftRegistry
  ↓ ParseRegisterResponse(respData)
HTTPServer
  ↓ 返回 JSON + X-Index 头
Client
```

#### 读路径（查询健康实例）
```
Client
  ↓ GET /v1/health/service/api?passing=1
HTTPServer.handleHealthService()
  ↓ Reg.ListHealthyInstances(ns, svc, opts)
RaftRegistry.ListHealthyInstances()
  ↓ mem.ListHealthyInstances() (直接读内存)
memoryRegistry.ListHealthyInstances()
  ↓ 1. 加 RLock
  ↓ 2. 遍历 instances, 过滤 passing
  ↓ 3. 返回 (views, svcIndex, nil)
RaftRegistry
  ↓ 转发结果
HTTPServer
  ↓ 返回 JSON + X-Index 头
Client
```

---

## 3. 代码重构

### 3.1 重构目标

M2 阶段对 Raft 相关代码进行了大规模重构，主要解决以下问题：
1. **代码拥挤**：raftfsm.go 和 raftreg.go 逻辑混杂，难以阅读
2. **重复代码**：4 个写方法（Register/Deregister/RenewTTL/ReportCheck）有相同的模式
3. **职责不清**：命令定义、编解码、处理逻辑耦合在一起

### 3.2 重构方案

#### 新增 raftcmd.go（181 行）

**职责**：统一管理 Raft 命令和响应的定义、构建、解析

**核心内容**：

```go
// ============================================================================
// 命令操作类型
// ============================================================================
const (
    opRegister    = "register"
    opDeregister  = "deregister"
    opRenewTTL    = "renew_ttl"
    opReportCheck = "report_check"
)

// ============================================================================
// 命令封装（Envelope）
// ============================================================================
type commandEnvelope struct {
    Op   string          `json:"op"`
    Data json.RawMessage `json:"data"`
}

// ============================================================================
// 命令负载类型
// ============================================================================
type registerCommand struct {
    Inst  ServiceInstance `json:"inst"`
    Specs []CheckSpec     `json:"specs"`
}

type deregisterCommand struct {
    Namespace string `json:"ns"`
    Service   string `json:"svc"`
    ID        string `json:"id"`
}

type checkCommand struct {
    ID     string `json:"id"`
    Status string `json:"status,omitempty"`
    Output string `json:"output,omitempty"`
}

// ============================================================================
// 响应类型
// ============================================================================
type registerResponse struct {
    Index    uint64   `json:"index"`
    CheckIDs []string `json:"check_ids,omitempty"`
    Err      string   `json:"err,omitempty"`
}

type indexResponse struct {
    Index uint64 `json:"index"`
    Err   string `json:"err,omitempty"`
}

// ============================================================================
// 命令构建函数
// ============================================================================
func BuildRegisterCommand(inst ServiceInstance, specs []CheckSpec) ([]byte, error)
func BuildDeregisterCommand(namespace, service, id string) ([]byte, error)
func BuildRenewTTLCommand(checkID string) ([]byte, error)
func BuildReportCheckCommand(checkID string, status CheckStatus, output string) ([]byte, error)

// ============================================================================
// 响应解析函数
// ============================================================================
func ParseRegisterResponse(data []byte) (index uint64, checkIDs []string, err error)
func ParseIndexResponse(data []byte) (index uint64, err error)

// ============================================================================
// 辅助工具
// ============================================================================
func statusString(s CheckStatus) string
func parseStatus(s string) CheckStatus
func encodeResponse(v interface{}) []byte
```

**优势**：
- 所有命令相关代码集中在一个文件
- 便于添加新命令类型
- 易于单独测试编解码逻辑

#### 重构 raftreg.go（110 → 135 行）

**重构前问题**：
```go
// 每个方法都重复这个模式
func (r *RaftRegistry) RegisterInstance(...) {
    env := struct{...}{Op: "register", Data: ...}
    b, _ := json.Marshal(env)
    f := r.raft.Apply(b, 5*time.Second)
    if err := f.Error(); err != nil { return 0, nil, err }
    var out rfRegRes
    _ = json.Unmarshal(f.Response().([]byte), &out)
    if out.Err != "" { return ..., errString(out.Err) }
    return ...
}
// 4 个写方法都是类似代码！
```

**重构后方案**：

1. **提取通用方法** `applyCommand()`：
```go
func (r *RaftRegistry) applyCommand(cmdData []byte) ([]byte, error) {
    future := r.raft.Apply(cmdData, 5*time.Second)
    if err := future.Error(); err != nil {
        return nil, err
    }
    respData, ok := future.Response().([]byte)
    if !ok {
        return nil, errString("invalid response type from raft")
    }
    return respData, nil
}
```

2. **简化写方法**：
```go
func (r *RaftRegistry) RegisterInstance(ctx context.Context, inst ServiceInstance, specs []CheckSpec) (uint64, []string, error) {
    // 1. 构建命令
    cmdData, err := BuildRegisterCommand(inst, specs)
    if err != nil {
        return 0, nil, err
    }

    // 2. 提交命令
    respData, err := r.applyCommand(cmdData)
    if err != nil {
        return 0, nil, err
    }

    // 3. 解析响应
    return ParseRegisterResponse(respData)
}
```

3. **添加清晰的分组注释**：
```go
// ============================================================================
// RaftRegistry 结构定义
// ============================================================================

// ============================================================================
// 写操作 - 通过 Raft 提交
// ============================================================================

// ============================================================================
// 读操作 - 直接从内存读取
// ============================================================================

// ============================================================================
// 内部辅助方法
// ============================================================================
```

**效果对比**：

| 指标 | 重构前 | 重构后 |
|------|--------|--------|
| 代码行数 | 110 | 135 |
| 重复代码 | 高 | 无 |
| 可读性 | 中等 | 优秀 |
| 可测试性 | 中等 | 优秀 |

#### 重构 raftfsm.go（147 → 238 行）

**重构前问题**：
- `Apply()` 方法包含 77 行的 switch-case 逻辑
- 每个命令的处理逻辑都内联在 case 中
- JSON 解析、错误处理代码重复

**重构后方案**：

1. **简化 Apply() 方法**（命令分发器模式）：
```go
func (f *raftFSM) Apply(l *hraft.Log) interface{} {
    // 解析命令封装
    var env commandEnvelope
    if err := json.Unmarshal(l.Data, &env); err != nil {
        return encodeResponse(indexResponse{Err: err.Error()})
    }

    // 根据操作类型分发处理
    switch env.Op {
    case opRegister:
        return f.applyRegister(env.Data)
    case opDeregister:
        return f.applyDeregister(env.Data)
    case opRenewTTL:
        return f.applyRenewTTL(env.Data)
    case opReportCheck:
        return f.applyReportCheck(env.Data)
    default:
        return encodeResponse(indexResponse{Err: "unknown op: " + env.Op})
    }
}
```

2. **提取命令处理器**：
```go
// applyRegister 处理注册命令
func (f *raftFSM) applyRegister(data json.RawMessage) interface{} {
    var cmd registerCommand
    if err := json.Unmarshal(data, &cmd); err != nil {
        return encodeResponse(indexResponse{Err: err.Error()})
    }

    idx, checkIDs, err := f.mem.RegisterInstance(context.TODO(), cmd.Inst, cmd.Specs)
    if err != nil {
        return encodeResponse(registerResponse{
            Index:    idx,
            CheckIDs: checkIDs,
            Err:      err.Error(),
        })
    }

    return encodeResponse(registerResponse{
        Index:    idx,
        CheckIDs: checkIDs,
    })
}

// applyDeregister 处理注销命令
func (f *raftFSM) applyDeregister(data json.RawMessage) interface{} {
    var cmd deregisterCommand
    if err := json.Unmarshal(data, &cmd); err != nil {
        return encodeResponse(indexResponse{Err: err.Error()})
    }

    idx, err := f.mem.DeregisterInstance(context.TODO(), cmd.Namespace, cmd.Service, cmd.ID)
    if err != nil {
        return encodeResponse(indexResponse{Index: idx, Err: err.Error()})
    }

    return encodeResponse(indexResponse{Index: idx})
}

// 其他命令处理器类似...
```

3. **改进快照数据结构命名**：
```go
// snapshotData 快照数据结构
type snapshotData struct {
    Instances map[string]ServiceInstance `json:"instances"`
    Checks    map[string]Check            `json:"checks"`
    IDToKeys  map[string][]string         `json:"id_to_keys"`
    SvcIndex  map[string]uint64           `json:"svc_index"`
    Index     uint64                      `json:"index"`
}
```

**效果对比**：

| 指标 | 重构前 | 重构后 |
|------|--------|--------|
| Apply() 方法行数 | 77 | 20 |
| 命令处理逻辑 | 内联 | 独立方法 |
| 可读性 | 较差 | 优秀 |
| 可维护性 | 较差 | 优秀 |

### 3.3 重构总结

**改进统计**：

| 文件 | 重构前行数 | 重构后行数 | 说明 |
|------|-----------|-----------|------|
| `raftcmd.go` | 0（不存在） | 181 | 新增，命令管理 |
| `raftreg.go` | 110 | 135 | +25 行（注释增加，重复减少） |
| `raftfsm.go` | 147 | 238 | +91 行（逻辑分离，更清晰） |
| **总计** | 257 | 554 | +297 行（结构更清晰） |

**代码质量提升**：
- ✅ 消除重复代码，提取通用方法
- ✅ 职责清晰分离，便于测试
- ✅ 命令统一管理，易于扩展
- ✅ 添加详细注释和分组
- ✅ 修复 context 使用问题（nil → context.TODO()）

---

## 4. 数据持久化

### 4.1 持久化组件

每个 Raft 节点维护三种存储：

#### StableStore（raft-stable.db）
- **内容**：当前任期（Term）、投票记录（VotedFor）
- **实现**：BoltDB
- **大小**：< 1MB

#### LogStore（raft-log.db）
- **内容**：Raft 日志条目（WAL）
- **实现**：BoltDB
- **大小**：随日志积累增长，可通过快照压缩

#### SnapshotStore（文件目录）
- **内容**：状态机快照（JSON 格式）
- **实现**：文件系统
- **保留策略**：最近 2 个快照

### 4.2 快照机制

#### 触发条件
```go
rcfg.SnapshotInterval = 20 * time.Second  // 时间间隔
rcfg.SnapshotThreshold = 8192             // 日志条目数
```

满足任一条件即触发快照。

#### 快照内容
```json
{
  "instances": {
    "default/api/api-1": {
      "namespace": "default",
      "service": "api",
      "id": "api-1",
      "address": "192.168.1.10",
      "port": 8080,
      ...
    }
  },
  "checks": {
    "chk:api-1:0": {
      "id": "chk:api-1:0",
      "spec": {...},
      "status": "passing",
      ...
    }
  },
  "id_to_keys": {
    "api-1": ["default/api/api-1"]
  },
  "svc_index": {
    "default/api": 12345
  },
  "index": 12345
}
```

#### 快照恢复流程
1. 节点启动时检测快照文件
2. 调用 `raftFSM.Restore(rc io.ReadCloser)`
3. 反序列化 JSON 到内存结构
4. 重建 `instances`, `checks`, `idToKeys`, `svcIndex`, `index`
5. 清空 `watchers`（不持久化）

### 4.3 数据目录结构
```
data/
├── node1/
│   ├── raft-stable.db      # 元数据
│   ├── raft-log.db         # WAL
│   ├── snapshots/
│   │   ├── 1-100-1234567890/
│   │   │   ├── state.bin   # 快照数据
│   │   │   └── meta.json   # 快照元信息
│   │   └── 2-200-1234567900/
│   │       ├── state.bin
│   │       └── meta.json
│   └── raft.db (已弃用)
├── node2/
│   └── ...
└── node3/
    └── ...
```

---

## 5. TTL 过期分布式化

### 5.1 设计目标

- TTL 过期仅在 Leader 上执行，避免多节点重复过期
- 过期事件通过 Raft 日志复制到所有节点
- Leader 切换时，新 Leader 自动接管过期职责

### 5.2 实现方案

#### Leader 监听与过期器控制
```go
// server/server.go:36-40
go func(ch <-chan bool) {
    for isLeader := range ch {
        if isLeader {
            mem.StartExpirer()   // 成为 Leader，启动过期器
        } else {
            mem.Stop()           // 失去 Leader，停止过期器
        }
    }
}(rn.Raft.LeaderCh())
```

#### 过期器实现（memory.go:340-384）
```go
func (m *memoryRegistry) expirer() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            m.expireOnce()
        case <-m.stopCh:
            return
        }
    }
}

func (m *memoryRegistry) expireOnce() {
    now := time.Now()
    m.mu.Lock()
    defer m.mu.Unlock()

    // 记录哪些服务发生了变化
    changedSvc := make(map[string]bool)
    for _, cr := range m.checks {
        if cr.chk.Spec.Type != CheckTTL {
            continue
        }
        ttl := cr.chk.Spec.TTL
        if ttl <= 0 || cr.chk.LastPass.IsZero() {
            continue
        }
        if now.Sub(cr.chk.LastPass) > ttl {
            if cr.chk.Status != StatusCritical {
                cr.chk.Status = StatusCritical
                cr.chk.LastUpdate = now
                svc := m.findSvcKeyByCheckLocked(cr.chk.ID)
                if svc != "" {
                    changedSvc[svc] = true
                }
            }
        }
    }

    // 批量推进索引，唤醒 watchers
    for svc := range changedSvc {
        m.nextIndexLocked(svc)
    }
}
```

### 5.3 Leader 切换流程
```
1. 原 Leader 失效
   ↓
2. Follower 检测心跳超时，发起选举
   ↓
3. 新 Leader 当选
   ↓
4. server.go 监听到 LeaderCh() 变化
   ↓
5. 调用 mem.StartExpirer() 启动过期器
   ↓
6. 新 Leader 开始执行 TTL 过期扫描
```

**恢复时间**：1-3 秒（取决于 Raft 选举超时配置）

---

## 6. 集群管理

### 6.1 集群引导（Bootstrap）

#### 单节点引导
```bash
bin/sds-server \
  -http :8500 \
  -raft-id node1 \
  -raft-bind :8501 \
  -raft-dir data/node1 \
  -raft-bootstrap=true
```

**流程**：
1. 检测数据目录是否有状态（`raftHasExistingState()`）
2. 如果无状态，执行 `BootstrapCluster()`
3. 创建单节点配置，成为 Leader

#### 代码实现（server/cluster.go:50-59）
```go
if cfg.Bootstrap {
    hasState, err := raftHasExistingState(stableStore, logStore, snapStore)
    if err != nil { return nil, err }
    if !hasState {
        // 单节点引导
        c := hraft.Configuration{
            Servers: []hraft.Server{
                {ID: rcfg.LocalID, Address: transport.LocalAddr()},
            },
        }
        f := r.BootstrapCluster(c)
        if f.Error() != nil { return nil, fmt.Errorf("bootstrap: %w", f.Error()) }
    }
}
```

### 6.2 节点加入（Join）

#### 操作流程
```bash
# 1. 启动新节点（不引导）
bin/sds-server \
  -http :9500 \
  -raft-id node2 \
  -raft-bind :9501 \
  -raft-dir data/node2 \
  -raft-bootstrap=false

# 2. 向 Leader 发送加入请求
curl -X POST 'http://127.0.0.1:8500/v1/raft/join' \
  -H 'Content-Type: application/json' \
  -d '{"ID":"node2","Addr":"127.0.0.1:9501"}'
```

#### API 实现（api/http.go:255-273）
```go
func (h *HTTPServer) handleRaftJoin(w http.ResponseWriter, r *http.Request) {
    if h.Joiner == nil {
        http.Error(w, "raft not enabled", http.StatusNotImplemented)
        return
    }
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    // 仅 Leader 接受加入请求
    if h.IsLeader != nil && !h.IsLeader() {
        http.Error(w, "not leader", http.StatusBadRequest)
        return
    }
    var req struct{ ID, Addr string }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    if req.ID == "" || req.Addr == "" {
        http.Error(w, "missing id/addr", http.StatusBadRequest)
        return
    }
    if err := h.Joiner.Join(req.ID, req.Addr); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    w.WriteHeader(http.StatusOK)
}
```

#### Joiner 实现（server/server.go:53-67）
```go
type raftJoiner struct { Raft *hraft.Raft }

func (j raftJoiner) Join(nodeID, addr string) error {
    // 检查节点是否已存在
    cfgFuture := j.Raft.GetConfiguration()
    if err := cfgFuture.Error(); err != nil { return err }
    for _, s := range cfgFuture.Configuration().Servers {
        if s.ID == hraft.ServerID(nodeID) || s.Address == hraft.ServerAddress(addr) {
            return nil  // 已存在，忽略
        }
    }
    // 添加为 Voter
    f := j.Raft.AddVoter(hraft.ServerID(nodeID), hraft.ServerAddress(addr), 0, 0)
    return f.Error()
}
```

### 6.3 节点移除（未来）

计划在 M3 实现：
```bash
curl -X POST 'http://leader:8500/v1/raft/remove' \
  -d '{"ID":"node2"}'
```

---

## 7. 升级演进路径

### 7.1 从 M1 到 M2

**不兼容变更**：
- ❌ M1 数据无法迁移（内存无持久化）
- ❌ 需要重新注册所有服务

**兼容变更**：
- ✅ HTTP API 完全兼容
- ✅ Agent 配置格式不变
- ✅ 响应头 `X-Index` 语义不变

**升级步骤**：
1. 停止 M1 服务端和所有 Agents
2. 部署 M2 集群（3-5 节点）
3. 重启 Agents，自动重新注册
4. 验证服务发现功能

### 7.2 M2 集群滚动升级（未来）

**目标**：支持无停机升级代码

**方案**：
1. 逐个停止 Follower 节点
2. 更新二进制文件
3. 重启节点，自动同步日志
4. 最后升级 Leader（触发选举）

**限制**：
- 需要保持 Raft 协议兼容性
- 快照格式需要版本化

---

## 8. 测试验证

### 8.1 单元测试

**raftcmd**：
```go
TestBuildRegisterCommand()
TestParseRegisterResponse()
TestStatusConversion()
```

**raftFSM**：
```go
TestApplyRegister()
TestApplyDeregister()
TestSnapshot()
TestRestore()
```

**raftRegistry**：
```go
TestRegisterInstance()
TestApplyCommand()
```

### 8.2 集成测试

**单节点**：
```bash
# 启动服务端
bin/sds-server -http :8500 -raft-bootstrap=true

# 注册服务
curl -X PUT http://localhost:8500/v1/agent/service/register \
  -d '{"Name":"api","ID":"api-1","Address":"127.0.0.1","Port":8080}'

# 查询健康实例
curl http://localhost:8500/v1/health/service/api?ns=default&passing=1

# 停止服务端，重启，验证数据持久化
```

**多节点**：
```bash
# 启动 3 节点集群
bin/sds-server -http :8500 -raft-id node1 -raft-bind :8501 -raft-bootstrap=true
bin/sds-server -http :9500 -raft-id node2 -raft-bind :9501 &
bin/sds-server -http :10500 -raft-id node3 -raft-bind :10501 &

# 加入集群
curl -X POST http://localhost:8500/v1/raft/join \
  -d '{"ID":"node2","Addr":"127.0.0.1:9501"}'
curl -X POST http://localhost:8500/v1/raft/join \
  -d '{"ID":"node3","Addr":"127.0.0.1:10501"}'

# 注册到 Leader
curl -X PUT http://localhost:8500/v1/agent/service/register \
  -d '{"Name":"api","ID":"api-1","Address":"127.0.0.1","Port":8080}'

# 从 Follower 查询（验证复制）
curl http://localhost:9500/v1/health/service/api?ns=default

# 停止 Leader，验证选举和服务连续性
kill <leader-pid>
sleep 3
curl http://localhost:9500/v1/health/service/api?ns=default
```

### 8.3 性能测试

**目标**：
- 注册 TPS: 500+
- 查询 QPS: 5000+
- 选举时间: < 3s

**工具**：
```bash
# HTTP 压测
hey -n 10000 -c 100 'http://localhost:8500/v1/health/service/api'

# 模拟 Agent
go run scripts/bench_agent.go --count 1000 --ttl 15s
```

---

## 附录

### A. 配置参数

**服务端**：
```bash
sds-server \
  -http :8500 \              # HTTP 监听地址
  -raft-id node1 \           # 节点唯一 ID
  -raft-bind :8501 \         # Raft 监听地址
  -raft-dir data/node1 \     # 数据目录
  -raft-bootstrap true       # 是否引导
```

### B. 监控指标（M3 规划）

```
sider_raft_leader{node="node1"} 1
sider_raft_commit_index 12345
sider_raft_last_log_index 12350
sider_registry_index 12345
sider_registry_instances_total 1000
sider_registry_checks_total{status="passing"} 980
```

### C. 故障排查

**问题：节点无法加入集群**
- 检查 Leader 状态：`curl http://leader:8500/v1/raft/stats`
- 检查网络连通性：`telnet node2-addr 9501`
- 查看 Raft 日志：`tail -f data/node1/raft.log`

**问题：数据不一致**
- 检查多数派状态：至少 2 个节点存活
- 查看日志索引：所有节点的 `CommitIndex` 应接近
- 触发快照：等待 20 秒自动快照

---

**文档版本**：v2.0
**最后更新**：2025-01-15
**维护者**：Sider Team

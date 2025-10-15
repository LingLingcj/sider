# Sider - 轻量级服务发现系统

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Build Status](https://img.shields.io/badge/build-passing-brightgreen.svg)](https://github.com)

**Sider** 是一个受 Consul 启发的轻量级服务发现系统，支持高可用、多种健康检查方式和实时变更通知。

---

## 📋 目录

- [特性](#-特性)
- [架构概览](#-架构概览)
- [快速开始](#-快速开始)
- [使用指南](#-使用指南)
- [配置说明](#-配置说明)
- [API 文档](#-api-文档)
- [测试方法](#-测试方法)
- [故障排查](#-故障排查)
- [开发指南](#-开发指南)
- [路线图](#-路线图)
- [贡献](#-贡献)
- [许可证](#-许可证)

---

## ✨ 特性

### 核心功能
- ✅ **服务注册与发现**：动态注册服务实例，实时查询健康实例
- ✅ **高可用架构**：基于 Raft 共识协议，支持 3-5 节点集群
- ✅ **数据持久化**：BoltDB + 快照，重启不丢数据
- ✅ **多种健康检查**：
  - **TTL**：基于心跳的简单检查
  - **HTTP**：定期访问 HTTP 端点
  - **TCP**：检查 TCP 端口连通性
  - **命令**：执行自定义脚本
- ✅ **实时变更通知**：长轮询机制，秒级感知服务变更
- ✅ **多租户隔离**：基于 Namespace 的逻辑隔离

### 当前版本（M2）
- ✅ Raft 共识协议（hashicorp/raft）
- ✅ Leader 选举与自动故障恢复
- ✅ TTL 过期分布式化（仅 Leader 执行）
- ✅ 集群管理接口（节点加入/移除）
- ✅ 代码重构（模块化、可测试）

### 规划功能（M3+）
- 🔲 DNS 接口（A/SRV 记录）
- 🔲 ACL/mTLS 安全机制
- 🔲 Prometheus 指标导出
- 🔲 CLI 工具和 Web UI
- 🔲 跨机房异步复制

---

## 🏗️ 架构概览

```
┌─────────────────────────────────────────────────────────┐
│                   Client Applications                    │
└───────────────────────┬─────────────────────────────────┘
                        │ HTTP API
                        ▼
┌────────────────────────────────────────────────────────┐
│              Sider Cluster (3-5 nodes)                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐             │
│  │  Node 1  │  │  Node 2  │  │  Node 3  │             │
│  │ (Leader) │  │(Follower)│  │(Follower)│             │
│  ├──────────┤  ├──────────┤  ├──────────┤             │
│  │   HTTP   │  │   HTTP   │  │   HTTP   │             │
│  │  Server  │  │  Server  │  │  Server  │             │
│  ├──────────┤  ├──────────┤  ├──────────┤             │
│  │  Raft    │◄─┼──Raft────┼─►│  Raft    │             │
│  │  FSM     │  │ Protocol  │  │  FSM     │             │
│  ├──────────┤  ├──────────┤  ├──────────┤             │
│  │ Memory   │  │ Memory   │  │ Memory   │             │
│  │ Registry │  │ Registry │  │ Registry │             │
│  ├──────────┤  ├──────────┤  ├──────────┤             │
│  │ BoltDB   │  │ BoltDB   │  │ BoltDB   │             │
│  └──────────┘  └──────────┘  └──────────┘             │
└────────────────────────────────────────────────────────┘
                        ▲
                        │ Register & Heartbeat
                        │
┌────────────────────────────────────────────────────────┐
│                    Sider Agents                         │
│         (部署在各服务主机上，管理本地服务)              │
└────────────────────────────────────────────────────────┘
```

---

## 🚀 快速开始

### 前置要求

- **Go**: 1.23 或更高版本
- **操作系统**: Linux / macOS / Windows
- **端口**: 8500 (HTTP), 8501 (Raft)

### 1. 克隆项目

```bash
git clone https://github.com/your-org/sider.git
cd sider
```

### 2. 构建

```bash
make build
```

**产物**：
- `bin/sds-server` - 服务端
- `bin/sds-agent` - Agent

### 3. 启动单节点（开发模式）

#### 终端 A：启动服务端
```bash
make run-server
# 或者
bin/sds-server -http :8500
```

服务端将监听 `http://127.0.0.1:8500`

#### 终端 B：启动 Agent（简单模式）
```bash
make run-agent
# 或者
bin/sds-agent \
  -server http://127.0.0.1:8500 \
  -ns default \
  -service demo \
  -id demo-1 \
  -addr 127.0.0.1 \
  -port 8080 \
  -ttl 15s
```

### 4. 验证服务注册

#### 列出所有服务
```bash
curl -s 'http://127.0.0.1:8500/v1/catalog/services?ns=default' | jq
```

**预期输出**：
```json
["demo"]
```

#### 查询健康实例
```bash
curl -s 'http://127.0.0.1:8500/v1/health/service/demo?ns=default&passing=1' | jq
```

**预期输出**：
```json
[
  {
    "Namespace": "default",
    "Service": "demo",
    "ID": "demo-1",
    "Address": "127.0.0.1",
    "Port": 8080,
    "Tags": null,
    "Meta": null,
    "Weights": {"Passing": 1, "Warning": 1}
  }
]
```

---

## 📖 使用指南

### 高可用集群部署

#### 1. 启动第一个节点（引导）

```bash
bin/sds-server \
  -http :8500 \
  -raft-id node1 \
  -raft-bind :8501 \
  -raft-dir data/node1 \
  -raft-bootstrap=true
```

#### 2. 启动第二个节点

```bash
bin/sds-server \
  -http :9500 \
  -raft-id node2 \
  -raft-bind :9501 \
  -raft-dir data/node2 \
  -raft-bootstrap=false &

# 加入集群
curl -X POST 'http://127.0.0.1:8500/v1/raft/join' \
  -H 'Content-Type: application/json' \
  -d '{"ID":"node2","Addr":"127.0.0.1:9501"}'
```

#### 3. 启动第三个节点

```bash
bin/sds-server \
  -http :10500 \
  -raft-id node3 \
  -raft-bind :10501 \
  -raft-dir data/node3 \
  -raft-bootstrap=false &

# 加入集群
curl -X POST 'http://127.0.0.1:8500/v1/raft/join' \
  -H 'Content-Type: application/json' \
  -d '{"ID":"node3","Addr":"127.0.0.1:10501"}'
```

#### 验证集群状态

```bash
# 查看所有节点
curl -s http://127.0.0.1:8500/v1/raft/stats | jq

# 注册服务到集群
curl -X PUT http://127.0.0.1:8500/v1/agent/service/register \
  -H 'Content-Type: application/json' \
  -d '{
    "Namespace": "default",
    "Name": "api",
    "ID": "api-1",
    "Address": "192.168.1.10",
    "Port": 8080,
    "Tags": ["v1.0", "production"],
    "Checks": [
      {
        "Type": "ttl",
        "TTL": "15s"
      }
    ]
  }'

# 从任意节点查询（验证复制）
curl -s http://127.0.0.1:9500/v1/health/service/api?ns=default | jq
```

### 使用 Agent 配置文件

#### 1. 创建配置文件

参考 `examples/agent.demo.json`：

```json
{
  "server": "http://127.0.0.1:8500",
  "services": [
    {
      "namespace": "default",
      "name": "api-gateway",
      "id": "api-gateway-1",
      "address": "192.168.1.10",
      "port": 8080,
      "tags": ["v1.0", "production"],
      "meta": {
        "version": "1.0.0",
        "region": "us-west"
      },
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
    },
    {
      "namespace": "default",
      "name": "user-service",
      "id": "user-service-1",
      "address": "192.168.1.11",
      "port": 8081,
      "checks": [
        {
          "type": "tcp",
          "interval": "10s",
          "timeout": "3s"
        }
      ]
    }
  ]
}
```

#### 2. 启动 Agent

```bash
make run-agent-config
# 或者
bin/sds-agent -config examples/agent.demo.json
```

#### 3. 验证多服务注册

```bash
# 列出所有服务
curl -s 'http://127.0.0.1:8500/v1/catalog/services?ns=default' | jq

# 查询 api-gateway
curl -s 'http://127.0.0.1:8500/v1/health/service/api-gateway?ns=default' | jq

# 查询 user-service
curl -s 'http://127.0.0.1:8500/v1/health/service/user-service?ns=default' | jq
```

### 长轮询（实时变更通知）

#### 客户端实现

```bash
#!/bin/bash
INDEX=0

while true; do
  echo "Watching service 'api' from index $INDEX..."

  # 长轮询，最多等待 30 秒
  RESPONSE=$(curl -s -i "http://127.0.0.1:8500/v1/health/service/api?ns=default&passing=1&index=$INDEX&wait=30s")

  # 提取新索引
  NEW_INDEX=$(echo "$RESPONSE" | grep -i "X-Index:" | awk '{print $2}' | tr -d '\r')

  if [ "$NEW_INDEX" != "$INDEX" ]; then
    echo "Service changed! New index: $NEW_INDEX"
    echo "$RESPONSE" | tail -n 1 | jq
    INDEX=$NEW_INDEX
  else
    echo "No change (timeout)"
  fi

  sleep 1
done
```

### 健康检查示例

#### TTL 检查（Agent 自动续约）

```bash
# Agent 会每 10 秒（2/3 * 15s）自动续约
bin/sds-agent -server http://127.0.0.1:8500 -service api -ttl 15s
```

#### HTTP 检查

```json
{
  "type": "http",
  "path": "/health",
  "interval": "10s",
  "timeout": "3s"
}
```

**健康判定**：
- `2xx/3xx` → `passing`
- `4xx` → `warning`
- 其他/超时 → `critical`

#### TCP 检查

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

#### 命令检查

```json
{
  "type": "cmd",
  "cmd": ["/usr/bin/check-app.sh"],
  "interval": "30s",
  "timeout": "10s"
}
```

**健康判定**：
- 退出码 `0` → `passing`
- 退出码 `非0` → `critical`
- 输出作为 `Output` 字段

---

## ⚙️ 配置说明

### 服务端参数

```bash
sds-server [flags]

Flags:
  -http string
        HTTP 监听地址 (默认 ":8500")

  -raft-id string
        Raft 节点唯一标识（集群模式必需）

  -raft-bind string
        Raft 通信地址（集群模式必需，格式: host:port）

  -raft-dir string
        数据存储目录（集群模式必需）

  -raft-bootstrap bool
        是否作为引导节点（仅第一个节点设置为 true）
```

### Agent 参数

#### 简单模式（单服务）

```bash
sds-agent [flags]

Flags:
  -server string
        Sider 服务端地址 (默认 "http://127.0.0.1:8500")

  -ns string
        命名空间 (默认 "default")

  -service string
        服务名称

  -id string
        实例 ID（默认: service-hostname-port）

  -addr string
        实例地址

  -port int
        实例端口

  -ttl string
        TTL 检查超时时间（如: "15s"）

  -deregister bool
        退出时是否自动注销（默认: true）
```

#### 配置文件模式（多服务）

```bash
sds-agent -config <file_or_dir>

# 指定文件
sds-agent -config /etc/sider/agent.json

# 指定目录（加载所有 .json 文件）
sds-agent -config /etc/sider/conf.d/
```

**配置文件格式**：参见 `examples/agent.demo.json`

---

## 🔌 API 文档

### 服务注册

#### 注册实例

```bash
PUT /v1/agent/service/register
Content-Type: application/json

{
  "Namespace": "default",
  "Name": "api",
  "ID": "api-1",
  "Address": "192.168.1.10",
  "Port": 8080,
  "Tags": ["v1.0", "production"],
  "Meta": {
    "version": "1.0.0",
    "region": "us-west"
  },
  "Weights": {
    "Passing": 10,
    "Warning": 1
  },
  "Checks": [
    {
      "Type": "ttl",
      "TTL": "15s"
    },
    {
      "Type": "http",
      "Path": "/health",
      "Interval": "10s",
      "Timeout": "3s"
    }
  ]
}
```

**响应**：
```json
{
  "Index": 123,
  "InstanceID": "api-1",
  "CheckIDs": ["chk:api-1:0", "chk:api-1:1"]
}
```

#### 注销实例（路径式）

```bash
PUT /v1/agent/service/deregister/{id}?ns={namespace}&service={service}
```

#### 注销实例（JSON）

```bash
PUT /v1/agent/service/deregister
Content-Type: application/json

{
  "Namespace": "default",
  "Service": "api",
  "ID": "api-1"
}
```

### 健康检查

#### 标记检查通过 / 续约 TTL

```bash
PUT /v1/agent/check/pass/{check_id}
```

#### 标记检查告警

```bash
PUT /v1/agent/check/warn/{check_id}
```

#### 标记检查失败

```bash
PUT /v1/agent/check/fail/{check_id}
```

### 服务查询

#### 列出所有服务

```bash
GET /v1/catalog/services?ns={namespace}
```

**响应**：
```json
["api", "web", "cache"]
```

#### 查询健康实例

```bash
GET /v1/health/service/{service}?ns={namespace}&passing=1&index={last_index}&wait={duration}
```

**参数**：
- `ns`: 命名空间（默认: `default`）
- `passing`: 仅返回健康实例（`1` 或 `true`）
- `index`: 长轮询起始索引
- `wait`: 最长等待时间（如: `30s`）

**响应头**：
```
X-Index: 125
```

**响应体**：
```json
[
  {
    "Namespace": "default",
    "Service": "api",
    "ID": "api-1",
    "Address": "192.168.1.10",
    "Port": 8080,
    "Tags": ["v1.0"],
    "Meta": {"version": "1.0.0"},
    "Weights": {"Passing": 10, "Warning": 1}
  }
]
```

### 集群管理

#### 加入集群

```bash
POST /v1/raft/join
Content-Type: application/json

{
  "ID": "node2",
  "Addr": "127.0.0.1:9501"
}
```

**注意**：仅 Leader 接受此请求

---


---

## 🗺️ 路线图

### M1（已完成）✅
- 内存注册表
- HTTP API
- Agent（TTL、HTTP、TCP、Cmd 检查）
- 长轮询

### M2（当前）✅
- Raft 共识
- 数据持久化
- Leader 选举
- 集群管理
- 代码重构

### M3（规划中）🚧
- DNS 接口（A/SRV 记录）
- ACL/mTLS 安全
- Prometheus 指标
- CLI 工具
- Web UI
- 陈旧读（stale=true）

### M4（远期）📅
- 跨机房异步复制
- 事件聚合/去抖
- 增量快照
- 备份/恢复
- 多租户配额

---

## 📄 许可证

本项目采用 MIT 许可证 - 详见 [LICENSE](LICENSE) 文件

---

## 📚 相关文档

- [架构文档](docs/architecture.md) - 详细架构设计
- [M2 设计文档](docs/m2_design.md) - 高可用设计
- [开发者指南](docs/dev_guide.md) - 开发指南
- [API 文档](docs/api.md) - 完整 API 说明

---

- [Consul](https://www.consul.io/) - 灵感来源
- [hashicorp/raft](https://github.com/hashicorp/raft) - Raft 实现
- [BoltDB](https://github.com/boltdb/bolt) - 持久化存储

---


# Sider（服务发现）- M1 骨架

一个受 Consul 启发的最小可用服务发现原型。本阶段（M1）包含：
- 内存注册表（暂不持久化）
- HTTP API：注册/注销/检查状态、健康查询与长轮询
- Agent：注册实例并按 TTL 周期续约；支持通过 JSON 配置注册多个服务与多种检查（ttl/http/tcp/cmd）

## 目录结构
- cmd/sds-server：服务端入口
- cmd/sds-agent：Agent 入口
- internal/registry：数据模型 + 内存注册表（TTL/Watch）
- internal/api：HTTP API 处理

## 构建
需要 Go 1.23+。

make build

产物：
- bin/sds-server
- bin/sds-agent

## 运行
终端A：

make run-server

终端B：启动一个带 TTL 检查的演示 Agent：

make run-agent

或使用配置文件一次性注册多个服务/检查（示例配置见 examples/agent.demo.json）：

make run-agent-config

## 试用
列出服务：

curl -s 'http://127.0.0.1:8500/v1/catalog/services?ns=default'

查询健康实例（可长轮询至多 30s）：

curl -i 'http://127.0.0.1:8500/v1/health/service/demo?ns=default&passing=1&index=1&wait=30s'

注销（路径式）：

curl -X PUT 'http://127.0.0.1:8500/v1/agent/service/deregister/<id>?ns=default&service=demo'

## 说明
- Agent 以 TTL 的 2/3 周期续约；若杀掉 Agent，服务端会在 TTL 左右将实例降为 critical，`passing=1` 查询不再返回该实例。
- Agent 现在支持 http/tcp/cmd 检查：
  - http：2xx/3xx 视为通过；4xx 警告；其他失败（简化逻辑）。
  - tcp：能连通视为通过，否则失败。
  - cmd：进程退出码 0 通过，非 0 失败，输出会作为检查说明（当前服务端未持久化输出，仅用于调试）。
- 支持通过 `-config` 指定 JSON 文件或目录；目录下的所有 .json 文件都会被加载。
- DNS 与持久化会在后续里程碑加入。

## 下一步
- 引入持久化与 Raft，实现高可用
- 提供 DNS A/SRV 记录
- Agent 增加 HTTP/TCP/Cmd 检查
- 安全：ACL/mTLS

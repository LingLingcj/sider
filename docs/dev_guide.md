# 开发者指南（中文）

本文面向参与 Sider 的开发者，介绍代码结构、开发构建、关键路径与约定。

## 代码结构概览
- cmd/sds-server：服务端入口，装配 Registry 与 HTTP API。
- cmd/sds-agent：Agent 入口，按配置注册服务与执行检查。
- internal/api：HTTP API（路由、处理、长轮询）。
  - 新增 `/v1/raft/join` 用于集群加入（仅 Leader 接受）。
- internal/registry：数据模型与实现
  - memory.go：内存注册表，含索引、watch、TTL 过期；支持按需启动/停止过期器。
  - registry.go：Registry 接口定义。
  - raftreg.go：基于 Raft 的 Registry 封装（写经 Raft，读直读内存）。
- internal/raft：已不再使用桩实现，接入 hashicorp/raft；如需切换，请直接替换 server 装配。
- docs/：架构说明与开发文档。

## 构建与运行
- 构建：`make build`（受限环境可加 `GOCACHE=$(pwd)/.gocache`）。
- 运行服务端：`make run-server`。
- 运行 Agent：`make run-agent`（单服务 TTL）或 `make run-agent-config`（多服务/多检查）。

## 关键路径与设计要点
- 索引与 watch：
  - 全局 `index` 与按服务 `svcIndex[ns/service]`；watch 按服务边缘触发，配合长轮询 `index+wait`。
- TTL 过期：
  - M2 桩实现仍由 `memoryRegistry` 内部定时扫描；未来真实 Raft 时只在 Leader 执行，并以命令提交过期事件。
- Raft 封装：
  - 写路径通过 `internal/raft.Node.Propose` 提交到 `FSM.Apply`，立即应用在本地（单节点）；
  - 未来替换为多节点 Raft 后，API 保持不变。

## 代码约定
- 注释与文档使用中文为主，必要时附英文关键字；
- 默认 ASCII 源码；只在必须时引入非 ASCII；
- 重要逻辑前给出简短注释，避免冗长解释；
- 变更时遵守接口稳定性：优先新增构造/选项以避免破坏现有调用；
- 单元测试优先覆盖：注册/注销/索引推进、TTL 续约与过期、watch 行为、长轮询超时。

## 后续任务（M2 → 真正 HA）
- 替换 `localNode` 为真实 Raft：复制、选举、WAL、快照、ReadIndex；
- TTL 过期迁移到 Leader 模块；
- 增加 `stale=1` 强/弱读参数处理；
- 指标与可观测性；
- Watch 资源释放与上下文绑定；

# Sider 架构文档（M1）

本文档描述 Sider 在 M1 阶段的整体架构、数据模型、核心机制、API 设计与演进路线。M1 以“最小可用”为目标：内存注册表、HTTP API、Agent TTL 健康检查与长轮询 Watch。后续将按路线图引入持久化、Raft、DNS、ACL/mTLS 等能力。

## 1. 背景与目标
- 目标：提供一个中心化的服务注册与发现组件，使服务实例能动态注册/下线，消费者能查询到健康实例列表并订阅变更。
- 适用场景：微服务集群内部寻址；灰度/多版本；客户端负载均衡。
- M1 范围：
  - 内存注册表（重启丢失）
  - HTTP API（注册/注销/检查上报/健康查询与长轮询）
  - Agent（注册、TTL 续约）
- 非目标（M1）：持久化/HA、DNS、ACL/mTLS、跨机房复制、UI。

## 2. 总体架构
组件说明：
- sds-server：服务端进程，承载 HTTP API，使用内存 Registry 存储状态。
- Registry：领域状态存储与变更分发，维护 Service/Instance/Check、ModifyIndex、Watchers，以及 TTL 过期逻辑。
- HTTP API：将外部 REST 请求转换为 Registry 调用，支持长轮询返回增量变化。
- sds-agent：部署在业务机上，向 server 注册实例并按 TTL 周期续约；可按 JSON 配置注册多个服务，执行 http/tcp/cmd 等外部检查并上报结果。

目录与模块（简要）：
- cmd/sds-server：服务端入口与参数解析
- cmd/sds-agent：Agent 入口与参数解析
- internal/server：装配 Registry 与 API
- internal/api：HTTP 路由与处理
- internal/registry：数据模型与内存实现（含 Watch 与 TTL 过期）
- internal/agent：Agent 客户端（注册、TTL 续约、外部检查运行器）

关键文件：
- sider/internal/registry/types.go:1 数据模型
- sider/internal/registry/registry.go:1 Registry 接口
- sider/internal/registry/memory.go:1 内存实现（索引/Watch/TTL）
- sider/internal/api/http.go:1 HTTP API 实现
- sider/internal/server/server.go:1 服务端装配
- sider/internal/agent/agent.go:1 Agent 实现

## 3. 数据模型
- Namespace：逻辑租户/隔离域（字符串）。
- Service：服务名（字符串）。
- ServiceInstance：
  - 字段：Namespace、Service、ID、Address、Port、Tags、Meta、Weights
  - 内部索引：CreateIndex、ModifyIndex（单调递增）
- CheckSpec / Check：
  - 类型：ttl/http/tcp/cmd（M1 仅 ttl 有行为）
  - 状态：passing/warning/critical/unknown
  - TTL/Interval/Timeout（字符串入参解析为 duration）
- ListOptions：查询过滤（PassingOnly、Tag、Zone；M1 仅实现 PassingOnly）。
- InstanceView：对外返回的精简实例视图（去除内部索引）。

健康聚合规则：
- 无检查视为 passing（方便快速接入）。
- 有检查时采用“最坏优先”：出现 critical 则实例为 critical；否则 unknown；否则 warning；全 passing 才 passing。

## 4. 一致性与索引模型
- M1：内存实现，串行写 + 进程内锁，等价于强一致单节点。
- 修改索引：
  - 全局索引 `index`：每次写操作自增。
  - 每服务索引 `svcIndex[ns/service]`：跟随全局索引，记录该服务最近一次变更的索引。
- 客户端通过响应头 `X-Index` 获知最新索引；长轮询以此为“水位线”。
- 读语义：M1 无副本读/stale 读；后续 Raft 后支持线性化读与 follower stale 读。

## 5. 事件与 Watch（长轮询）
- Watcher 由 `WatchService(ns, service, lastIndex)` 注册：
  - 若 `svcIndex > lastIndex`，立即返回已关闭的通知通道（边缘触发）。
  - 否则将带缓冲通道加入 `watchers[ns/service]` 列表，等待下一次变更时统一唤醒并清空。
- 触发时机：注册/注销/检查状态改变/TTL 过期。
- 客户端使用：GET /v1/health/service/<name>?ns=...&index=<last>&wait=30s。

## 6. 健康检查
- TTL 检查：
  - 注册时若声明 TTL，检查初始状态为 critical（避免未就绪即对外可见）。
  - Agent 每 2/3 TTL 续约一次（PUT /v1/agent/check/pass/<check_id>）。
  - 后台过期器每秒扫描；若 `now - LastPass > TTL`，置为 critical 并推进索引。
- 外部检查（http/tcp/cmd）：由 Agent 定期执行并通过 `PUT /v1/agent/check/{pass|warn|fail}/{check_id}` 上报结果；HTTP 2xx/3xx 视为通过，4xx 警告，其他失败（简化逻辑）。

## 7. HTTP API 设计
- 注册实例：
  - PUT /v1/agent/service/register（JSON）
  - 返回头：X-Index；体内包含 `InstanceID` 与 `CheckIDs`。
- 注销实例：
  - PUT /v1/agent/service/deregister/<id>?ns=&service=
  - 或 PUT /v1/agent/service/deregister（JSON）
- 检查上报：
  - PUT /v1/agent/check/pass|warn|fail/<check_id>
  - 对 TTL，`pass` 等同续约；其他类型走显式上报。
- 列服务：GET /v1/catalog/services?ns=<ns>
- 健康查询（可长轮询）：
  - GET /v1/health/service/<name>?ns=<ns>&passing=1&index=<last>&wait=30s
  - 返回头：X-Index

约定：错误 4xx/5xx，成功 2xx；JSON 响应；TTL/Interval/Timeout 使用 Go duration 格式（如“15s”）。

## 8. 运行时流程
- 注册：Agent/客户端 -> Register -> Registry 写入实例与检查 -> 更新索引 -> 唤醒 watchers。
- 续约：Agent -> check/pass -> TTL 检查置 passing，更新时间与索引。
- 过期：后台扫描 TTL 超时 -> 置 critical -> 更新索引。
- 查询：客户端 -> health/service（passing=1）-> 返回实例视图与 X-Index；带 index+wait 可长轮询变更。
- 注销：客户端 -> deregister -> 删除实例与检查 -> 更新索引。

## 9. 并发与锁
- Registry 使用 `sync.RWMutex`：写路径串行推进索引与唤醒 watchers；读路径并发查询。
- Watchers 为一次性通道：变更时批量 close 并清空列表，避免 goroutine 泄漏。
- TTL 过期器：`time.Ticker(1s)` 驱动 `expireOnce`，与写路径同锁保护。

## 10. 安全（后续）
- M1：无 ACL/mTLS。
- 规划：
  - mTLS（短周期证书）保护所有链路；
  - ACL/RBAC：Token 携带命名空间与权限；
  - 审计日志。

## 11. 可观测性
- M1：基础日志（请求访问日志、注册与续约日志）。
- 规划：Prometheus 指标（索引推进、watchers 数、过期次数等），OpenTelemetry Tracing。

## 12. 部署与配置
- sds-server：
  - 参数：`-http`（默认 `:8500`）
  - 信号：SIGINT/SIGTERM 优雅退出
- sds-agent：
  - 参数：`-server/-ns/-service/-id/-addr/-port/-ttl`（单服务模式，向后兼容）
  - 或 `-config <file_or_dir>` 加载 JSON 配置（可多服务，多检查类型）；`-deregister` 控制退出时是否自动注销。
  - ID 默认：`<service>-<hostname>-<port>`

## 13. 可扩展性与 HA（M2+）
- Raft 共识：
  - 写入线性化；3/5 节点；BoltDB/Badger 持久化；快照/日志压缩；
  - 读模型：强一致读 + follower stale 读；
  - 兼容现有 HTTP 语义（X-Index、长轮询不变）。
- DNS：
  - A/AAAA/SRV 记录；按 passing 过滤；权重轮询；短 TTL（3–10s）。
- 多机房：
  - 同城强一致；跨城异步复制（replicator）。

## 14. 风险与权衡
- TTL 初始 critical：需要 Agent 续约后才对外可见（更安全，但首次暴露延迟）。
- watchers 清理：客户端取消后在下一次变更前仍占用一个通道槽位（影响极小）；可绑定 ctx 或定期清理。
- 查找 check 所属服务为 O(n)：大规模时可引入 `checkID -> svcKey` 索引。
- 标签/可用区：当前仅占位；需要尽快落地过滤与排序逻辑。

## 15. 测试策略
- 单元测试：
  - Registry：注册/注销/索引推进；TTL 续约与过期；Watch 触发；健康聚合。
  - API：参数校验、错误码、长轮询超时。
- 集成测试：
  - server+agent：注册后通过 health/service 可查到实例；停止 agent 后 TTL 过期剔除。
- 压测（后续）：热点服务 watch 风暴、批量续约、冷/热服务查询。

## 16. 路线图
- M1（已完成）：内存注册表、HTTP、Agent TTL、长轮询。
- M2：引入 Raft + 持久化、快照压缩、Follower stale 读、Prometheus 指标。
- M3：DNS、更多健康检查、ACL/mTLS、CLI/UI。
- M4：热点优化（事件聚合、去抖）、备份/恢复、多 DC 复制。

## 17. 术语表
- Passing：健康通过；Warning：告警但可选；Critical：健康失败；Unknown：未知状态。
- TTL：基于时间的心跳超时；续约即“心跳”。
- ModifyIndex：全局单调递增的变更序号；用于增量感知与长轮询。
- Watch：阻塞等待某服务的索引推进，返回变更信号。

## 参考源码位置
- 模型：sider/internal/registry/types.go:1
- 内存注册表：sider/internal/registry/memory.go:1
- HTTP API：sider/internal/api/http.go:1
- 服务端装配：sider/internal/server/server.go:1
- Agent：sider/internal/agent/agent.go:1

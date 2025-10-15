package registry

import (
	"context"
)

// Registry 抽象了服务/健康状态存储。
// M1 实现为内存版；后续可替换为基于 Raft 的强一致存储。
type Registry interface {
	RegisterInstance(ctx context.Context, inst ServiceInstance, specs []CheckSpec) (idx uint64, checkIDs []string, err error)
	DeregisterInstance(ctx context.Context, namespace, service, id string) (idx uint64, err error)

	// TTL 与外部检查
	RenewTTL(ctx context.Context, checkID string) (idx uint64, err error)
	ReportCheck(ctx context.Context, checkID string, status CheckStatus, output string) (idx uint64, err error)

	// 读接口
	ListHealthyInstances(ctx context.Context, namespace, service string, opts ListOptions) (views []InstanceView, idx uint64, err error)
	ListServices(ctx context.Context, namespace string) (names []string, idx uint64, err error)

	// 监听指定服务的变更；若 lastIndex 落后，会立刻触发一次通知。
	WatchService(ctx context.Context, namespace, service string, lastIndex uint64) (idx uint64, notify <-chan struct{})
}

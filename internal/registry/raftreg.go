package registry

import (
	"context"
	"time"

	hraft "github.com/hashicorp/raft"
)

// raftreg.go - RaftRegistry 实现
// 使用 hashicorp/raft 执行写入提交；读直接从内存快照读。

// ============================================================================
// RaftRegistry 结构定义
// ============================================================================

// RaftRegistry 使用 Raft 实现的注册表：写操作通过 Raft 日志复制，读操作直接访问内存。
type RaftRegistry struct {
	raft *hraft.Raft
	mem  *memoryRegistry
}

// NewRaftRegistry 创建一个新的 RaftRegistry 实例
func NewRaftRegistry(r *hraft.Raft, mem *memoryRegistry) *RaftRegistry {
	return &RaftRegistry{raft: r, mem: mem}
}

// Stop 停止底层注册表（包括 TTL 过期器）
func (r *RaftRegistry) Stop() {
	r.mem.Stop()
}

// ============================================================================
// 写操作 - 通过 Raft 提交
// ============================================================================

// RegisterInstance 注册服务实例（写操作，通过 Raft 复制）
func (r *RaftRegistry) RegisterInstance(ctx context.Context, inst ServiceInstance, specs []CheckSpec) (uint64, []string, error) {
	cmdData, err := BuildRegisterCommand(inst, specs)
	if err != nil {
		return 0, nil, err
	}

	respData, err := r.applyCommand(cmdData)
	if err != nil {
		return 0, nil, err
	}

	return ParseRegisterResponse(respData)
}

// DeregisterInstance 注销服务实例（写操作，通过 Raft 复制）
func (r *RaftRegistry) DeregisterInstance(ctx context.Context, namespace, service, id string) (uint64, error) {
	cmdData, err := BuildDeregisterCommand(namespace, service, id)
	if err != nil {
		return 0, err
	}

	respData, err := r.applyCommand(cmdData)
	if err != nil {
		return 0, err
	}

	return ParseIndexResponse(respData)
}

// RenewTTL 续约 TTL 检查（写操作，通过 Raft 复制）
func (r *RaftRegistry) RenewTTL(ctx context.Context, checkID string) (uint64, error) {
	cmdData, err := BuildRenewTTLCommand(checkID)
	if err != nil {
		return 0, err
	}

	respData, err := r.applyCommand(cmdData)
	if err != nil {
		return 0, err
	}

	return ParseIndexResponse(respData)
}

// ReportCheck 报告健康检查结果（写操作，通过 Raft 复制）
func (r *RaftRegistry) ReportCheck(ctx context.Context, checkID string, status CheckStatus, output string) (uint64, error) {
	cmdData, err := BuildReportCheckCommand(checkID, status, output)
	if err != nil {
		return 0, err
	}

	respData, err := r.applyCommand(cmdData)
	if err != nil {
		return 0, err
	}

	return ParseIndexResponse(respData)
}

// ============================================================================
// 读操作 - 直接从内存读取
// ============================================================================

// ListHealthyInstances 查询健康实例（读操作，直接从内存读取）
func (r *RaftRegistry) ListHealthyInstances(ctx context.Context, namespace, service string, opts ListOptions) ([]InstanceView, uint64, error) {
	return r.mem.ListHealthyInstances(ctx, namespace, service, opts)
}

// ListServices 列出所有服务（读操作，直接从内存读取）
func (r *RaftRegistry) ListServices(ctx context.Context, namespace string) ([]string, uint64, error) {
	return r.mem.ListServices(ctx, namespace)
}

// WatchService 监听服务变更（读操作，直接从内存监听）
func (r *RaftRegistry) WatchService(ctx context.Context, namespace, service string, lastIndex uint64) (uint64, <-chan struct{}) {
	return r.mem.WatchService(ctx, namespace, service, lastIndex)
}

// ============================================================================
// 内部辅助方法
// ============================================================================

// applyCommand 提交命令到 Raft 并等待响应
func (r *RaftRegistry) applyCommand(cmdData []byte) ([]byte, error) {
	future := r.raft.Apply(cmdData, 5*time.Second)
	if err := future.Error(); err != nil {
		return nil, err
	}

	// 响应数据由 FSM.Apply 返回（已编码为 []byte）
	respData, ok := future.Response().([]byte)
	if !ok {
		return nil, errString("invalid response type from raft")
	}

	return respData, nil
}

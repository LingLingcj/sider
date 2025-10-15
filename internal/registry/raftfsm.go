package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"

	hraft "github.com/hashicorp/raft"
)

// raftfsm.go - Raft FSM 实现
// 实现 hashicorp/raft 的 FSM 接口，将日志命令映射到 memoryRegistry。
// 快照实现为全量 JSON dump，便于后续替换为更高效实现。

// ============================================================================
// raftFSM 结构定义
// ============================================================================

// raftFSM 实现 hashicorp/raft 的 FSM 接口
type raftFSM struct {
	mem *memoryRegistry
	mu  sync.Mutex // 保护 Snapshot 期间的并发读
}

// NewRaftFSMForServer 供 server 组装 Raft 使用
func NewRaftFSMForServer(mem *memoryRegistry) *raftFSM {
	return &raftFSM{mem: mem}
}

// ============================================================================
// FSM 接口实现
// ============================================================================

// Apply 应用日志命令到状态机
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

// Snapshot 将内存状态全量序列化
func (f *raftFSM) Snapshot() (hraft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mem.mu.RLock()
	defer f.mem.mu.RUnlock()

	// 构造快照视图（仅必要��段）
	snap := snapshotData{
		Instances: make(map[string]ServiceInstance, len(f.mem.instances)),
		Checks:    make(map[string]Check, len(f.mem.checks)),
		IDToKeys:  make(map[string][]string, len(f.mem.idToKeys)),
		SvcIndex:  make(map[string]uint64, len(f.mem.svcIndex)),
		Index:     f.mem.index,
	}

	// 复制数据
	for k, rec := range f.mem.instances {
		snap.Instances[k] = rec.inst
	}
	for k, cr := range f.mem.checks {
		snap.Checks[k] = cr.chk
	}
	for k, v := range f.mem.idToKeys {
		snap.IDToKeys[k] = append([]string(nil), v...)
	}
	for k, v := range f.mem.svcIndex {
		snap.SvcIndex[k] = v
	}

	// watchers 不入快照
	data, _ := json.Marshal(snap)
	return &memSnapshot{data: data}, nil
}

// Restore 从快照恢复内存状态
func (f *raftFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()

	var snap snapshotData
	if err := json.NewDecoder(rc).Decode(&snap); err != nil {
		return err
	}

	f.mem.mu.Lock()
	defer f.mem.mu.Unlock()

	// 重建实例映射
	f.mem.instances = make(map[string]*instanceRecord, len(snap.Instances))
	for k, inst := range snap.Instances {
		f.mem.instances[k] = &instanceRecord{inst: inst}
	}

	// 重建检查映射
	f.mem.checks = make(map[string]*checkRecord, len(snap.Checks))
	for k, c := range snap.Checks {
		f.mem.checks[k] = &checkRecord{chk: c}
	}

	// 重建 ID 索引
	f.mem.idToKeys = make(map[string][]string, len(snap.IDToKeys))
	for k, v := range snap.IDToKeys {
		f.mem.idToKeys[k] = append([]string(nil), v...)
	}

	// 重建服务索引
	f.mem.svcIndex = make(map[string]uint64, len(snap.SvcIndex))
	for k, v := range snap.SvcIndex {
		f.mem.svcIndex[k] = v
	}

	f.mem.index = snap.Index

	// watchers 清空
	f.mem.watchers = make(map[string][]chan struct{})

	return nil
}

// ============================================================================
// 命令处理器（Command Handlers）
// ============================================================================

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

// applyRenewTTL 处理 TTL 续约命令
func (f *raftFSM) applyRenewTTL(data json.RawMessage) interface{} {
	var cmd checkCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return encodeResponse(indexResponse{Err: err.Error()})
	}

	idx, err := f.mem.RenewTTL(context.TODO(), cmd.ID)
	if err != nil {
		return encodeResponse(indexResponse{Index: idx, Err: err.Error()})
	}

	return encodeResponse(indexResponse{Index: idx})
}

// applyReportCheck 处理健康检查报告命令
func (f *raftFSM) applyReportCheck(data json.RawMessage) interface{} {
	var cmd checkCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return encodeResponse(indexResponse{Err: err.Error()})
	}

	status := parseStatus(cmd.Status)
	idx, err := f.mem.ReportCheck(context.TODO(), cmd.ID, status, cmd.Output)
	if err != nil {
		return encodeResponse(indexResponse{Index: idx, Err: err.Error()})
	}

	return encodeResponse(indexResponse{Index: idx})
}

// ============================================================================
// 快照相关类型
// ============================================================================

// snapshotData 快照数据结构
type snapshotData struct {
	Instances map[string]ServiceInstance `json:"instances"`
	Checks    map[string]Check           `json:"checks"`
	IDToKeys  map[string][]string        `json:"id_to_keys"`
	SvcIndex  map[string]uint64          `json:"svc_index"`
	Index     uint64                     `json:"index"`
}

// memSnapshot 实现 hraft.FSMSnapshot 接口
type memSnapshot struct {
	data []byte
}

func (m *memSnapshot) Persist(sink hraft.SnapshotSink) error {
	_, err := io.Copy(sink, bytes.NewReader(m.data))
	if err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (m *memSnapshot) Release() {}

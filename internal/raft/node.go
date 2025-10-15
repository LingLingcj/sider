package raft

import (
    "context"
    "errors"
    "sync/atomic"
)

// 本文件提供一个本地单节点的“Raft”桩实现，
// 用于在 M2 阶段搭建接口与调用链路。
// 未来可替换为真正的 Raft 实现（多节点复制、选主、WAL/快照）。

// Command 表示一次提交的有序写操作。
type Command struct {
    Type string
    Data []byte
}

// Result 表示状态机应用后的返回结果。
// Data 的结构由上层约定（通常为 JSON 编码）。
type Result struct {
    Index uint64
    Data  []byte
}

// FSM 是被复制状态机接口。
type FSM interface {
    Apply(cmd Command, index uint64) (Result, error)
}

// Node 抽象 Raft 节点：此处为单节点实现。
type Node interface {
    Start() error
    Stop() error
    Propose(ctx context.Context, cmd Command) (Result, error)
    IsLeader() bool
}

// localNode 为单节点实现：立即在本地应用，返回结果。
type localNode struct {
    fsm   FSM
    index uint64
    alive int32
}

func NewLocalNode(fsm FSM) Node {
    return &localNode{fsm: fsm}
}

func (l *localNode) Start() error {
    atomic.StoreInt32(&l.alive, 1)
    return nil
}
func (l *localNode) Stop() error {
    atomic.StoreInt32(&l.alive, 0)
    return nil
}

func (l *localNode) Propose(ctx context.Context, cmd Command) (Result, error) {
    if atomic.LoadInt32(&l.alive) == 0 {
        return Result{}, errors.New("raft node not started")
    }
    // 单节点：本地自增索引并直接应用
    l.index++
    idx := l.index
    select {
    case <-ctx.Done():
        return Result{}, ctx.Err()
    default:
    }
    return l.fsm.Apply(cmd, idx)
}

func (l *localNode) IsLeader() bool { return atomic.LoadInt32(&l.alive) == 1 }


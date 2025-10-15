package server

import (
    "fmt"
    "net"
    "os"
    "path/filepath"
    "time"

    hraft "github.com/hashicorp/raft"
    raftboltdb "github.com/hashicorp/raft-boltdb"
)

// raftNode 封装底层 raft 组件的装配。
type raftNode struct {
    Raft      *hraft.Raft
    Transport *hraft.NetworkTransport
}

type raftConfig struct {
    ID        string // 节点 ID（唯一）
    Bind      string // 监听地址（host:port）
    DataDir   string // 数据目录
    Bootstrap bool   // 首次引导为 true
}

func setupRaft(cfg raftConfig, fsm hraft.FSM) (*raftNode, error) {
    if err := os.MkdirAll(cfg.DataDir, 0755); err != nil { return nil, err }
    rcfg := hraft.DefaultConfig()
    rcfg.LocalID = hraft.ServerID(cfg.ID)
    rcfg.SnapshotInterval = 20 * time.Second
    rcfg.SnapshotThreshold = 8192

    addr, err := net.ResolveTCPAddr("tcp", cfg.Bind)
    if err != nil { return nil, err }
    transport, err := hraft.NewTCPTransport(cfg.Bind, addr, 3, 10*time.Second, os.Stderr)
    if err != nil { return nil, err }

    // 存储：BoltDB（稳定+日志），文件快照。
    stableStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-stable.db"))
    if err != nil { return nil, err }
    logStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.db"))
    if err != nil { return nil, err }
    snapStore, err := hraft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
    if err != nil { return nil, err }

    r, err := hraft.NewRaft(rcfg, fsm, logStore, stableStore, snapStore, transport)
    if err != nil { return nil, err }

    if cfg.Bootstrap {
        hasState, err := raftHasExistingState(stableStore, logStore, snapStore)
        if err != nil { return nil, err }
        if !hasState {
            // 单节点引导
            c := hraft.Configuration{Servers: []hraft.Server{{ID: rcfg.LocalID, Address: transport.LocalAddr()}}}
            f := r.BootstrapCluster(c)
            if f.Error() != nil { return nil, fmt.Errorf("bootstrap: %w", f.Error()) }
        }
    }
    return &raftNode{Raft: r, Transport: transport}, nil
}

func raftHasExistingState(st hraft.StableStore, lg hraft.LogStore, sn hraft.SnapshotStore) (bool, error) {
    // 简单判断：检查最新快照或日志是否存在
    if snap, err := sn.List(); err == nil && len(snap) > 0 { return true, nil }
    // 查询首条日志
    var idx uint64 = 1
    if err := lg.GetLog(idx, &hraft.Log{}); err == nil { return true, nil }
    return false, nil
}

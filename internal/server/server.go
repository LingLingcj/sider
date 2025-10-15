package server

import (
    "context"
    "log"
    "sider/internal/api"
    "sider/internal/registry"

    hraft "github.com/hashicorp/raft"
)

// Server 负责组装 Registry 与 HTTP API 并运行。
type Server struct {
    HTTPAddr string
    RaftID   string // 节点 ID
    RaftBind string // 监听地址（host:port）
    RaftDir  string // 数据目录
    Bootstrap bool  // 是否引导
}

func (s *Server) Run(ctx context.Context) error {
    // 1) 创建底层内存注册表（不自动启过期器，由 Leader 控制）。
    mem := registry.NewMemoryRegistryWithOptions(registry.Options{AutoExpirer: false})

    // 2) 启动 Raft（hashicorp/raft）。
    fsm := registry.NewRaftFSMForServer(mem)
    rn, err := setupRaft(raftConfig{ID: s.RaftID, Bind: s.RaftBind, DataDir: s.RaftDir, Bootstrap: s.Bootstrap}, fsm)
    if err != nil {
        log.Printf("Raft 启动失败: %v", err)
        return err
    }
    // 3) 将 Registry 写路径绑定到 Raft，读直读内存。
    rreg := registry.NewRaftRegistry(rn.Raft, mem)

    // 4) 监听领导权变化，控制 TTL 过期器只在 Leader 上运行。
    go func(ch <-chan bool) {
        for isLeader := range ch {
            if isLeader { mem.StartExpirer() } else { mem.Stop() }
        }
    }(rn.Raft.LeaderCh())

    // 5) 启动 HTTP 服务，并暴露 join 接口。
    httpSrv := &api.HTTPServer{Reg: rreg, Addr: s.HTTPAddr, Joiner: raftJoiner{Raft: rn.Raft}, IsLeader: func() bool { return rn.Raft.State() == hraft.Leader }}

    defer rreg.Stop()
    if err := httpSrv.Start(ctx); err != nil {
        log.Printf("HTTP 服务退出: %v", err)
        return err
    }
    return nil
}

// raftJoiner 通过 Raft API 接受新节点加入（只允许在 Leader 上调用）。
type raftJoiner struct { Raft *hraft.Raft }

func (j raftJoiner) Join(nodeID, addr string) error {
    // 若已存在则忽略
    cfgFuture := j.Raft.GetConfiguration()
    if err := cfgFuture.Error(); err != nil { return err }
    for _, s := range cfgFuture.Configuration().Servers {
        if s.ID == hraft.ServerID(nodeID) || s.Address == hraft.ServerAddress(addr) {
            return nil
        }
    }
    f := j.Raft.AddVoter(hraft.ServerID(nodeID), hraft.ServerAddress(addr), 0, 0)
    return f.Error()
}

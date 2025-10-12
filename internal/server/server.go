package server

import (
    "context"
    "log"
    "sider/internal/api"
    "sider/internal/registry"
)

// Server 负责组装 Registry 与 HTTP API 并运行。
type Server struct {
    HTTPAddr string
}

func (s *Server) Run(ctx context.Context) error {
    reg := registry.NewMemoryRegistry()
    httpSrv := &api.HTTPServer{Reg: reg, Addr: s.HTTPAddr}

    // M1 仅启动 HTTP；DNS 将在后续里程碑加入。
    defer reg.Stop() // 确保退出时停止后台过期器
    if err := httpSrv.Start(ctx); err != nil {
        log.Printf("HTTP 服务退出: %v", err)
        return err
    }
    return nil
}

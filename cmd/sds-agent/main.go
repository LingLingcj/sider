package main

import (
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "log"
    "os"
    "os/signal"
    "path/filepath"
    "strings"
    "syscall"
    "time"

    "sider/internal/agent"
    "sider/internal/api"
)

// 文件配置结构：支持单文件多服务，或目录下多文件。
type fileConfig struct {
    Server           string        `json:"server"`
    DeregisterOnExit bool          `json:"deregister_on_exit"`
    Services         []fileService `json:"services"`
}
type fileService struct {
    Namespace string            `json:"ns"`
    Service   string            `json:"service"`
    ID        string            `json:"id"`
    Address   string            `json:"address"`
    Addr      string            `json:"addr"` // 别名
    Port      int               `json:"port"`
    Tags      []string          `json:"tags"`
    Meta      map[string]string `json:"meta"`
    Checks    []api.CheckDef    `json:"checks"`
}

func main() {
    var cfgPath string
    var serverHTTP, ns, svc, id, addr string
    var port int
    var ttlStr string
    var dereg bool
    flag.StringVar(&cfgPath, "config", "", "JSON 配置文件路径，或包含多个 JSON 的目录")
    flag.StringVar(&serverHTTP, "server", "http://127.0.0.1:8500", "Server 的 HTTP 地址，例如 http://127.0.0.1:8500（配置文件可覆盖）")
    flag.StringVar(&ns, "ns", "default", "命名空间（单服务模式）")
    flag.StringVar(&svc, "service", "demo", "服务名（单服务模式）")
    flag.StringVar(&id, "id", "", "实例 ID（可选，单服务模式）")
    flag.StringVar(&addr, "addr", "127.0.0.1", "对外发布的地址（单服务模式）")
    flag.IntVar(&port, "port", 800, "服务端口（单服务模式）")
    flag.StringVar(&ttlStr, "ttl", "15s", "TTL（单服务模式，未在 checks 声明时生效）")
    flag.BoolVar(&dereg, "deregister", true, "进程退出时自动从 server 注销（配置文件可覆盖）")
    flag.Parse()

    ctx, cancel := signalContext()
    defer cancel()

    // 配置文件模式：可以同时注册多个服务，并支持多种检查。
    if cfgPath != "" {
        ags, err := loadAgentsFromPath(cfgPath, serverHTTP, dereg)
        if err != nil {
            log.Fatalf("加载配置失败: %v", err)
        }
        if len(ags) == 0 {
            log.Fatalf("未在 %s 中发现任何服务配置", cfgPath)
        }
        // 并发运行多个 Agent
        errCh := make(chan error, len(ags))
        for _, a := range ags {
            go func(a *agent.Agent) {
                errCh <- a.Run(ctx)
            }(a)
        }
        // 等待任一出错或收到退出信号
        select {
        case err := <-errCh:
            if err != nil {
                log.Fatalf("agent error: %v", err)
            }
        case <-ctx.Done():
        }
        return
    }

    // 单服务兼容模式：仅 TTL 检查
    ttl, err := time.ParseDuration(ttlStr)
    if err != nil {
        log.Fatalf("bad ttl: %v", err)
    }
    a := agent.New(agent.Config{
        ServerHTTP:       serverHTTP,
        Namespace:        ns,
        Service:          svc,
        ID:               id,
        Address:          addr,
        Port:             port,
        TTL:              ttl,
        DeregisterOnExit: dereg,
    })
    if err := a.Run(ctx); err != nil {
        log.Fatalf("agent error: %v", err)
    }
}

func signalContext() (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithCancel(context.Background())
    c := make(chan os.Signal, 2)
    signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-c
        cancel()
    }()
    return ctx, cancel
}

// loadAgentsFromPath 从文件或目录加载 JSON 配置，并构造多个 Agent。
func loadAgentsFromPath(path string, defaultServer string, defaultDeregister bool) ([]*agent.Agent, error) {
    st, err := os.Stat(path)
    if err != nil {
        return nil, err
    }
    var files []string
    if st.IsDir() {
        entries, err := os.ReadDir(path)
        if err != nil {
            return nil, err
        }
        for _, e := range entries {
            if e.IsDir() {
                continue
            }
            name := e.Name()
            if strings.HasSuffix(strings.ToLower(name), ".json") {
                files = append(files, filepath.Join(path, name))
            }
        }
    } else {
        files = []string{path}
    }
    var res []*agent.Agent
    for _, f := range files {
        ags, err := loadAgentsFromFile(f, defaultServer, defaultDeregister)
        if err != nil {
            return nil, fmt.Errorf("%s: %w", f, err)
        }
        res = append(res, ags...)
    }
    return res, nil
}

// loadAgentsFromFile 既支持顶层含 services 的聚合文件，也支持单服务文件。
func loadAgentsFromFile(file string, defaultServer string, defaultDeregister bool) ([]*agent.Agent, error) {
    f, err := os.Open(file)
    if err != nil {
        return nil, err
    }
    defer f.Close()
    b, err := io.ReadAll(f)
    if err != nil {
        return nil, err
    }
    // 先尝试聚合结构
    var fc fileConfig
    if err := json.Unmarshal(b, &fc); err == nil && (len(fc.Services) > 0 || fc.Server != "") {
        server := defaultIfEmpty(fc.Server, defaultServer)
        dereg := fc.DeregisterOnExit || defaultDeregister
        var out []*agent.Agent
        for _, s := range fc.Services {
            out = append(out, agent.New(convertFileService(server, dereg, s)))
        }
        return out, nil
    }
    // 尝试单服务结构（直接是 fileService）
    var s fileService
    if err := json.Unmarshal(b, &s); err != nil {
        return nil, fmt.Errorf("不支持的 JSON 结构: %w", err)
    }
    cfg := convertFileService(defaultServer, defaultDeregister, s)
    return []*agent.Agent{agent.New(cfg)}, nil
}

func convertFileService(server string, dereg bool, s fileService) agent.Config {
    address := s.Address
    if address == "" {
        address = s.Addr
    }
    return agent.Config{
        ServerHTTP:       server,
        Namespace:        defaultIfEmpty(s.Namespace, "default"),
        Service:          s.Service,
        ID:               s.ID,
        Address:          address,
        Port:             s.Port,
        Tags:             s.Tags,
        Meta:             s.Meta,
        Checks:           s.Checks,
        DeregisterOnExit: dereg,
    }
}

func defaultIfEmpty(s, def string) string { if s == "" { return def }; return s }

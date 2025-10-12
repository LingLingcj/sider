package agent

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "os"
    "os/exec"
    "strings"
    "strconv"
    "time"

    "sider/internal/api"
)

// Config 为单个服务实例的 Agent 配置。
// 与最初单服务/TTL 的硬编码方式相比，现支持：
// - 多种检查类型（ttl/http/tcp/cmd）
// - 自定义检查间隔与超时
// - 进程退出时可选自动注销
type Config struct {
    ServerHTTP       string        // 例如 http://127.0.0.1:8500
    Namespace        string        // 命名空间
    Service          string        // 服务名
    ID               string        // 实例 ID（留空将自动生成）
    Address          string        // 对外地址
    Port             int           // 服务端口
    Tags             []string      // 标签
    Meta             map[string]string
    TTL              time.Duration // 兼容旧参数：若 >0 且未在 Checks 中显式声明 TTL，则自动添加
    Checks           []api.CheckDef
    DeregisterOnExit bool // 退出时调用服务端注销接口
}

type Agent struct {
    cfg          Config
    client       *http.Client
    checkIDs     []string
    loopCancels  []context.CancelFunc // 各检查循环的取消函数
}

func New(cfg Config) *Agent {
    return &Agent{cfg: cfg, client: &http.Client{Timeout: 5 * time.Second}}
}

// Run 启动注册并按配置执行健康检查循环；直到 ctx 取消。
func (a *Agent) Run(ctx context.Context) error {
    if a.cfg.ServerHTTP == "" {
        return errors.New("missing ServerHTTP")
    }
    if a.cfg.Namespace == "" || a.cfg.Service == "" {
        return errors.New("missing Namespace/Service")
    }
    if a.cfg.ID == "" {
        host, _ := os.Hostname()
        a.cfg.ID = fmt.Sprintf("%s-%s-%d", a.cfg.Service, host, a.cfg.Port)
    }

    // 若未显式声明 TTL 检查但保留了旧 TTL 参数，则补上一条 TTL 检查。
    hasTTL := false
    for _, c := range a.cfg.Checks {
        if strings.EqualFold(c.Type, string("ttl")) {
            hasTTL = true
            break
        }
    }
    if !hasTTL && a.cfg.TTL > 0 {
        a.cfg.Checks = append(a.cfg.Checks, api.CheckDef{Type: "ttl", TTL: a.cfg.TTL.String()})
    }

    if err := a.register(ctx); err != nil {
        return err
    }

    // 按检查类型启动对应循环
    a.startCheckLoops(ctx)

    <-ctx.Done()
    // 结束各检查循环
    for _, cancel := range a.loopCancels {
        cancel()
    }
    if a.cfg.DeregisterOnExit {
        _ = a.deregister(context.Background())
    }
    return nil
}

// register 将实例及检查注册到服务端。
func (a *Agent) register(ctx context.Context) error {
    req := api.RegisterServiceRequest{
        Name:      a.cfg.Service,
        Namespace: a.cfg.Namespace,
        ID:        a.cfg.ID,
        Address:   a.cfg.Address,
        Port:      a.cfg.Port,
        Tags:      a.cfg.Tags,
        Meta:      mergeStringMap(map[string]string{"agent": "sider"}, a.cfg.Meta),
        Checks:    a.cfg.Checks,
    }
    body, _ := json.Marshal(req)
    url := fmt.Sprintf("%s/v1/agent/service/register", stringsTrimTrailingSlash(a.cfg.ServerHTTP))
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
    httpReq.Header.Set("Content-Type", "application/json")
    resp, err := a.client.Do(httpReq)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("register failed: %s", string(b))
    }
    var rr api.RegisterResponse
    _ = json.NewDecoder(resp.Body).Decode(&rr)
    a.checkIDs = rr.CheckIDs
    log.Printf("已注册实例 %s (checks=%v) index=%d", rr.InstanceID, rr.CheckIDs, rr.Index)
    return nil
}

// deregister 注销当前实例（可选）。
func (a *Agent) deregister(ctx context.Context) error {
    if a.cfg.ID == "" {
        return nil
    }
    url := fmt.Sprintf("%s/v1/agent/service/deregister/%s?ns=%s&service=%s",
        stringsTrimTrailingSlash(a.cfg.ServerHTTP), a.cfg.ID, a.cfg.Namespace, a.cfg.Service)
    req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
    resp, err := a.client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("deregister failed: %s", string(b))
    }
    return nil
}

// startCheckLoops 根据返回的 checkIDs 与定义的 Checks 启动对应的循环。
func (a *Agent) startCheckLoops(ctx context.Context) {
    // 按请求顺序返回的 CheckIDs 与 a.cfg.Checks 一一对应。
    for i, def := range a.cfg.Checks {
        if i >= len(a.checkIDs) {
            break
        }
        cid := a.checkIDs[i]
        // 为每个检查单独派生一个可取消的 ctx，以便优雅停止。
        cctx, cancel := context.WithCancel(ctx)
        a.loopCancels = append(a.loopCancels, cancel)
        switch strings.ToLower(def.Type) {
        case "ttl":
            // TTL：定时续约（2/3 TTL）。若入参未指定 TTL 字符串，回退至 a.cfg.TTL。
            ttl := a.cfg.TTL
            if def.TTL != "" {
                if d, err := time.ParseDuration(def.TTL); err == nil && d > 0 {
                    ttl = d
                }
            }
            if ttl <= 0 {
                ttl = 15 * time.Second
            }
            go a.renewLoop(cctx, cid, ttl)
        case "http":
            go a.httpCheckLoop(cctx, cid, def)
        case "tcp":
            go a.tcpCheckLoop(cctx, cid, def)
        case "cmd":
            go a.cmdCheckLoop(cctx, cid, def)
        default:
            log.Printf("未知检查类型: %s（忽略）", def.Type)
            cancel()
        }
    }
}

// 以 TTL 的 2/3 为续约间隔
func (a *Agent) renewLoop(ctx context.Context, checkID string, ttl time.Duration) {
    interval := ttl * 2 / 3
    if interval < time.Second {
        interval = time.Second
    }
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            if err := a.renewOnce(ctx, checkID); err != nil {
                log.Printf("TTL 续约失败: %v", err)
            }
        case <-ctx.Done():
            return
        }
    }
}

func (a *Agent) renewOnce(ctx context.Context, checkID string) error {
    url := fmt.Sprintf("%s/v1/agent/check/pass/%s", stringsTrimTrailingSlash(a.cfg.ServerHTTP), checkID)
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
    resp, err := a.client.Do(httpReq)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("renew failed: %s", string(b))
    }
    return nil
}

// httpCheckLoop：按间隔请求指定 URL，依据 HTTP 状态码上报检查结果。
func (a *Agent) httpCheckLoop(ctx context.Context, checkID string, def api.CheckDef) {
    interval := parseDurationDefault(def.Interval, 10*time.Second)
    timeout := parseDurationDefault(def.Timeout, 3*time.Second)
    url := def.Path
    if url == "" {
        // 缺省使用实例地址/端口的 /health
        addr := a.cfg.Address
        if addr == "" {
            addr = "127.0.0.1"
        }
        url = fmt.Sprintf("http://%s:%d/health", addr, a.cfg.Port)
    }
    client := &http.Client{Timeout: timeout}
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    // 首次立即执行一次
    a.runHTTPCheckOnce(ctx, client, url, checkID)
    for {
        select {
        case <-ticker.C:
            a.runHTTPCheckOnce(ctx, client, url, checkID)
        case <-ctx.Done():
            return
        }
    }
}

func (a *Agent) runHTTPCheckOnce(ctx context.Context, client *http.Client, url, checkID string) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    resp, err := client.Do(req)
    if err != nil {
        _ = a.reportCheck(ctx, checkID, "fail", err.Error())
        return
    }
    defer resp.Body.Close()
    // 2xx 和 3xx 视为通过，其余失败（简化逻辑）。
    if resp.StatusCode >= 200 && resp.StatusCode < 400 {
        _ = a.reportCheck(ctx, checkID, "pass", "")
    } else if resp.StatusCode >= 400 && resp.StatusCode < 500 {
        _ = a.reportCheck(ctx, checkID, "warn", fmt.Sprintf("status=%d", resp.StatusCode))
    } else {
        _ = a.reportCheck(ctx, checkID, "fail", fmt.Sprintf("status=%d", resp.StatusCode))
    }
}

// tcpCheckLoop：按间隔进行 TCP 连接测试。
func (a *Agent) tcpCheckLoop(ctx context.Context, checkID string, def api.CheckDef) {
    interval := parseDurationDefault(def.Interval, 10*time.Second)
    timeout := parseDurationDefault(def.Timeout, 3*time.Second)
    target := def.Path
    if target == "" {
        target = net.JoinHostPort(defaultString(a.cfg.Address, "127.0.0.1"), strconv.Itoa(a.cfg.Port))
    }
    dialer := &net.Dialer{Timeout: timeout}
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    run := func() {
        conn, err := dialer.DialContext(ctx, "tcp", target)
        if err != nil {
            _ = a.reportCheck(ctx, checkID, "fail", err.Error())
            return
        }
        _ = conn.Close()
        _ = a.reportCheck(ctx, checkID, "pass", "")
    }
    run()
    for {
        select {
        case <-ticker.C:
            run()
        case <-ctx.Done():
            return
        }
    }
}

// cmdCheckLoop：执行命令，0 退出码为 pass，其余 fail。
func (a *Agent) cmdCheckLoop(ctx context.Context, checkID string, def api.CheckDef) {
    interval := parseDurationDefault(def.Interval, 10*time.Second)
    // Timeout 通过 context 控制
    timeout := parseDurationDefault(def.Timeout, 5*time.Second)
    cmdline := strings.TrimSpace(def.Path)
    if cmdline == "" {
        log.Printf("cmd 检查缺少 Path，忽略")
        return
    }
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    run := func() {
        cctx, cancel := context.WithTimeout(ctx, timeout)
        defer cancel()
        // 简化解析：通过 shell 执行，避免复杂的转义与参数拆分。
        cmd := exec.CommandContext(cctx, "bash", "-lc", cmdline)
        out, err := cmd.CombinedOutput()
        if err != nil {
            // 若为超时，err 会包含 context deadline exceeded
            _ = a.reportCheck(ctx, checkID, "fail", string(out))
            return
        }
        _ = a.reportCheck(ctx, checkID, "pass", string(out))
    }
    run()
    for {
        select {
        case <-ticker.C:
            run()
        case <-ctx.Done():
            return
        }
    }
}

func (a *Agent) reportCheck(ctx context.Context, checkID string, action string, output string) error {
    // action: pass|warn|fail
    url := fmt.Sprintf("%s/v1/agent/check/%s/%s", stringsTrimTrailingSlash(a.cfg.ServerHTTP), action, checkID)
    var body io.Reader
    if output != "" {
        // 简化：将输出通过 query 忽略；当前服务器端未接收输出体，这里仅保留接口位置
        _ = output
    }
    req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
    resp, err := a.client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("report %s failed: %s", action, string(b))
    }
    return nil
}

func stringsTrimTrailingSlash(s string) string {
    for len(s) > 0 && s[len(s)-1] == '/' {
        s = s[:len(s)-1]
    }
    return s
}

func parseDurationDefault(s string, d time.Duration) time.Duration {
    if s == "" {
        return d
    }
    if v, err := time.ParseDuration(s); err == nil && v > 0 {
        return v
    }
    return d
}

func defaultString(s, def string) string {
    if s == "" {
        return def
    }
    return s
}

func mergeStringMap(a, b map[string]string) map[string]string {
    if a == nil && b == nil {
        return nil
    }
    out := make(map[string]string)
    for k, v := range a {
        out[k] = v
    }
    for k, v := range b {
        out[k] = v
    }
    return out
}

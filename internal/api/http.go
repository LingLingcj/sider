package api

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "log"
    "net/http"
    "strconv"
    "strings"
    "time"

    "sider/internal/registry"
)

// HTTPServer 暴露 M1 阶段的最小 API 面。
type HTTPServer struct {
    Reg  registry.Registry
    Addr string
    srv  *http.Server
}

func (h *HTTPServer) Start(ctx context.Context) error {
    mux := http.NewServeMux()
    mux.HandleFunc("/v1/agent/service/register", h.handleRegister)
    mux.HandleFunc("/v1/agent/service/deregister/", h.handleDeregisterByPath) // 路径式注销
    mux.HandleFunc("/v1/agent/service/deregister", h.handleDeregisterJSON)    // JSON 请求体注销
    mux.HandleFunc("/v1/agent/check/pass/", h.handleCheckPass)
    mux.HandleFunc("/v1/agent/check/warn/", h.handleCheckWarn)
    mux.HandleFunc("/v1/agent/check/fail/", h.handleCheckFail)
    mux.HandleFunc("/v1/catalog/services", h.handleCatalogServices)
    mux.HandleFunc("/v1/health/service/", h.handleHealthService)

    h.srv = &http.Server{Addr: h.Addr, Handler: logRequests(mux)}

    go func() {
        <-ctx.Done()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        _ = h.srv.Shutdown(shutdownCtx)
    }()
    log.Printf("HTTP 服务启动，监听 %s", h.Addr)
    err := h.srv.ListenAndServe()
    if err != nil && !errors.Is(err, http.ErrServerClosed) {
        return err
    }
    return nil
}

func logRequests(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        next.ServeHTTP(w, r)
        log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
    })
}

func (h *HTTPServer) handleRegister(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPut && r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req RegisterServiceRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
        return
    }
    specs, err := convertCheckDefs(req.Checks)
    if err != nil {
        http.Error(w, "bad checks: "+err.Error(), http.StatusBadRequest)
        return
    }
    inst := registry.ServiceInstance{
        Namespace: req.Namespace,
        Service:   req.Name,
        ID:        req.ID,
        Address:   req.Address,
        Port:      req.Port,
        Tags:      req.Tags,
        Meta:      req.Meta,
        Weights:   registry.Weights{Passing: req.Weights.Passing, Warning: req.Weights.Warning},
    }
    idx, checkIDs, err := h.Reg.RegisterInstance(r.Context(), inst, specs)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", idx))
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(RegisterResponse{Index: idx, InstanceID: inst.ID, CheckIDs: checkIDs})
}

func (h *HTTPServer) handleDeregisterByPath(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPut && r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    // 路径: /v1/agent/service/deregister/{id}
    parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/agent/service/deregister/"), "/")
    if len(parts) < 1 || parts[0] == "" {
        http.Error(w, "missing id", http.StatusBadRequest)
        return
    }
    id := parts[0]
    ns := r.URL.Query().Get("ns")
    svc := r.URL.Query().Get("service")
    idx, err := h.Reg.DeregisterInstance(r.Context(), ns, svc, id)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", idx))
    w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) handleDeregisterJSON(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPut && r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req DeregisterRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
        return
    }
    idx, err := h.Reg.DeregisterInstance(r.Context(), req.Namespace, req.Service, req.ID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", idx))
    w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) handleCheckPass(w http.ResponseWriter, r *http.Request) {
    h.handleCheckStatus(w, r, registry.StatusPassing)
}
func (h *HTTPServer) handleCheckWarn(w http.ResponseWriter, r *http.Request) {
    h.handleCheckStatus(w, r, registry.StatusWarning)
}
func (h *HTTPServer) handleCheckFail(w http.ResponseWriter, r *http.Request) {
    // For TTL checks, pass is renew; fail transitions to critical.
    h.handleCheckStatus(w, r, registry.StatusCritical)
}

func (h *HTTPServer) handleCheckStatus(w http.ResponseWriter, r *http.Request, st registry.CheckStatus) {
    if r.Method != http.MethodPut && r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    // 路径: /v1/agent/check/{pass|warn|fail}/{check_id}
    path := r.URL.Path
    idx := strings.LastIndex(path, "/")
    if idx < 0 || idx == len(path)-1 {
        http.Error(w, "missing check id", http.StatusBadRequest)
        return
    }
    checkID := path[idx+1:]
    var newIdx uint64
    var err error
    if st == registry.StatusPassing {
        // 若为 TTL 检查则续约；否则回退到 ReportCheck
        newIdx, err = h.Reg.RenewTTL(r.Context(), checkID)
        if err != nil {
            // not a TTL check, fallback to explicit report
            newIdx, err = h.Reg.ReportCheck(r.Context(), checkID, st, "")
        }
    } else {
        newIdx, err = h.Reg.ReportCheck(r.Context(), checkID, st, "")
    }
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", newIdx))
    w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) handleCatalogServices(w http.ResponseWriter, r *http.Request) {
    ns := r.URL.Query().Get("ns")
    names, idx, err := h.Reg.ListServices(r.Context(), ns)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", idx))
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(names)
}

func (h *HTTPServer) handleHealthService(w http.ResponseWriter, r *http.Request) {
    // 路径: /v1/health/service/{name}
    name := strings.TrimPrefix(r.URL.Path, "/v1/health/service/")
    ns := r.URL.Query().Get("ns")
    passing := r.URL.Query().Get("passing")
    tag := r.URL.Query().Get("tag")
    // zone is parsed but unused in M1
    zone := r.URL.Query().Get("zone")

    // 长轮询参数
    lastIdxStr := r.URL.Query().Get("index")
    waitStr := r.URL.Query().Get("wait")
    var lastIdx uint64
    if lastIdxStr != "" {
        if v, err := strconv.ParseUint(lastIdxStr, 10, 64); err == nil {
            lastIdx = v
        }
    }
    var wait time.Duration
    if waitStr != "" {
        if d, err := time.ParseDuration(waitStr); err == nil {
            wait = d
        }
    }

    // 可选等待变更
    if wait > 0 && lastIdx > 0 {
        ctx := r.Context()
        ctx, cancel := context.WithTimeout(ctx, wait)
        defer cancel()
        curr, ch := h.Reg.WatchService(ctx, ns, name, lastIdx)
        select {
        case <-ch:
            // proceed to respond with new state
            _ = curr
        case <-ctx.Done():
            // timeout; fall through to return current state
        }
    }

    opts := registry.ListOptions{PassingOnly: passing == "1" || strings.ToLower(passing) == "true", Tag: tag, Zone: zone}
    views, idx, err := h.Reg.ListHealthyInstances(r.Context(), ns, name, opts)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("X-Index", fmt.Sprintf("%d", idx))
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(views)
}

func convertCheckDefs(defs []CheckDef) ([]registry.CheckSpec, error) {
    out := make([]registry.CheckSpec, 0, len(defs))
    for _, d := range defs {
        cs := registry.CheckSpec{Type: registry.CheckType(strings.ToLower(d.Type)), TTLRaw: d.TTL, HTTP: d.Path, IntRaw: d.Interval, TmRaw: d.Timeout}
        if d.TTL != "" {
            dur, err := time.ParseDuration(d.TTL)
            if err != nil {
                return nil, fmt.Errorf("bad TTL: %w", err)
            }
            cs.TTL = dur
        }
        if d.Interval != "" {
            dur, err := time.ParseDuration(d.Interval)
            if err != nil {
                return nil, fmt.Errorf("bad Interval: %w", err)
            }
            cs.Interval = dur
        }
        if d.Timeout != "" {
            dur, err := time.ParseDuration(d.Timeout)
            if err != nil {
                return nil, fmt.Errorf("bad Timeout: %w", err)
            }
            cs.Timeout = dur
        }
        out = append(out, cs)
    }
    return out, nil
}

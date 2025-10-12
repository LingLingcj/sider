package api

// HTTP API 的请求/响应结构体。

type RegisterServiceRequest struct {
    Name      string            `json:"Name"`
    Namespace string            `json:"Namespace"`
    ID        string            `json:"ID"`
    Address   string            `json:"Address"`
    Port      int               `json:"Port"`
    Tags      []string          `json:"Tags"`
    Meta      map[string]string `json:"Meta"`
    Checks    []CheckDef        `json:"Checks"`
    Weights   struct {
        Passing int `json:"Passing"`
        Warning int `json:"Warning"`
    } `json:"Weights"`
}

type CheckDef struct {
    Type     string `json:"Type"`     // 检查类型（ttl/http/tcp/cmd）
    TTL      string `json:"TTL"`      // 仅 ttl 检查使用
    Path     string `json:"Path"`     // http 检查使用
    Interval string `json:"Interval"` // 检查间隔
    Timeout  string `json:"Timeout"`  // 超时
}

type DeregisterRequest struct {
    Namespace string `json:"Namespace"`
    Service   string `json:"Service"`
    ID        string `json:"ID"`
}

type RegisterResponse struct {
    Index      uint64   `json:"Index"`
    InstanceID string   `json:"InstanceID"`
    CheckIDs   []string `json:"CheckIDs"`
}

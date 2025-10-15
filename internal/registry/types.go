package registry

import "time"

// 基础类型：服务/实例/健康检查。

// CheckType 表示健康检查类型。
type CheckType string

const (
	CheckTTL  CheckType = "ttl"
	CheckHTTP CheckType = "http"
	CheckTCP  CheckType = "tcp"
	CheckCmd  CheckType = "cmd"
)

// CheckStatus 表示健康检查的当前状态。
type CheckStatus int

const (
	StatusUnknown CheckStatus = iota
	StatusPassing
	StatusWarning
	StatusCritical
)

// Weights 定义负载均衡时的权重（通过或警告状态）。
type Weights struct {
	Passing int `json:"Passing"`
	Warning int `json:"Warning"`
}

// CheckSpec 定义一个健康检查的配置。
type CheckSpec struct {
	Type     CheckType     `json:"Type"`
	TTL      time.Duration `json:"-"`    // parsed from input
	TTLRaw   string        `json:"TTL"`  // original string in requests
	HTTP     string        `json:"Path"` // for HTTP check
	Interval time.Duration `json:"-"`
	IntRaw   string        `json:"Interval"`
	Timeout  time.Duration `json:"-"`
	TmRaw    string        `json:"Timeout"`
}

// Check 保存某一次健康检查的运行时状态。
type Check struct {
	ID         string
	Spec       CheckSpec
	Status     CheckStatus
	Output     string
	LastUpdate time.Time
	LastPass   time.Time
}

// ServiceInstance 描述某个服务的一个实例。
type ServiceInstance struct {
	Namespace string            `json:"Namespace"`
	Service   string            `json:"Name"`
	ID        string            `json:"ID"`
	Address   string            `json:"Address"`
	Port      int               `json:"Port"`
	Tags      []string          `json:"Tags"`
	Meta      map[string]string `json:"Meta"`
	Weights   Weights           `json:"Weights"`

	// Bookkeeping（内部索引/排序标记）
	CreateIndex uint64 `json:"-"`
	ModifyIndex uint64 `json:"-"`
}

// ListOptions 控制查询实例时的过滤条件。
type ListOptions struct {
	PassingOnly bool
	Tag         string
	Zone        string
}

// InstanceView 是返回给客户端的精简实例视图。
type InstanceView struct {
	Namespace string            `json:"Namespace"`
	Service   string            `json:"Service"`
	ID        string            `json:"ID"`
	Address   string            `json:"Address"`
	Port      int               `json:"Port"`
	Tags      []string          `json:"Tags"`
	Meta      map[string]string `json:"Meta"`
	Weights   Weights           `json:"Weights"`
}

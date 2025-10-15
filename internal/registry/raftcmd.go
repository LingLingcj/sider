package registry

import "encoding/json"

// raftcmd.go - Raft 命令和响应类型定义
// 将 Raft 日志命令的编解码逻辑集中管理，便于维护和测试。

// ============================================================================
// 命令操作类型
// ============================================================================

const (
	opRegister    = "register"
	opDeregister  = "deregister"
	opRenewTTL    = "renew_ttl"
	opReportCheck = "report_check"
)

// ============================================================================
// 命令封装（Envelope）
// ============================================================================

// commandEnvelope 是所有 Raft 命令的外层包装，包含操作类型和数据负载。
type commandEnvelope struct {
	Op   string          `json:"op"`
	Data json.RawMessage `json:"data"`
}

// ============================================================================
// 命令负载类型（Command Payloads）
// ============================================================================

// registerCommand 注册实例命令
type registerCommand struct {
	Inst  ServiceInstance `json:"inst"`
	Specs []CheckSpec     `json:"specs"`
}

// deregisterCommand 注销实例命令
type deregisterCommand struct {
	Namespace string `json:"ns"`
	Service   string `json:"svc"`
	ID        string `json:"id"`
}

// checkCommand TTL 续约和健康检查报告命令
type checkCommand struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"` // 仅用于 report_check
	Output string `json:"output,omitempty"` // 仅用于 report_check
}

// ============================================================================
// 响应类型（Response Types）
// ============================================================================

// registerResponse 注册实例响应
type registerResponse struct {
	Index    uint64   `json:"index"`
	CheckIDs []string `json:"check_ids,omitempty"`
	Err      string   `json:"err,omitempty"`
}

// indexResponse 通用索引响应（用于注销、续约、报告等）
type indexResponse struct {
	Index uint64 `json:"index"`
	Err   string `json:"err,omitempty"`
}

// ============================================================================
// 命令构建辅助函数
// ============================================================================

// buildCommand 构建命令封装并序列化为 JSON
func buildCommand(op string, data interface{}) ([]byte, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	env := commandEnvelope{Op: op, Data: payload}
	return json.Marshal(env)
}

// BuildRegisterCommand 构建注册命令
func BuildRegisterCommand(inst ServiceInstance, specs []CheckSpec) ([]byte, error) {
	return buildCommand(opRegister, registerCommand{Inst: inst, Specs: specs})
}

// BuildDeregisterCommand 构建注销命令
func BuildDeregisterCommand(namespace, service, id string) ([]byte, error) {
	return buildCommand(opDeregister, deregisterCommand{
		Namespace: namespace,
		Service:   service,
		ID:        id,
	})
}

// BuildRenewTTLCommand 构建 TTL 续约命令
func BuildRenewTTLCommand(checkID string) ([]byte, error) {
	return buildCommand(opRenewTTL, checkCommand{ID: checkID})
}

// BuildReportCheckCommand 构建健康检查报告命令
func BuildReportCheckCommand(checkID string, status CheckStatus, output string) ([]byte, error) {
	return buildCommand(opReportCheck, checkCommand{
		ID:     checkID,
		Status: statusString(status),
		Output: output,
	})
}

// ============================================================================
// 响应解析辅助函数
// ============================================================================

// ParseRegisterResponse 解析注册响应
func ParseRegisterResponse(data []byte) (index uint64, checkIDs []string, err error) {
	var resp registerResponse
	if e := json.Unmarshal(data, &resp); e != nil {
		return 0, nil, e
	}
	if resp.Err != "" {
		return resp.Index, resp.CheckIDs, errString(resp.Err)
	}
	return resp.Index, resp.CheckIDs, nil
}

// ParseIndexResponse 解析通用索引响应
func ParseIndexResponse(data []byte) (index uint64, err error) {
	var resp indexResponse
	if e := json.Unmarshal(data, &resp); e != nil {
		return 0, e
	}
	if resp.Err != "" {
		return resp.Index, errString(resp.Err)
	}
	return resp.Index, nil
}

// ============================================================================
// 内部辅助函数
// ============================================================================

// statusString 将 CheckStatus 转换为字符串
func statusString(s CheckStatus) string {
	switch s {
	case StatusPassing:
		return "pass"
	case StatusWarning:
		return "warn"
	case StatusCritical:
		return "fail"
	default:
		return "unknown"
	}
}

// parseStatus 将字符串转换为 CheckStatus
func parseStatus(s string) CheckStatus {
	switch s {
	case "pass":
		return StatusPassing
	case "warn":
		return StatusWarning
	case "fail":
		return StatusCritical
	default:
		return StatusUnknown
	}
}

// errString 是一个简单的错误类型，用于从字符串创建错误
type errString string

func (e errString) Error() string { return string(e) }

// encodeResponse 将响应编码为 JSON 字节数组
func encodeResponse(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

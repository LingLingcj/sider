package registry

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// memoryRegistry 是内存版 Registry 的实现。
// 支持：TTL 过期、全局修改索引、按服务的 Watch（用于长轮询）。
type memoryRegistry struct {
	mu sync.RWMutex

	// 使用 ns/service/id 作为键
	instances map[string]*instanceRecord

	// checkID -> checkRecord
	checks map[string]*checkRecord

	// id -> 完整键（ns/svc/id）。ID 通常全局唯一，但仍保留映射以兼容查询。
	idToKeys map[string][]string

	// 服务键 -> 该服务最新索引
	svcIndex map[string]uint64

	// 服务键 -> Watchers 列表
	watchers map[string][]chan struct{}

	// 全局索引
	index uint64

	// 过期清理的后台通道与状态
	stopCh         chan struct{}
	expirerStarted bool
}

type instanceRecord struct {
	inst   ServiceInstance
	checks []string // 检查 ID 列表
}

type checkRecord struct {
	chk Check
}

// Options 控制内存注册表的行为。
type Options struct {
	// AutoExpirer: 是否在创建时自动启动 TTL 过期清理器。
	AutoExpirer bool
}

func NewMemoryRegistry() *memoryRegistry { // 兼容旧接口，默认自动启用过期器
	return NewMemoryRegistryWithOptions(Options{AutoExpirer: true})
}

// NewMemoryRegistryWithOptions 允许控制是否自动启动过期器。
func NewMemoryRegistryWithOptions(opts Options) *memoryRegistry {
	mr := &memoryRegistry{
		instances: make(map[string]*instanceRecord),
		checks:    make(map[string]*checkRecord),
		idToKeys:  make(map[string][]string),
		svcIndex:  make(map[string]uint64),
		watchers:  make(map[string][]chan struct{}),
	}
	if opts.AutoExpirer {
		mr.StartExpirer()
	}
	return mr
}

func (m *memoryRegistry) key(namespace, service, id string) string {
	return namespace + "/" + service + "/" + id
}

func (m *memoryRegistry) svcKey(namespace, service string) string {
	return namespace + "/" + service
}

func (m *memoryRegistry) nextIndexLocked(svc string) uint64 {
	m.index++
	m.svcIndex[svc] = m.index
	// 通知该服务的所有 Watchers
	if lst, ok := m.watchers[svc]; ok && len(lst) > 0 {
		for _, ch := range lst {
			select {
			case ch <- struct{}{}:
			default:
				// 已通知过，跳过
			}
			close(ch)
		}
		m.watchers[svc] = nil
	}
	return m.index
}

func (m *memoryRegistry) RegisterInstance(ctx context.Context, inst ServiceInstance, specs []CheckSpec) (uint64, []string, error) {
	if inst.Namespace == "" || inst.Service == "" || inst.ID == "" {
		return 0, nil, errors.New("missing Namespace/Service/ID")
	}
	svc := m.svcKey(inst.Namespace, inst.Service)
	k := m.key(inst.Namespace, inst.Service, inst.ID)

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.instances[k]; exists {
		// 若实例已存在：仅更新元信息；检查集合保持不变（M1 简化）。
		rec := m.instances[k]
		inst.CreateIndex = rec.inst.CreateIndex
		inst.ModifyIndex = m.index + 1
		rec.inst = inst
		idx := m.nextIndexLocked(svc)
		return idx, nil, nil
	}

	inst.CreateIndex = m.index + 1
	inst.ModifyIndex = inst.CreateIndex
	rec := &instanceRecord{inst: inst}

	// 创建健康检查
	var checkIDs []string
	for i, s := range specs {
		cid := "chk:" + inst.ID + ":" + itoa(i)
		spec := s
		// 归一化时长：若上游解析失败则为 0（等同未启用）。
		// TTL 检查在首次续约前标记为 critical。
		chk := Check{ID: cid, Spec: spec, Status: StatusUnknown, LastUpdate: now}
		if spec.Type == CheckTTL {
			// 尚未续约
			chk.Status = StatusCritical
		}
		m.checks[cid] = &checkRecord{chk: chk}
		rec.checks = append(rec.checks, cid)
		checkIDs = append(checkIDs, cid)
	}

	m.instances[k] = rec
	m.idToKeys[inst.ID] = append(m.idToKeys[inst.ID], k)

	idx := m.nextIndexLocked(svc)
	return idx, checkIDs, nil
}

func (m *memoryRegistry) DeregisterInstance(ctx context.Context, namespace, service, id string) (uint64, error) {
	if id == "" {
		return 0, errors.New("missing id")
	}
	svc := m.svcKey(namespace, service)
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string
	if namespace != "" && service != "" {
		keys = []string{m.key(namespace, service, id)}
	} else {
		keys = append(keys, m.idToKeys[id]...)
	}
	if len(keys) == 0 {
		return m.index, errors.New("instance not found")
	}
	for _, k := range keys {
		rec, ok := m.instances[k]
		if !ok {
			continue
		}
		// 删除其下的所有检查
		for _, cid := range rec.checks {
			delete(m.checks, cid)
		}
		delete(m.instances, k)
	}
	// 清理 id 索引
	delete(m.idToKeys, id)
	idx := m.nextIndexLocked(svc)
	return idx, nil
}

func (m *memoryRegistry) RenewTTL(ctx context.Context, checkID string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cr, ok := m.checks[checkID]
	if !ok {
		return m.index, errors.New("check not found")
	}
	if cr.chk.Spec.Type != CheckTTL {
		return m.index, errors.New("not a ttl check")
	}
	cr.chk.Status = StatusPassing
	cr.chk.LastPass = time.Now()
	cr.chk.LastUpdate = cr.chk.LastPass

	// 找到所属服务，发送通知
	svc := m.findSvcKeyByCheckLocked(checkID)
	idx := m.nextIndexLocked(svc)
	return idx, nil
}

func (m *memoryRegistry) ReportCheck(ctx context.Context, checkID string, status CheckStatus, output string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cr, ok := m.checks[checkID]
	if !ok {
		return m.index, errors.New("check not found")
	}
	cr.chk.Status = status
	cr.chk.Output = output
	cr.chk.LastUpdate = time.Now()
	svc := m.findSvcKeyByCheckLocked(checkID)
	idx := m.nextIndexLocked(svc)
	return idx, nil
}

func (m *memoryRegistry) ListHealthyInstances(ctx context.Context, namespace, service string, opts ListOptions) ([]InstanceView, uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []InstanceView
	svc := m.svcKey(namespace, service)
	prefix := namespace + "/" + service + "/"
	for k, rec := range m.instances {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if opts.PassingOnly {
			if aggStatus := m.aggregateStatusLocked(rec); aggStatus != StatusPassing {
				continue
			}
		}
		out = append(out, InstanceView{
			Namespace: rec.inst.Namespace,
			Service:   rec.inst.Service,
			ID:        rec.inst.ID,
			Address:   rec.inst.Address,
			Port:      rec.inst.Port,
			Tags:      append([]string(nil), rec.inst.Tags...),
			Meta:      cloneMap(rec.inst.Meta),
			Weights:   rec.inst.Weights,
		})
	}
	// 稳定排序，确保输出一致
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	idx := m.svcIndex[svc]
	if idx == 0 {
		idx = m.index
	}
	return out, idx, nil
}

func (m *memoryRegistry) ListServices(ctx context.Context, namespace string) ([]string, uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]struct{})
	var names []string
	prefix := namespace + "/"
	for k := range m.instances {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, m.index, nil
}

func (m *memoryRegistry) WatchService(ctx context.Context, namespace, service string, lastIndex uint64) (uint64, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	svc := m.svcKey(namespace, service)
	curr := m.svcIndex[svc]
	ch := make(chan struct{}, 1)
	if curr > lastIndex {
		ch <- struct{}{}
		close(ch)
		return curr, ch
	}
	m.watchers[svc] = append(m.watchers[svc], ch)
	return curr, ch
}

// --- 内部方法 ---

func (m *memoryRegistry) aggregateStatusLocked(rec *instanceRecord) CheckStatus {
	// 聚合规则：以“最坏状态”为准；若无检查，视为 Passing。
	if len(rec.checks) == 0 {
		return StatusPassing
	}
	agg := StatusPassing
	for _, cid := range rec.checks {
		if cr, ok := m.checks[cid]; ok {
			switch cr.chk.Status {
			case StatusPassing:
				// ok
			case StatusWarning:
				if agg == StatusPassing {
					agg = StatusWarning
				}
			case StatusUnknown:
				if agg == StatusPassing || agg == StatusWarning {
					agg = StatusUnknown
				}
			case StatusCritical:
				return StatusCritical
			}
		}
	}
	return agg
}

func (m *memoryRegistry) findSvcKeyByCheckLocked(checkID string) string {
	// 遍历实例找到所属服务。时间复杂度 O(n)，对 M1 足够。
	for k, rec := range m.instances {
		for _, cid := range rec.checks {
			if cid == checkID {
				// k 形如 ns/svc/id；取前两段
				parts := strings.Split(k, "/")
				if len(parts) >= 2 {
					return parts[0] + "/" + parts[1]
				}
			}
		}
	}
	return ""
}

func (m *memoryRegistry) expirer() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.expireOnce()
		case <-m.stopCh:
			return
		}
	}
}

func (m *memoryRegistry) expireOnce() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// 记录哪些服务发生了变化
	changedSvc := make(map[string]bool)
	for _, cr := range m.checks {
		if cr.chk.Spec.Type != CheckTTL {
			continue
		}
		ttl := cr.chk.Spec.TTL
		if ttl <= 0 {
			continue
		}
		if cr.chk.LastPass.IsZero() {
			continue
		}
		if now.Sub(cr.chk.LastPass) > ttl {
			if cr.chk.Status != StatusCritical {
				cr.chk.Status = StatusCritical
				cr.chk.LastUpdate = now
				svc := m.findSvcKeyByCheckLocked(cr.chk.ID)
				if svc != "" {
					changedSvc[svc] = true
				}
			}
		}
	}
	for svc := range changedSvc {
		m.nextIndexLocked(svc)
	}
}

// StartExpirer 启动 TTL 过期清理器（若尚未启动）。
func (m *memoryRegistry) StartExpirer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expirerStarted {
		return
	}
	m.stopCh = make(chan struct{})
	m.expirerStarted = true
	go m.expirer()
}

// Stop 停止 TTL 过期清理器（若正在运行）。
func (m *memoryRegistry) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expirerStarted {
		close(m.stopCh)
		m.expirerStarted = false
	}
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// itoa：简单的整数转字符串，避免引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

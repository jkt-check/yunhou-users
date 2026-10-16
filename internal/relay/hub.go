package relay

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Conn 是一条已注册的连接(Hello 完成之后)。Task 5 的 wsConn 与测试
// fake 都实现此接口。Enqueue 必须非阻塞:出站缓冲满时由实现方内部触发
// 该连接的 slow_consumer 关闭;Hub 永远不阻塞在 Enqueue 上。
type Conn interface {
	UserID() string
	GetRole() Role
	DeviceID() string // device 角色有效
	ClientID() string // client 角色有效
	DeviceMeta() DeviceInfo
	Enqueue(frame []byte)
}

// Options 汇总全部时序/限额参数(spec §8),测试注入短值。
type Options struct {
	MaxFrameBytes      int           // 256 << 10
	MaxFramesPerSec    float64       // 100
	OutboundBuffer     int           // 256
	MaxDevicesPerUser  int           // 10
	MaxClientsPerUser  int           // 20
	HelloTimeout       time.Duration // 10s
	PingInterval       time.Duration // 30s
	ReadIdleTimeout    time.Duration // 90s(经 ping/pong 探测,见 conn.go)
	TicketExpiringLead time.Duration // 60s:exp 前多久发 ticket_expiring
	RenewGrace         time.Duration // 60s:exp 后未 renew 的宽限
	TicketRetryAfterMS int           // 30000
	MaxIDLen           int           // 128:device_id/client_id 长度上限
}

// DefaultOptions 返回 spec §8 的正式值。
func DefaultOptions() Options {
	return Options{
		MaxFrameBytes:      256 << 10,
		MaxFramesPerSec:    100,
		OutboundBuffer:     256,
		MaxDevicesPerUser:  10,
		MaxClientsPerUser:  20,
		HelloTimeout:       10 * time.Second,
		PingInterval:       30 * time.Second,
		ReadIdleTimeout:    90 * time.Second,
		TicketExpiringLead: 60 * time.Second,
		RenewGrace:         60 * time.Second,
		TicketRetryAfterMS: 30000,
		MaxIDLen:           128,
	}
}

// RejectError 是 Register 的拒绝原因(连接数超限 / 停机中)。
type RejectError struct{ Reason CloseReason }

func (e *RejectError) Error() string { return "relay: connection rejected: " + string(e.Reason) }

type room struct {
	devices map[string]Conn
	clients map[string]Conn
}

// Hub 是单实例内存房间路由中心(spec §5.1)。不解析业务载荷。
// metrics 为 nil 时全部指标记录为 no-op;warn 恒存在,节流异常日志。
type Hub struct {
	opts     Options
	mu       sync.RWMutex
	rooms    map[string]*room
	shutdown atomic.Bool
	metrics  *Metrics
	warn     *warnThrottled
}

func NewHub(opts Options, m *Metrics) *Hub {
	return &Hub{opts: opts, rooms: make(map[string]*room), metrics: m, warn: newWarnThrottled()}
}

func (h *Hub) Options() Options { return h.opts }

func (h *Hub) ShutdownStarted() bool { return h.shutdown.Load() }

// Register 把连接注册进 user 的房间;同角色同 id 踢旧连(给旧连发
// closed replaced,关闭动作由旧连自己的 Enqueue 实现触发)。
// 替换已有同 id 连接不计入限额。返回被踢的旧连(无则 nil);
// 拒绝时返回 *RejectError。
func (h *Hub) Register(c Conn) (Conn, error) {
	if h.ShutdownStarted() {
		return nil, &RejectError{Reason: ReasonShutdown}
	}

	h.mu.Lock()
	// 持锁复查:Shutdown 在同一互斥锁下快照房间,此处复查可消除
	// 标志检查与插入之间的 TOCTOU 窗口(停机后注册的连接必然被拒)。
	if h.shutdown.Load() {
		h.mu.Unlock()
		return nil, &RejectError{Reason: ReasonShutdown}
	}
	r := h.rooms[c.UserID()]
	if r == nil {
		r = &room{devices: make(map[string]Conn), clients: make(map[string]Conn)}
		h.rooms[c.UserID()] = r
	}

	var kicked Conn
	switch c.GetRole() {
	case RoleDevice:
		if _, exists := r.devices[c.DeviceID()]; !exists && len(r.devices) >= h.opts.MaxDevicesPerUser {
			h.mu.Unlock()
			return nil, &RejectError{Reason: ReasonProtocol}
		}
		kicked = r.devices[c.DeviceID()]
		r.devices[c.DeviceID()] = c
	case RoleClient:
		if _, exists := r.clients[c.ClientID()]; !exists && len(r.clients) >= h.opts.MaxClientsPerUser {
			h.mu.Unlock()
			return nil, &RejectError{Reason: ReasonProtocol}
		}
		kicked = r.clients[c.ClientID()]
		r.clients[c.ClientID()] = c
	}

	// presence 广播与入会快照(device 上线才广播)的收集与 Enqueue 全部放在
	// 持锁内进行。Enqueue 契约非阻塞(见 Conn 注释,wsConn 用 select/default
	// 且关闭路径异步,不会同步重入 h.mu);持锁发送可消除「快照收集后、入队前
	// 目标恰好并发 Unregister」的窗口——否则离场的 presence offline 可能排在
	// 快照 online 之前进入新连接的出站缓冲,给新入会 device 留下永不纠正的
	// 陈旧在线状态。relay delta 2026-10-16 改动 3:presence 投递范围由
	// 「房间内全部 client」扩为「全部 client + 全部其他 device(不含事件
	// 主体自身)」;同时向新入会 device 逐台分帧补发其他在线 device 的
	// presence online 快照(wire 形状与常规 presence 完全一致,零协议变更)。
	// 快照帧在 Register 内 Enqueue,先于 conn 层 writer goroutine 启动,
	// 而 hello_ok 走 sendDirect 同步写——因此对端线上顺序恒为
	// hello_ok → 快照帧,满足「HelloOk 之后补发」。
	// 注意:持锁内 Enqueue 触发的 slow_consumer 路径会同步走 warn.Logf
	// (stderr,节流 1/min/key);日志实现不得改为阻塞式 writer,否则
	// Register 会拖住整个 hub。
	if c.GetRole() == RoleDevice {
		meta := c.DeviceMeta()
		frame := PresenceFrame(c.DeviceID(), true, &meta)
		var snapshot []DeviceInfo
		for _, cl := range r.clients {
			cl.Enqueue(frame)
		}
		for _, d := range r.devices {
			if d == c {
				continue // 事件主体自身不收自己的 presence/快照
			}
			d.Enqueue(frame)
			snapshot = append(snapshot, d.DeviceMeta())
		}
		// map 迭代序随机;排序保证快照帧序稳定(逐台分帧按 device_id 升序)。
		sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].DeviceID < snapshot[j].DeviceID })
		// 入会快照:向新 device 逐台补发其他在线 device 的 presence online。
		for i := range snapshot {
			m := snapshot[i]
			c.Enqueue(PresenceFrame(m.DeviceID, true, &m))
		}
	}
	roomCount := len(h.rooms)
	h.mu.Unlock()

	// 指标:被踢旧连的房间成员资格在替换时结束(其后的 Unregister 因
	// removed=false 不再扣减,connections gauge 保持配平)。
	if kicked != nil && kicked != c {
		h.metrics.connUnregistered(kicked.GetRole(), connAge(kicked))
	}
	h.metrics.connRegistered(c.GetRole())
	h.metrics.roomsActiveSet(roomCount)

	if kicked != nil && kicked != c {
		kicked.Enqueue(ClosedFrame(ReasonReplaced))
	}
	return kicked, nil
}

// Unregister 摘除连接;device 断开往 room 内全部 client + 全部其他 device
// 广播 presence offline(LWT,relay delta 2026-10-16 改动 3;事件主体已从
// map 摘除,天然不含自身)。仅当 map 里存的仍是该连接时才摘除(防旧连踢掉新连)。
func (h *Hub) Unregister(c Conn) {
	h.mu.Lock()
	r := h.rooms[c.UserID()]
	if r == nil {
		h.mu.Unlock()
		return
	}
	removed := false
	if c.GetRole() == RoleDevice {
		if cur, ok := r.devices[c.DeviceID()]; ok && cur == c {
			delete(r.devices, c.DeviceID())
			removed = true
		}
	} else {
		if cur, ok := r.clients[c.ClientID()]; ok && cur == c {
			delete(r.clients, c.ClientID())
			removed = true
		}
	}
	var presenceTargets []Conn
	if removed && c.GetRole() == RoleDevice {
		for _, cl := range r.clients {
			presenceTargets = append(presenceTargets, cl)
		}
		for _, d := range r.devices {
			presenceTargets = append(presenceTargets, d)
		}
	}
	if len(r.devices) == 0 && len(r.clients) == 0 {
		delete(h.rooms, c.UserID())
	}
	roomCount := len(h.rooms)
	h.mu.Unlock()

	if removed {
		h.metrics.connUnregistered(c.GetRole(), connAge(c))
	}
	h.metrics.roomsActiveSet(roomCount)

	if removed && c.GetRole() == RoleDevice {
		frame := PresenceFrame(c.DeviceID(), false, nil)
		for _, t := range presenceTargets {
			t.Enqueue(frame)
		}
	}
}

// OnlineDevices 返回 user 房间当前在线 device 全量表(hello_ok 数据源)。
// 按 device_id 排序保证输出稳定。
func (h *Hub) OnlineDevices(userID string) []DeviceInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[userID]
	if r == nil {
		return nil
	}
	out := make([]DeviceInfo, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d.DeviceMeta())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

// RouteApp 按 spec §6 + relay delta 2026-10-16 路由业务帧:
//   - device 无 target → 广播给 room 内全部 client(附盖章 from_device_id)
//   - device 带非空 target(改动 2)→ 只定向投给同房在线的目标 device
//     (盖章 from_device_id=发送方、target_device_id=收件人,不广播 client);
//     目标离线/不存在/跨房/为发送方自身 → 沿用 undeliverable 语义只回发送方,
//     不泄露存在性(自身定向按 undeliverable 处理,避免回声帧干扰对端
//     握手状态机)
//   - client → 指定 device(附盖章 from_client_id);不在线 → 仅回发送方
//     undeliverable;跨 room 目标同样 undeliverable,不泄露存在性。
//
// 改动 1(发送方身份盖章):所有出站副本的 sender 字段全部由本函数按入站
// 连接的认证身份重新构造——发送方在信封上自填的 from_device_id /
// from_client_id 从未被解析(conn.go 的 AppFrame 解码结构不含这两个字段),
// 天然是「覆写」而非「透传」;发送方不填 sender 时同样盖为正确值。
func (h *Hub) RouteApp(from Conn, targetDeviceID string, payload json.RawMessage) {
	h.metrics.frameIn(from.GetRole())
	h.mu.RLock()
	r := h.rooms[from.UserID()]
	switch from.GetRole() {
	case RoleDevice:
		if r == nil {
			h.mu.RUnlock()
			return
		}
		if targetDeviceID != "" {
			dev := r.devices[targetDeviceID]
			h.mu.RUnlock()
			// dev == from:目标为发送方自身,按 undeliverable 处理,
			// 不向发送方回声自己的帧(防对端握手状态机混淆)。
			if dev == nil || dev == from {
				h.metrics.undeliverableInc()
				from.Enqueue(UndeliverableFrame(targetDeviceID))
				return
			}
			dev.Enqueue(AppFromDeviceDirectedFrame(from.DeviceID(), targetDeviceID, payload))
			h.metrics.frameOut(RoleDevice)
			return
		}
		targets := make([]Conn, 0, len(r.clients))
		for _, cl := range r.clients {
			targets = append(targets, cl)
		}
		h.mu.RUnlock()
		frame := AppFromDeviceFrame(from.DeviceID(), payload)
		for _, cl := range targets {
			cl.Enqueue(frame)
			h.metrics.frameOut(RoleClient)
		}
	case RoleClient:
		var dev Conn
		if r != nil {
			dev = r.devices[targetDeviceID]
		}
		h.mu.RUnlock()
		if dev == nil {
			h.metrics.undeliverableInc()
			from.Enqueue(UndeliverableFrame(targetDeviceID))
			return
		}
		dev.Enqueue(AppFromClientFrame(from.ClientID(), payload))
		h.metrics.frameOut(RoleDevice)
	}
}

// Shutdown 停止接受新连接,先对每个 device 向其 room 的 client + 其他
// device 广播 presence offline(relay delta 2026-10-16 改动 3;停机也触发
// presence,spec §5.3),再给全部连接发 closed shutdown。顺序不能反过来:
// wsConn.Enqueue 对 closed 帧会同步置 closing 标志,之后入队的 presence
// 帧会被直接丢弃。
// 随后等待连接收尾:wsConn 收到 closed 帧后 flush 并 Unregister,
// 这里轮询房间清空即返回;wait 为等待上限(≤0 表示不等待,立即返回)。
func (h *Hub) Shutdown(wait time.Duration) {
	h.shutdown.Store(true)

	h.mu.Lock()
	var conns []Conn
	type devBroadcast struct {
		deviceID string
		targets  []Conn
	}
	var broadcasts []devBroadcast
	for _, r := range h.rooms {
		for _, d := range r.devices {
			conns = append(conns, d)
			b := devBroadcast{deviceID: d.DeviceID()}
			for _, c := range r.clients {
				b.targets = append(b.targets, c)
			}
			for _, od := range r.devices {
				if od == d {
					continue // 事件主体自身不收自己的 presence
				}
				b.targets = append(b.targets, od)
			}
			broadcasts = append(broadcasts, b)
		}
		for _, c := range r.clients {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()

	// presence 先行:此时各连接尚未置 closing,帧能进入出站缓冲;
	// 随后的 closed 帧触发统一关闭路径,writer 按序 flush(presence
	// 先于 closed 到达对端)。
	for _, b := range broadcasts {
		frame := PresenceFrame(b.deviceID, false, nil)
		for _, c := range b.targets {
			c.Enqueue(frame)
		}
	}
	closedFrame := ClosedFrame(ReasonShutdown)
	for _, c := range conns {
		c.Enqueue(closedFrame)
	}

	if wait <= 0 {
		return
	}
	deadline := time.Now().Add(wait)
	for {
		h.mu.RLock()
		empty := len(h.rooms) == 0
		h.mu.RUnlock()
		if empty || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

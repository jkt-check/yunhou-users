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
type Hub struct {
	opts     Options
	mu       sync.RWMutex
	rooms    map[string]*room
	shutdown atomic.Bool
}

func NewHub(opts Options) *Hub {
	return &Hub{opts: opts, rooms: make(map[string]*room)}
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

	// 收集 presence 广播目标(device 上线才广播);发送在解锁后进行
	var clients []Conn
	if c.GetRole() == RoleDevice {
		for _, cl := range r.clients {
			clients = append(clients, cl)
		}
	}
	h.mu.Unlock()

	if kicked != nil && kicked != c {
		kicked.Enqueue(ClosedFrame(ReasonReplaced))
	}
	if c.GetRole() == RoleDevice {
		meta := c.DeviceMeta()
		frame := PresenceFrame(c.DeviceID(), true, &meta)
		for _, cl := range clients {
			cl.Enqueue(frame)
		}
	}
	return kicked, nil
}

// Unregister 摘除连接;device 断开往 room 内全部 client 广播 presence
// offline。仅当 map 里存的仍是该连接时才摘除(防旧连踢掉新连)。
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
	var clients []Conn
	if removed && c.GetRole() == RoleDevice {
		for _, cl := range r.clients {
			clients = append(clients, cl)
		}
	}
	if len(r.devices) == 0 && len(r.clients) == 0 {
		delete(h.rooms, c.UserID())
	}
	h.mu.Unlock()

	if removed && c.GetRole() == RoleDevice {
		frame := PresenceFrame(c.DeviceID(), false, nil)
		for _, cl := range clients {
			cl.Enqueue(frame)
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

// RouteApp 按 spec §6 路由业务帧:
//   - device → 广播给 room 内全部 client(附 from_device_id)
//   - client → 指定 device(附 from_client_id);不在线 → 仅回发送方
//     undeliverable;跨 room 目标同样 undeliverable,不泄露存在性。
func (h *Hub) RouteApp(from Conn, targetDeviceID string, payload json.RawMessage) {
	h.mu.RLock()
	r := h.rooms[from.UserID()]
	switch from.GetRole() {
	case RoleDevice:
		if r == nil {
			h.mu.RUnlock()
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
		}
	case RoleClient:
		var dev Conn
		if r != nil {
			dev = r.devices[targetDeviceID]
		}
		h.mu.RUnlock()
		if dev == nil {
			from.Enqueue(UndeliverableFrame(targetDeviceID))
			return
		}
		dev.Enqueue(AppFromClientFrame(from.ClientID(), payload))
	}
}

// Shutdown 停止接受新连接,给全部连接发 closed shutdown,并对每个
// device 向其 room 的 client 广播 presence offline(停机也触发
// presence,spec §5.3)。连接的实际关闭与出站 flush 等待由各 conn 的
// 实现完成(conn 层职责);wait 仅保留在签名中供 Task 7 兼容。
func (h *Hub) Shutdown(wait time.Duration) {
	h.shutdown.Store(true)

	h.mu.Lock()
	var conns []Conn
	type devBroadcast struct {
		deviceID string
		clients  []Conn
	}
	var broadcasts []devBroadcast
	for _, r := range h.rooms {
		for _, d := range r.devices {
			conns = append(conns, d)
			b := devBroadcast{deviceID: d.DeviceID()}
			for _, c := range r.clients {
				b.clients = append(b.clients, c)
			}
			broadcasts = append(broadcasts, b)
		}
		for _, c := range r.clients {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()

	closedFrame := ClosedFrame(ReasonShutdown)
	for _, c := range conns {
		c.Enqueue(closedFrame)
	}
	for _, b := range broadcasts {
		frame := PresenceFrame(b.deviceID, false, nil)
		for _, c := range b.clients {
			c.Enqueue(frame)
		}
	}
}

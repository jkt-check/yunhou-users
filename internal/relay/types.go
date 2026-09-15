package relay

import "encoding/json"

// ProtocolVersion 是当前信封协议版本(spec §6)。不认识的 v → closed protocol。
const ProtocolVersion = 1

type Role string

const (
	RoleDevice Role = "device"
	RoleClient Role = "client"
)

// CloseReason 是 closed 帧的 reason 枚举(spec §6.2)。
type CloseReason string

const (
	ReasonAuth         CloseReason = "auth"
	ReasonEntitlement  CloseReason = "entitlement" // v1 保留不触发(已决事项 2)
	ReasonReplaced     CloseReason = "replaced"
	ReasonShutdown     CloseReason = "shutdown"
	ReasonSlowConsumer CloseReason = "slow_consumer"
	ReasonProtocol     CloseReason = "protocol"
	ReasonIdleTimeout  CloseReason = "idle_timeout"
)

// ---- 入站帧(连接方 → relay)----

// HelloFrame 必须为首帧。device 必填 DeviceID/DeviceName/AppVersion;
// client 必填 ClientID。
type HelloFrame struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Ticket     string `json:"ticket"`
	Role       Role   `json:"role"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	ClientName string `json:"client_name,omitempty"`
}

type RenewFrame struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Ticket string `json:"ticket"`
}

// AppFrame 是业务帧;Payload 对 relay 不透明(不解析、不记录)。
type AppFrame struct {
	V              int             `json:"v"`
	Type           string          `json:"type"`
	TargetDeviceID string          `json:"target_device_id,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// typePeek 只解析信封外层,用于按 type 分发。
type typePeek struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// PeekType 解析帧的 v/type;坏 JSON 返回错误。
func PeekType(frame []byte) (v int, typ string, err error) {
	var p typePeek
	if err := json.Unmarshal(frame, &p); err != nil {
		return 0, "", err
	}
	return p.V, p.Type, nil
}

// ---- 出站帧(relay → 连接方)----

type DeviceInfo struct {
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	ConnectedAt int64  `json:"connected_at"`
}

func marshalFrame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil { // 构造器入参全部受控,不可达;防御性兜底
		return []byte(`{"v":1,"type":"closed","reason":"protocol"}`)
	}
	return b
}

func HelloOKFrame(devices []DeviceInfo, serverTime int64) []byte {
	if devices == nil {
		devices = []DeviceInfo{}
	}
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "hello_ok",
		"room_devices": devices, "server_time": serverTime,
	})
}

// PresenceFrame:online=true 时 meta 必填;false 时 meta 省略(spec §6.2)。
func PresenceFrame(deviceID string, online bool, meta *DeviceInfo) []byte {
	f := map[string]any{
		"v": ProtocolVersion, "type": "presence",
		"device_id": deviceID, "online": online,
	}
	if online && meta != nil {
		f["meta"] = map[string]any{
			"device_name": meta.DeviceName, "app_version": meta.AppVersion,
		}
	}
	return marshalFrame(f)
}

func TicketExpiringFrame(retryAfterMS int) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "ticket_expiring", "retry_after_ms": retryAfterMS,
	})
}

func AppFromDeviceFrame(deviceID string, payload json.RawMessage) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "app",
		"from_device_id": deviceID, "payload": payload,
	})
}

func AppFromClientFrame(clientID string, payload json.RawMessage) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "app",
		"from_client_id": clientID, "payload": payload,
	})
}

func UndeliverableFrame(targetDeviceID string) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "undeliverable", "target_device_id": targetDeviceID,
	})
}

func ClosedFrame(reason CloseReason) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "closed", "reason": string(reason),
	})
}

package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Usage 查询响应缓存
//
// 看板场景：同一用户同时打开多个 Usage 页面，页面每 5 秒刷新一组统计查询。
// 这些查询彼此相同，重复打到 PostgreSQL 是纯浪费。本中间件把「同一用户 +
// 同一路由 + 同一组查询参数」的 200 JSON 响应缓存 5 秒，并把同 key 的并发
// miss 合并成一次下游执行。
//
// 三条不变式：
//  1. 只有 GET、只有 200 进缓存；错误响应一律不进缓存。
//  2. key = 用户 + 路由模板 + 规范化查询参数 + 时间桶 + 用户代次。参数名顺序
//     不同但语义相同的请求共享条目；集合语义参数（agent/provider_key/
//     model_key/device_id）的值顺序也被规范化。单值参数的值顺序即语义
//     （handler 只取第一个值），必须保留。用户/路由/参数不同则互相隔离。
//  3. 写操作成功后用户代次 +1，代次参与 key，所以「查询在写之前开始、写
//     之后失效、旧查询随后落库」的竞态不会复活旧数据。
//
// 缓存是进程内的：多实例之间不共享，跨实例的新鲜度由 5 秒 TTL 兜底。
//
// 适用面：条目只保存状态码、响应体、Content-Type 三项，所以只适合「一次性
// 写完、响应头完全由这三项决定」的响应。Usage 的 summary/breakdown/
// timeseries/devices 都是这种形态；export 不行——它额外带 X-Result-* 头，
// 缓存回放时这些头会丢。
// ---------------------------------------------------------------------------

// usageCacheKey 标识一份可缓存的响应。桶与代次都在 key 里，所以旧桶的条目
// 永远不会被新请求命中（过期条目只等惰性清扫回收）。
type usageCacheKey struct {
	user       string
	path       string
	params     string
	bucket     int64
	generation uint64
}

// usageCacheEntry 是一次已完成的 200 响应。只保存状态码、响应体和
// Content-Type——这三个是本组 JSON handler 唯一对外可见的响应契约要素。
type usageCacheEntry struct {
	status      int
	body        []byte
	contentType string
}

// usageCacheInflight 跟踪某个 key 上的 leader 调用。done 关闭前 entry 不可读；
// 关闭 done 与写 entry 之间由 channel 建立 happens-before，跟随者读到的一定
// 是 leader 写完的结果。
//
// leader 崩了（panic）时 failed=true、panicValue 记着它的 panic 值：等待者
// 共享这一次失败，而不是各自执行下游（否则 N 个等待者会并发打 N 次 DB）。
type usageCacheInflight struct {
	done       chan struct{}
	entry      *usageCacheEntry
	failed     bool
	panicValue any
}

// UsageQueryCache 是 Usage 看板 GET 查询的进程内响应缓存。零值不可用，必须
// 用 NewUsageQueryCache 构造。
//
// 并发模型：一把互斥锁保护 entries、inflight 和 generations 三张表。下游
// handler 绝不持有锁执行（否则一个慢查询会把整个缓存堵死）。过期条目在桶
// 切换时惰性清扫，不起后台 goroutine。
type UsageQueryCache struct {
	mu sync.Mutex

	ttl time.Duration
	now func() time.Time

	// entries: 已完成且可复用的 200 响应。
	entries map[usageCacheKey]*usageCacheEntry
	// inflight: 每个 key 至多一个 leader 调用，其余请求在这里等待。
	inflight map[usageCacheKey]*usageCacheInflight
	// generations: 每用户的缓存代次，成功写后 +1。
	generations map[string]uint64

	// lastSweep 是上次清扫时的桶号；桶号变化时才清扫，避免每次请求都扫全表。
	lastSweep int64
}

// NewUsageQueryCache 构造一个 Usage 查询缓存。
//
// ttl 是缓存桶宽（生产取 5 秒），也就是条目的最长存活时间。now 是时钟
// 注入点（测试用假时钟）；传 nil 时用 time.Now。ttl <= 0 表示禁用缓存，
// 所有请求直接透传给下游。
func NewUsageQueryCache(ttl time.Duration, now func() time.Time) *UsageQueryCache {
	if now == nil {
		now = time.Now
	}
	return &UsageQueryCache{
		ttl:         ttl,
		now:         now,
		entries:     make(map[usageCacheKey]*usageCacheEntry),
		inflight:    make(map[usageCacheKey]*usageCacheInflight),
		generations: make(map[string]uint64),
		lastSweep:   -1,
	}
}

// Handle 返回缓存读中间件。挂在 Usage 的 GET 查询路由上，位置在鉴权和限流
// 之后、handler 之前。
//
// userKey 取 JWT 里的 user_id（接线时用 c.GetString(ContextUserID)）。返回
// 空串表示没有可识别身份，此时请求直接透传，绝不与他人共享缓存条目。
func (q *UsageQueryCache) Handle(userKey func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 只缓存 GET 查询。
		if q.ttl <= 0 || c.Request.Method != http.MethodGet {
			c.Next()
			return
		}
		user := userKey(c)
		if user == "" {
			c.Next()
			return
		}

		bucket := q.bucket()
		// 路径与参数规范化在锁外算：查询串解析不便宜，不该占着全局锁。
		path := c.FullPath()
		params := canonicalQuery(c)

		q.mu.Lock()
		q.sweepLocked(bucket)
		key := usageCacheKey{
			user:       user,
			path:       path,
			params:     params,
			bucket:     bucket,
			generation: q.generations[user],
		}
		if entry, ok := q.entries[key]; ok {
			q.mu.Unlock()
			// 命中缓存：终止链，handler 不能再跑一次（否则响应体会写两遍）。
			c.Abort()
			q.replay(c, entry)
			return
		}
		if flight, ok := q.inflight[key]; ok {
			// 已有 leader 在跑：等它跑完再复用它的结果，不重复访问 DB。
			q.mu.Unlock()
			q.awaitLeader(c, flight)
			return
		}
		// 本请求成为 leader。
		flight := &usageCacheInflight{done: make(chan struct{})}
		q.inflight[key] = flight
		generation := key.generation
		q.mu.Unlock()

		entry := q.executeLeader(c, key, flight, user, generation)

		q.replay(c, entry)
	}
}

// InvalidateOnSuccess 返回写后失效中间件。挂在 Usage 的写路由上（batch、
// backfill、删除、设备写），位置在限流之后、handler 之前。
//
// 只有 2xx 响应才让该用户的缓存代次 +1；失败请求不失效。代次参与 key，
// 所以失效之后旧条目再也命中不了。
//
// 写路径也按当前桶做一次惰性清扫：只写不读的用户永远不会走 Handle，若只在
// 查询路径清扫，这些用户的代次条目就只增不减。清扫只会删除「当前桶内没有任何
// 存活条目」的用户代次，因此不影响失效竞态——已经失效的在途 leader 依旧对不上
// 新代次，结果不会落库。
//
// 缓存禁用（ttl <= 0）时代次毫无用处，因此不更新——否则 generations 会随
// 写请求无限增长。
func (q *UsageQueryCache) InvalidateOnSuccess(userKey func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if q.ttl <= 0 {
			// 缓存禁用：不累积代次。
			return
		}
		status := c.Writer.Status()
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return
		}
		user := userKey(c)
		if user == "" {
			return
		}
		bucket := q.bucket()
		q.mu.Lock()
		q.sweepLocked(bucket)
		q.generations[user]++
		q.mu.Unlock()
	}
}

// awaitLeader 等待 leader 的结果，并把结果（或失败）交给本请求。
//
//   - leader 正常完成：回放 leader 的响应，本请求不执行下游。
//   - leader panic：把同一个 panic 抛给本请求，由 Gin's recovery 像处理
//     leader 那样转成 500。等待者**绝不**自己执行下游——否则 N 个等待者会
//     各自打一次 DB，变成未去重的并发扇出。
//   - 客户端主动断连（context.Canceled）：静默结束。响应已经没人读，写不写
//     都无从送达，也不执行下游。
//   - 服务端 deadline（context.DeadlineExceeded）：**不能**静默结束。
//     timeoutMiddleware 只做 context.WithTimeout + c.Next()，它不写响应；这里
//     直接 Abort 会让客户端收到一个空的 200，把超时伪装成成功。此时继续等
//     leader 并共享它的结果——leader 受同一个服务端 deadline 约束，会先返回
//     （可能已经是 5xx）。等待期间同样不执行下游。
func (q *UsageQueryCache) awaitLeader(c *gin.Context, flight *usageCacheInflight) {
	ctx := c.Request.Context()
	select {
	case <-flight.done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			// 客户端已经放弃这个结果：不等 leader，也不执行下游。
			c.Abort()
			return
		}
		// deadline：等 leader 发布结果（done 一定会被 executeLeader 关闭，
		// 因为 runChain 会 recover 掉下游 panic）。
		<-flight.done
	}

	if flight.failed {
		// leader 崩了：共享这一次失败。
		if flight.panicValue != nil {
			panic(flight.panicValue)
		}
		panic("usage query cache: the leader request failed")
	}
	entry := flight.entry
	if entry == nil {
		// 理论上不可达：leader 正常完成必有 entry，失败必有 failed=true。
		c.Abort()
		return
	}
	// 跟随者不调用 c.Next()，但必须 Abort：否则 Gin 的引擎循环会继续往下
	// 跑 handler，把响应写第二遍。
	c.Abort()
	q.replay(c, entry)
}

// executeLeader 以 leader 身份跑完下游链路，把结果写进缓存并唤醒跟随者。
// 下游执行期间不持有任何锁。
func (q *UsageQueryCache) executeLeader(
	c *gin.Context,
	key usageCacheKey,
	flight *usageCacheInflight,
	user string,
	generation uint64,
) *usageCacheEntry {
	entry, panicValue, panicked := q.runChain(c)

	q.mu.Lock()
	delete(q.inflight, key)
	// 只有 200 进缓存。写期间代次变化说明结果已经过时（查询在写之前开始），
	// 这样的结果不能落库。
	if !panicked && entry.status == http.StatusOK && generation == q.generations[user] {
		q.entries[key] = entry
	}
	if panicked {
		// 记录失败：等待者共享这一次 panic，不会各自去执行下游。
		flight.failed = true
		flight.panicValue = panicValue
	}
	flight.entry = entry
	close(flight.done)
	q.mu.Unlock()

	if panicked {
		// leader 崩了：等待者共享这一次失败（flight.failed=true，各自 panic
		// 同一个值），不会自己去跑下游。这里把 panic 交回 Gin's recovery
		// 中间件，行为与没有缓存时一致。
		panic(panicValue)
	}
	return entry
}

// runChain 装上记录器跑下游链路，返回捕获到的响应。原始 writer 在返回前
// 一定被恢复（panic 路径也恢复），所以 Gin's recovery 中间件仍然写到真正的
// 客户端连接上。
func (q *UsageQueryCache) runChain(c *gin.Context) (entry *usageCacheEntry, panicValue any, panicked bool) {
	original := c.Writer
	recorder := newUsageCacheRecorder(original)
	c.Writer = recorder
	defer func() {
		c.Writer = original
		if r := recover(); r != nil {
			panicked = true
			panicValue = r
		}
	}()

	c.Next()

	return &usageCacheEntry{
		status:      recorder.Status(),
		body:        append([]byte(nil), recorder.body.Bytes()...),
		contentType: recorder.Header().Get("Content-Type"),
	}, nil, false
}

// replay 把缓存里的响应写回客户端。leader 与跟随者走同一条路径，所以两条
// 路径的响应完全一致（状态码 + Content-Type + 响应体）。
func (q *UsageQueryCache) replay(c *gin.Context, entry *usageCacheEntry) {
	writer := c.Writer
	if entry.contentType != "" {
		writer.Header().Set("Content-Type", entry.contentType)
	}
	writer.WriteHeader(entry.status)
	writer.WriteHeaderNow()
	if len(entry.body) > 0 {
		_, _ = writer.Write(entry.body)
	}
}

// bucket 返回当前时间桶。桶宽即 ttl，所以时间推进一个 ttl 必然换桶。
func (q *UsageQueryCache) bucket() int64 {
	return q.now().UnixNano() / int64(q.ttl)
}

// sweepLocked 清扫过期条目。桶号变化时才触发（每个桶一次），把旧桶条目
// 删掉，并顺手回收那些已经没有任何存活条目的用户代次——代次只有在用户完全
// 没有存活条目时才能回收，否则失效语义会被破坏。
//
// 调用方必须持有 q.mu。
func (q *UsageQueryCache) sweepLocked(bucket int64) {
	if q.lastSweep == bucket {
		return
	}
	q.lastSweep = bucket

	live := make(map[string]struct{}, len(q.generations))
	for key := range q.entries {
		if key.bucket < bucket {
			delete(q.entries, key)
			continue
		}
		live[key.user] = struct{}{}
	}
	for user := range q.generations {
		if _, ok := live[user]; !ok {
			delete(q.generations, user)
		}
	}
}

// usageCacheMultiValueParams 是 handler 用 QueryArray 读取、语义为「集合」的
// 参数（agent/provider_key/model_key/device_id）。只有这些参数的值顺序可以
// 规范化掉，因为下游把它们当集合用（IN / ANY），顺序不影响结果。
//
// 其余参数（source/from/to/dimension/limit/interval/group_by/tz…）handler 用
// c.Query / DefaultQuery 只取第一个值，?source=all&source=chat 与
// ?source=chat&source=all 是两种不同语义，值顺序必须保留。
var usageCacheMultiValueParams = map[string]struct{}{
	"agent":        {},
	"provider_key": {},
	"model_key":    {},
	"device_id":    {},
}

// canonicalQuery 把查询参数规范化：参数名排序；只有集合语义的参数
// （usageCacheMultiValueParams）才把同名的多个值排序。于是
// ?agent=b&agent=a 与 ?agent=a&agent=b 共享条目，而重复的单值参数保持原顺序、
// 不会被错误合并。
func canonicalQuery(c *gin.Context) string {
	raw := c.Request.URL.Query()
	if len(raw) == 0 {
		return ""
	}
	canonical := make(url.Values, len(raw))
	for name, values := range raw {
		if _, multi := usageCacheMultiValueParams[name]; !multi {
			// 单值参数：保留原始顺序，顺序即语义。
			canonical[name] = values
			continue
		}
		sorted := make([]string, len(values))
		copy(sorted, values)
		sort.Strings(sorted)
		canonical[name] = sorted
	}
	// url.Values.Encode 本身按参数名排序。
	return canonical.Encode()
}

// ---------------------------------------------------------------------------
// 响应记录器
// ---------------------------------------------------------------------------

// usageCacheNoWritten 对齐 Gin 的 noWritten：尚未写出任何响应时 Size() 为 -1。
const usageCacheNoWritten = -1

// usageCacheRecorder 把下游响应收进内存而不是写给客户端。它嵌入原始的
// gin.ResponseWriter 以复用 Header/Hijack/CloseNotify/Pusher，但覆写了
// 「写出」这一族方法（WriteHeader/WriteHeaderNow/Write/WriteString）**以及
// Flush**——Flush 不再把响应头提交给真实连接。
type usageCacheRecorder struct {
	gin.ResponseWriter
	status int
	size   int
	body   bytes.Buffer
}

func newUsageCacheRecorder(w gin.ResponseWriter) *usageCacheRecorder {
	return &usageCacheRecorder{ResponseWriter: w, status: http.StatusOK, size: usageCacheNoWritten}
}

// WriteHeader 与 Gin 的 responseWriter.WriteHeader 语义一致：第一个状态码
// 生效，之后的调用被忽略。
func (w *usageCacheRecorder) WriteHeader(code int) {
	if code > 0 && w.status != code {
		if w.Written() {
			return
		}
		w.status = code
	}
}

// WriteHeaderNow 只标记「响应已写出」，不碰真实连接——真实响应要等缓存
// 决定如何回放时才写。
func (w *usageCacheRecorder) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}

// Flush 是被**覆写**的方法，不是透传给嵌入 writer：它只标记「已写出」，
// 不碰真实连接。若放任嵌入的 writer 去 Flush，下游 handler 会在缓存还没决定
// 如何回放时就把响应头提交给客户端，leader 的响应就与跟随者不再逐字节一致。
// 代价是流式响应会被整体缓冲——所以缓存只能挂在「一次性写完的 JSON 响应」
// 路由上（Usage 的 summary/breakdown/timeseries/devices 都是这种形态）。
func (w *usageCacheRecorder) Flush() {
	w.WriteHeaderNow()
}

func (w *usageCacheRecorder) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	n, err := w.body.Write(data)
	w.size += n
	return n, err
}

func (w *usageCacheRecorder) WriteString(s string) (int, error) {
	w.WriteHeaderNow()
	n, err := w.body.WriteString(s)
	w.size += n
	return n, err
}

func (w *usageCacheRecorder) Status() int { return w.status }

func (w *usageCacheRecorder) Size() int { return w.size }

func (w *usageCacheRecorder) Written() bool { return w.size != usageCacheNoWritten }

// Unwrap 让 http.ResponseController 能穿过记录器拿到真正的 writer，与 Gin
// 自己的 responseWriter 行为一致。
func (w *usageCacheRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

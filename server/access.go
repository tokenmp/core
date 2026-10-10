package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

// accessRecord 是一次请求的访问记录。
//
// 中间件建立记录并放进 context，转发路径往里填模型、渠道、尝试条数与用量，
// 中间件在处理器返回之后写出这一行。
//
// 来自请求的字符串（method、path、user_agent、model）在记入时就截断，见 clipLogValue。
// 记录因此不会在整个请求期间持有一个兆字节级的字符串，写日志时也不必逐个再防。
//
// 记录不加锁。它只被本次请求的那一个 goroutine 读写：转发路径全程同步，
// 中间件在 ServeHTTP 返回之后才读，两者之间是程序顺序，不存在并发窗口。
type accessRecord struct {
	requestID string
	method    string
	path      string
	client    string
	// userAgent 是请求头里的 User-Agent，已经过 clipLogValue 截断。它和 client 一起
	// 用来认客户端：默认只听 127.0.0.1 时 client 恒为 127.0.0.1，认不出是谁在调用。
	userAgent string
	started   time.Time
	status    int
	model     string
	channel   string
	attempts  int
	// 用量按字段存，不存 *dto.Usage：那个结构体带指针与三十来个字段，交给 slog
	// 打出来是一串结构体字面量与内存地址。按字段存也顺带切断了与转换会话的别名，
	// 收尾之后这份拷贝不会再被改写。
	hasUsage         bool
	promptTokens     int
	completionTokens int
	totalTokens      int
	cachedTokens     int
}

// accessRecordKey 是记录在 context 里的键。用私有类型避免与别的包撞键。
type accessRecordKey struct{}

// accessOf 取出本次请求的访问记录。请求不经过中间件时返回 nil。
func accessOf(ctx context.Context) *accessRecord {
	record, _ := ctx.Value(accessRecordKey{}).(*accessRecord)
	return record
}

// requestIDOf 返回本次请求的 id。取不到时返回空串。
func requestIDOf(ctx context.Context) string {
	record := accessOf(ctx)
	if record == nil {
		return ""
	}
	return record.requestID
}

// setModel 记下这次请求的模型名。接收者为 nil 时是空操作。模型名来自请求体，
// 与其余来自请求的字符串一样先截断。
//
// 接收者可以为 nil 是本文件里这几个写入方法的统一约定：记录只由中间件建立，
// 直接调用转发路径的场合（例如只用了 engine 的宿主）没有记录，写入应当静默跳过，
// 而不是在各处重复判空。
func (r *accessRecord) setModel(model string) {
	if r == nil {
		return
	}
	r.model = clipLogValue(model)
}

// served 记下实际服务的那条渠道，以及一共尝试了几条候选。
func (r *accessRecord) served(channel string, attempts int) {
	if r == nil {
		return
	}
	r.channel = channel
	r.attempts = attempts
}

// recordUsage 记下这次请求的用量。上游没给用量时保持原样。
func (r *accessRecord) recordUsage(usage *dto.Usage) {
	if r == nil || usage == nil {
		return
	}
	r.hasUsage = true
	r.promptTokens = usage.PromptTokens
	r.completionTokens = usage.CompletionTokens
	r.totalTokens = usage.TotalTokens
	r.cachedTokens = usage.PromptCacheHitTokens
}

// withAccessLog 包住路由表，给每个请求记两行访问日志（进入与完成），并让这次请求
// 的其余日志带上同一个 request_id。
//
// 包在 routes 这一层而不是散在转发路径上：/healthz、/v1/models、未知路径与四种
// 方言的端点都从同一处经过，宿主用 Handler() 递进来的请求也照样入账。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		record := &accessRecord{
			requestID: s.newRequestID(),
			method:    clipLogValue(request.Method),
			path:      clipLogValue(request.URL.Path),
			client:    clientAddress(request.RemoteAddr),
			userAgent: clipLogValue(request.UserAgent()),
			// started 取在写进入行之前。duration 记的是客户端等待的时长，日志 sink 慢
			// 带来的拖延也一样拖了客户端，不该从这段时长里排除掉。
			started: s.now(),
		}
		s.logArrival(record)
		tracked := &accessWriter{ResponseWriter: writer, record: record}
		ctx := context.WithValue(request.Context(), accessRecordKey{}, record)
		defer func() {
			if recovered := recover(); recovered != nil {
				// 处理器 panic 时补一行再原样抛出：这条请求确实失败了，不能按
				// 未写响应头的默认值记成 200。抛出之后 net/http 原有的 panic
				// 处理，以及 http.ErrAbortHandler 的语义都不变。
				record.status = http.StatusInternalServerError
				s.logAccess(record)
				panic(recovered)
			}
			s.logAccess(record)
		}()
		next.ServeHTTP(tracked, request.WithContext(ctx))
	})
}

// logArrival 写出请求进入的那一行。
//
// 进入行与完成行分开记，是因为两者回答的问题不同。完成行回答「这个请求最后怎么了」；
// 转发路径只在请求结束时写日志，请求进来之后、结束之前本来没有痕迹。流式响应可能
// 持续几分钟，这期间日志里看不出请求正在跑；请求卡住或进程被杀时，至少还留下这一行。
//
// 代价是每个请求多一行。两行各管一半：进入行回答「是谁在调用」，给的是 client 与
// user_agent；完成行回答「结果是什么」，给的是 status、duration、model、channel 与
// attempts。method、path 与 request_id 两行都有，用来把同一次请求认出来。进入行必定
// 写出（在处理器之前，panic 路径也写出），所以完成行不再重复客户端标识。
//
// 记 INFO 而不是 DEBUG。进入时还不知道结果，没有状态码可以分级；而默认的 log.level
// 是 info，把进入行记成 DEBUG 等于默认看不见，上面那点可见性就拿不到。
func (s *Server) logArrival(record *accessRecord) {
	s.logger.Info("请求进入",
		"request_id", record.requestID,
		"method", record.method,
		"path", record.path,
		"client", record.client,
		"user_agent", record.userAgent)
}

// logAccess 写出完成行。
//
// 级别口径：状态码小于 400 记 INFO，其余记 WARN。默认的 log.level 是 info，
// 因此服务只要在跑就能看见每个请求，包括同方言透传这条最常见的路径；失败请求
// 与既有的 WARN 行同级，一起由 log.level 控制。
func (s *Server) logAccess(record *accessRecord) {
	status := record.status
	if status == 0 {
		// 处理器没有显式调 WriteHeader：net/http 会按 200 写响应头。
		status = http.StatusOK
	}
	attrs := []any{
		"request_id", record.requestID,
		"method", record.method,
		"path", record.path,
		"status", status,
		"duration", s.now().Sub(record.started),
		"model", record.model,
		"channel", record.channel,
		"attempts", record.attempts,
	}
	if record.hasUsage {
		attrs = append(attrs,
			"prompt_tokens", record.promptTokens,
			"completion_tokens", record.completionTokens,
			"total_tokens", record.totalTokens)
		if record.cachedTokens > 0 {
			attrs = append(attrs, "cached_tokens", record.cachedTokens)
		}
	}
	if status < http.StatusBadRequest {
		s.logger.Info("请求完成", attrs...)
		return
	}
	s.logger.Warn("请求完成", attrs...)
}

// newRequestID 生成进程内唯一的请求 id。
//
// 用自增计数器而不是随机数：唯一性由计数器本身保证，不依赖熵源，也就不存在
// 「生成失败」这条路径。id 只在日志里出现，不参与任何路由或鉴权判断。
func (s *Server) newRequestID() string {
	return strconv.FormatUint(s.requestCounter.Add(1), 10)
}

// maxLogValueBytes 是访问日志里来自请求的字符串字段写出的字节上限。
//
// 这些值都不受别处约束：请求体上限是 32 MiB，请求头走 Go 默认的 1 MiB（Server 没有设
// MaxHeaderBytes），而同一个值在进入行与完成行里各写一次。实测一个请求能把一行日志写到
// 兆字节级。channel 来自配置文件，client 与 request_id 由服务端生成，不受这条规则约束。
const maxLogValueBytes = 256

// logValueEllipsis 是截断标记。它算在上限之内，clipLogValue 的结果因此恒不超过
// maxLogValueBytes，而不是上限之外再加三个字节。
const logValueEllipsis = "…"

// clipLogValue 把请求带来的字符串截到上限之内。
//
// 截断点回退到 UTF-8 字符边界，合法的 UTF-8 因此不会被切成半个字符。Go 的请求头解析
// 不校验 UTF-8，值里可以带非法字节，那种输入截断后仍可能以不完整的字符结尾；slog 的
// 两种 handler 都会把它转义成合法输出，代价只是几个 \xNN。
func clipLogValue(value string) string {
	if len(value) <= maxLogValueBytes {
		return value
	}
	cut := maxLogValueBytes - len(logValueEllipsis)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + logValueEllipsis
}

// clientAddress 从 RemoteAddr 里去掉端口。取不出主机时用原值。
func clientAddress(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// accessWriter 记下处理器写出的状态码，并把 Flush 透传到底层。
//
// Flush 不能省。channel 与 server 里都用 writer.(http.Flusher) 取刷新器，
// 断言失败时静默退化成不刷新，流式响应会攒到缓冲区满才发出去，而且不会报错。
// 这里无条件转发，底层没有刷新能力时什么也不做。
type accessWriter struct {
	http.ResponseWriter
	record *accessRecord
}

// WriteHeader 只记第一个状态码，与 net/http 的行为一致。
func (w *accessWriter) WriteHeader(status int) {
	if w.record.status == 0 {
		w.record.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write 在处理器没有显式调 WriteHeader 时记下 200：net/http 会按 200 写响应头。
func (w *accessWriter) Write(body []byte) (int, error) {
	if w.record.status == 0 {
		w.record.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

// Flush 无条件转发到底层。
func (w *accessWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

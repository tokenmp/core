package server

import (
	"time"

	"github.com/tokenmp/core/dialect"
)

// Outcome 是一次请求结束时交给宿主的原始量。
//
// 只交原始量，不算指标。指标口径随宿主变：TTFT 是从请求进入算还是从上游首字节算，
// TPS 的分母取总时长还是生成时长，core 算出来的一定不合宿主的意。宿主拿这些量自己算。
//
// 字段是每次请求都会填的，没有「有时有有时没有」的项：上游没给用量时 Usage 为 nil，
// 这是唯一的可选项。
type Outcome struct {
	// RequestID 是这次请求的 id，与访问日志里的 request_id 相同。
	RequestID string
	Method    string
	Path      string
	// Client 是去掉端口之后的客户端地址，UserAgent 是请求头里的 User-Agent。
	Client    string
	UserAgent string

	// Status 是写回客户端的状态码。处理器没有显式写响应头时按 200 记，与访问日志一致。
	Status int
	// Duration 是从请求进入中间件到处理器返回的时长，含重试与等待上游的全部时间。
	Duration time.Duration
	// FirstByte 是从请求进入中间件到首次写出响应体的时长，也就是 core 能测到的 TTFT。
	// 整个请求没有写出过响应体时为 0。
	//
	// 只算响应体，不算响应头：流式路径拿到上游响应头就写响应头，而第一个分片可能过很久
	// 才到，按响应头记会让 TTFT 系统性偏小。非流式响应是一次写完的，这个值因此约等于
	// Duration，看不出 TTFT——该不该算由宿主按 Streaming 自己判断。
	FirstByte time.Duration
	// BytesWritten 是写回客户端的响应体字节数，不含响应头。
	BytesWritten int64

	// Model 是插件改写之后参与匹配的模型名。Channel 是实际服务的那条渠道，降级换过
	// 渠道时是真正服务的那个；没有一条候选服务成功时为空串。
	Model   string
	Channel string
	// Streaming 报告客户端是否要求流式，取自请求内容，不是上游实际按流式回的。
	Streaming bool

	// Attempts 按顺序记下每条候选尝试的耗时与状态，长度与访问日志的 attempts 相同。
	Attempts []Attempt

	// Usage 是上游给出的用量，上游没给时为 nil。
	Usage *Usage
}

// Attempt 是一次候选尝试。
type Attempt struct {
	Channel  string
	Dialect  dialect.Name
	Upstream string
	// Status 是上游返回的状态码。0 表示没拿到上游响应，原因看 Failure。
	Status int
	// Failure 是没拿到上游响应的原因，Status 非 0 时恒为 FailureNone。
	Failure Failure
	// Duration 是这次尝试的耗时，从选定候选算到拿到响应头或失败。
	Duration time.Duration
}

// Failure 是一次尝试没拿到上游响应的原因。
//
// 取值封闭：不把上游的错误文字带进来。那是客户端与上游可控的字符串，长度没有上限，
// 放进公开面就得跟着定一套截断口径。原因文字在 WARN 日志里。
type Failure string

const (
	// FailureNone 表示没有失败，看 Status。
	FailureNone Failure = ""
	// FailureChannel 表示渠道在装配里不存在。
	FailureChannel Failure = "channel"
	// FailureConvert 表示请求转换失败，没发出请求。
	FailureConvert Failure = "convert"
	// FailureConnect 表示连接上游失败，或读上游响应失败。
	FailureConnect Failure = "connect"
)

// Usage 是上游给出的用量。字段与访问日志一致，够算 TPS 与账目。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 是命中提示缓存的那部分提示词 token，上游没报时为 0。
	CachedTokens int
}

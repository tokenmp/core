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
	// FirstByte 是从请求进入中间件到首次写出响应头或响应体的时长。整个请求没有写出过
	// 任何东西时为 0，宿主可以据此判断 TTFT 无从计算。
	//
	// 取「首次写出」而不是「首次写出响应体」：流式响应里处理器先写响应头再写第一个
	// 分片，两者之间通常没有别的工作，而 net/http 不暴露分片何时真的离开缓冲区。
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
	// Status 是上游返回的状态码。0 表示没拿到上游响应：转换失败、连接失败，
	// 或渠道在装配里不存在。
	Status int
	// Duration 是这次尝试的耗时，从选定候选算到拿到响应头或失败。
	Duration time.Duration
}

// Usage 是上游给出的用量。字段与访问日志一致，够算 TPS 与账目。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 是命中提示缓存的那部分提示词 token，上游没报时为 0。
	CachedTokens int
}

// Package relay 是转发内核：把客户端方言的请求体转换成上游方言，把上游的响应体
// 转换回客户端方言，并在两个方向上原样带出转换损耗的诊断。
//
// 内核是纯函数式的，不发网络请求、不读配置：上游地址、鉴权、重试都在 channel
// 与 server 层。同方言不走转换，由调用方按字节透传，因此本包在两方言相同时
// 直接返回 passthrough 标记而不是重新序列化 —— 逐字节保留未建模字段只有在
// 不经过 JSON 往返时才成立。
package relay

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/reasoning"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/tokenmp/core/dialect"
)

// DefaultClaudeMaxTokens 是 OpenAI 或 Gemini 转 Claude 时，源请求未带
// max_tokens 时补的默认值。Claude Messages 要求该字段，缺失即 400。
const DefaultClaudeMaxTokens = 4096

// ErrSameDialect 表示两个方言相同，无需转换。
var ErrSameDialect = errors.New("relay: 源方言与目标方言相同，应按字节透传")

// Options 是内核的行为开关。零值可用，此时只做无附加适配的协议转换。
type Options struct {
	// ToolLossPolicy 决定请求转换遇到工具语义损耗时是否报错。默认 allow：
	// 照常完成并把每条损耗写进诊断。响应与流式转换不受该策略影响。
	ToolLossPolicy types.ConversionLossPolicy
	// DefaultMaxTokens 返回源请求未带 max_tokens 时给 Claude 补的值。
	// 为 nil 时用 DefaultClaudeMaxTokens。
	DefaultMaxTokens func(model string) int
	// ThinkingAdapter 打开推理设置的渲染。模型名尾缀仍由 ParseModel 先行剥离。
	ThinkingAdapter bool
	// ModelSuffix 打开模型名尾缀识别。打开时 ParseModel 才会剥离
	// -thinking / -nothinking / -high 这类尾缀。
	ModelSuffix bool
}

// Engine 是转换内核。可以并发使用。
type Engine struct {
	options convmeta.Options
	suffix  bool
}

// NewEngine 按选项创建内核。
func NewEngine(opts Options) *Engine {
	defaultMaxTokens := opts.DefaultMaxTokens
	if defaultMaxTokens == nil {
		defaultMaxTokens = func(string) int { return DefaultClaudeMaxTokens }
	}
	return &Engine{
		suffix: opts.ModelSuffix,
		options: convmeta.Options{
			ToolLossPolicy: opts.ToolLossPolicy,
			Claude: convmeta.ClaudeOptions{
				ThinkingAdapterEnabled: opts.ThinkingAdapter,
				DefaultMaxTokens:       defaultMaxTokens,
			},
			Gemini: convmeta.GeminiOptions{
				ThinkingAdapterEnabled: opts.ThinkingAdapter,
			},
		},
	}
}

// Options 返回内核使用的转换选项快照。
func (e *Engine) Options() convmeta.Options {
	return e.options
}

// ParseModel 按需剥离模型名尾缀。
//
// 未打开 ModelSuffix 时原样返回，状态为 nil。打开时识别各家已知尾缀，返回
// 基础模型名与推理状态；识别不出尾缀时同样原样返回。
func (e *Engine) ParseModel(model string) (string, *dto.ReasoningConversionState) {
	if !e.suffix {
		return model, nil
	}
	base, intent, ok, err := reasoning.ParseKnownProviderModelSuffix(model, true)
	if err != nil || !ok {
		return model, nil
	}
	state := reasoning.StateFromIntent(intent)
	if state == nil {
		return base, nil
	}
	return base, state
}

// Prepared 是转换后的上游请求。
type Prepared struct {
	// Passthrough 为真时 Body 是客户端原始报文字节，未做解析。
	Passthrough bool
	// Body 是发给上游的请求体。
	Body []byte
	// Stream 是客户端请求的流式意图。
	Stream bool
	// Result 是转换结果，Passthrough 时为 nil。
	Result *relayconvert.RequestResult
}

// Diagnostics 返回请求转换产生的诊断。
func (p *Prepared) Diagnostics() []types.ConversionDiagnostic {
	if p == nil || p.Result == nil {
		return nil
	}
	return p.Result.Diagnostics
}

// Request 描述一次待转换的请求。
//
// UpstreamModel 是发往上游的模型名，由调用方从配置的路由规则算出；为空时沿用
// OriginModel。OriginModel 是客户端请求的模型名，用于推理状态判断与诊断。
//
// Stream 是调用方从请求行或路径得到的流式意图。Gemini 的流式只体现在路径上
// （:streamGenerateContent），请求体里没有该字段，因此这个方言必须由调用方给出；
// 另外三方言以请求体为准，体里没写时才用本字段。
//
// Body 会被完整读入内存后解析，调用方负责限制其大小。
type Request struct {
	From          dialect.Name
	To            dialect.Name
	Body          []byte
	OriginModel   string
	UpstreamModel string
	Stream        bool
}

// PrepareRequest 生成发给上游的请求体。
//
// 两方言相同时返回 Passthrough 为真的结果，不做任何解析。
func (e *Engine) PrepareRequest(ctx context.Context, req Request) (*Prepared, error) {
	if req.From == req.To {
		stream, err := e.streamIntent(req)
		if err != nil {
			return nil, err
		}
		return &Prepared{Passthrough: true, Body: req.Body, Stream: stream}, nil
	}
	request, err := dialect.DecodeRequest(req.From, req.Body)
	if err != nil {
		return nil, err
	}
	stream := req.Stream || requestIsStream(request)
	upstreamModel := req.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = req.OriginModel
	}
	baseModel, state := e.ParseModel(req.OriginModel)
	meta := &convmeta.Values{
		OriginModelName:     baseModel,
		UpstreamModelName:   upstreamModel,
		ChannelMetaAttached: true,
		IsStream:            stream,
		ReasoningConversion: state,
		Options:             &e.options,
	}
	result, err := relayconvert.ConvertRequest(ctx, meta, req.To.RelayFormat(), request)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 请求失败: %w", req.From, req.To, err)
	}
	encoded, err := dialect.Encode(result.Value)
	if err != nil {
		return nil, err
	}
	return &Prepared{Body: encoded, Stream: stream, Result: result}, nil
}

// PreparedResponse 是转换后的非流式响应。
type PreparedResponse struct {
	// Passthrough 为真时 Body 是上游原始报文字节，未做解析。
	Passthrough bool
	// Body 是发给客户端的响应体。
	Body []byte
	// Usage 是归一后的用量，可能为 nil。
	Usage *dto.Usage
	// Diagnostics 是转换损耗。
	Diagnostics []types.ConversionDiagnostic
}

// Response 描述一次待转换的非流式响应。From 是渠道方言，To 是客户端方言。
type Response struct {
	From          dialect.Name
	To            dialect.Name
	Body          []byte
	OriginModel   string
	UpstreamModel string
}

// PrepareResponse 把上游非流式响应转换回客户端方言。
func (e *Engine) PrepareResponse(ctx context.Context, resp Response) (*PreparedResponse, error) {
	if resp.From == resp.To {
		return &PreparedResponse{Passthrough: true, Body: resp.Body}, nil
	}
	response, err := dialect.DecodeResponse(resp.From, resp.Body)
	if err != nil {
		return nil, err
	}
	meta := e.responseMeta(resp.OriginModel, resp.UpstreamModel)
	result, err := relayconvert.ConvertResponse(ctx, meta, resp.To.RelayFormat(), response)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 响应失败: %w", resp.From, resp.To, err)
	}
	encoded, err := dialect.Encode(result.Value)
	if err != nil {
		return nil, err
	}
	return &PreparedResponse{Body: encoded, Usage: result.Usage, Diagnostics: result.Diagnostics}, nil
}

// responseMeta 构造响应阶段的转换上下文。推理状态在响应阶段不参与判断，
// 因此不需要从模型名重新解析。
func (e *Engine) responseMeta(originModel, upstreamModel string) *convmeta.Values {
	if upstreamModel == "" {
		upstreamModel = originModel
	}
	return &convmeta.Values{
		OriginModelName:     originModel,
		UpstreamModelName:   upstreamModel,
		ChannelMetaAttached: true,
		Options:             &e.options,
	}
}

// streamIntent 只解析出请求体的流式意图，不做完整建模。
func (e *Engine) streamIntent(req Request) (bool, error) {
	request, err := dialect.DecodeRequest(req.From, req.Body)
	if err != nil {
		return false, err
	}
	return req.Stream || requestIsStream(request), nil
}

// requestIsStream 从请求 DTO 读流式意图。
func requestIsStream(request any) bool {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.OpenAIResponsesRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.ClaudeRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.GeminiChatRequest:
		// Gemini 的流式由端点区分，请求体里没有该字段。
		return false
	default:
		return false
	}
}

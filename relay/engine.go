// Package relay 是转发内核：把客户端方言的请求体转换成上游方言，把上游的响应体
// 转换回客户端方言，并在两个方向上原样带出转换损耗的诊断。
//
// 内核是纯函数式的，不发网络请求、不读配置：上游地址、鉴权、重试都在 channel
// 与 server 层。同方言不走转换，本包在两方言相同时直接返回 passthrough 标记，
// 只按字节定点改顶层 model 一个字段而不重新序列化 —— 逐字节保留未建模字段只有
// 在不经过 JSON 往返时才成立。
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/reasoning"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/sjson"

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
	ToolLossPolicy LossPolicy
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
			ToolLossPolicy: types.ConversionLossPolicy(opts.ToolLossPolicy),
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

// ParseModel 按需剥离模型名尾缀。
//
// 未打开 ModelSuffix 时原样返回，状态为 nil。打开时识别各家已知尾缀，返回
// 基础模型名与推理状态；识别不出尾缀时同样原样返回。
//
// 调用方应当用返回的基础模型名去匹配路由规则，再去发请求。
func (e *Engine) ParseModel(model string) (string, *Reasoning) {
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
	return base, reasoningOf(state)
}

// Prepared 是转换后的上游请求。
type Prepared struct {
	// Passthrough 为真时 Body 是客户端原始报文字节（可能只改过顶层 model 一个
	// 字段），未做 DTO 建模。
	Passthrough bool
	// Body 是发给上游的请求体。
	Body []byte
	// Stream 是客户端请求的流式意图。
	Stream bool

	report      Report
	diagnostics []Diagnostic
}

// Report 说明这次转换是怎么完成的。Passthrough 为真时返回零值。
func (p *Prepared) Report() Report {
	if p == nil {
		return Report{}
	}
	return p.report
}

// Diagnostics 返回请求转换产生的诊断。Passthrough 为真时返回空。
func (p *Prepared) Diagnostics() []Diagnostic {
	if p == nil {
		return nil
	}
	return p.diagnostics
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
// 两方言相同时返回 Passthrough 为真的结果，不做 DTO 建模；但只要规则给了与
// 客户端模型名不同的上游名，顶层 model 字段仍会被定点改写，见 setModelField。
func (e *Engine) PrepareRequest(ctx context.Context, req Request) (*Prepared, error) {
	upstreamModel := req.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = req.OriginModel
	}
	if req.From == req.To {
		stream, err := e.streamIntent(req)
		if err != nil {
			return nil, err
		}
		body, err := setModelField(req.To, req.Body, req.OriginModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		return &Prepared{Passthrough: true, Body: body, Stream: stream}, nil
	}
	request, err := dialect.DecodeRequest(req.From, req.Body)
	if err != nil {
		return nil, err
	}
	stream := req.Stream || requestIsStream(request)
	baseModel, parsed := e.ParseModel(req.OriginModel)
	meta := &convmeta.Values{
		OriginModelName:     baseModel,
		UpstreamModelName:   upstreamModel,
		ChannelMetaAttached: true,
		IsStream:            stream,
		ReasoningConversion: reasoningStateOf(parsed),
		Options:             &e.options,
	}
	result, err := relayconvert.ConvertRequest(ctx, meta, relayFormatOf(req.To), request)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 请求失败: %w", req.From, req.To, err)
	}
	encoded, err := dialect.Encode(result.Value)
	if err != nil {
		return nil, err
	}
	body, err := setModelField(req.To, encoded, req.OriginModel, upstreamModel)
	if err != nil {
		return nil, err
	}
	return &Prepared{
		Body:        body,
		Stream:      stream,
		report:      reportOfRequest(result),
		diagnostics: requestDiagnostics(result),
	}, nil
}

// setModelField 把目标报文顶层的 model 字段定点改成上游模型名。
//
// 目标方言不是 Gemini、上游名又确实与客户端模型名不同时才改写，两个分支都走
// 这里收口。这样做与 RelayKit 每个请求转换器各自的行为无关：四个方向的转换器
// 对 UpstreamModelName 的处理并不一致，其中两个 Gemini 方向把模型名写进路径，
// 而 claude_messages 转 openai 等方向仍沿用请求体里的模型名。既然四种方言的
// 目标报文都在顶层有 model 字段，集中改一处比逐个方向补丁可靠。Gemini 的模型名
// 在路径上而不是请求体里，由 channel 按 route.Upstream 拼路径，本来就对，跳过。
//
// 用 sjson 逐字节定点替换，不经过 DTO 往返：未建模字段、多余空白与 1e5 这类
// 非规范写法全部原样保留。值相同时直接返回原字节，因此「规则没写 upstream」或
// 「名字相同」的同方言请求仍然是零改动。
func setModelField(to dialect.Name, body []byte, originModel, upstreamModel string) ([]byte, error) {
	if to == dialect.GeminiGenerate || upstreamModel == "" || upstreamModel == originModel {
		return body, nil
	}
	updated, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("relay: 改写上游模型名失败: %w", err)
	}
	return updated, nil
}

// PreparedResponse 是转换后的非流式响应。
type PreparedResponse struct {
	// Passthrough 为真时 Body 是上游原始报文字节，未做解析。
	Passthrough bool
	// Body 是发给客户端的响应体。
	Body []byte
	// Usage 是归一后的用量，可能为 nil。
	//
	// 这个字段用的是 RelayKit 的类型，理由见 docs/relay.md 的
	// 「与 RelayKit 的关系」：镜像用量结构只会带来漂移。
	Usage *dto.Usage

	report      Report
	diagnostics []Diagnostic
}

// Report 说明这次转换是怎么完成的。Passthrough 为真时返回零值。
func (p *PreparedResponse) Report() Report {
	if p == nil {
		return Report{}
	}
	return p.report
}

// Diagnostics 返回响应转换产生的诊断。Passthrough 为真时返回空。
func (p *PreparedResponse) Diagnostics() []Diagnostic {
	if p == nil {
		return nil
	}
	return p.diagnostics
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
		return &PreparedResponse{
			Passthrough: true,
			Body:        resp.Body,
			Usage:       passthroughUsage(ctx, e.responseMeta(resp.OriginModel, resp.UpstreamModel), resp),
		}, nil
	}
	response, err := dialect.DecodeResponse(resp.From, resp.Body)
	if err != nil {
		return nil, err
	}
	meta := e.responseMeta(resp.OriginModel, resp.UpstreamModel)
	result, err := relayconvert.ConvertResponse(ctx, meta, relayFormatOf(resp.To), response)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 响应失败: %w", resp.From, resp.To, err)
	}
	encoded, err := dialect.Encode(result.Value)
	if err != nil {
		return nil, err
	}
	return &PreparedResponse{
		Body:        encoded,
		Usage:       result.Usage,
		report:      reportOfResponse(result),
		diagnostics: responseDiagnostics(result),
	}, nil
}

// passthroughUsage 为同方言直通的响应取出用量，响应字节本身不改。
// 子串预筛覆盖四种方言的用量键（Gemini 为 usageMetadata），只有可能带用量的
// 响应才解码。解码或转换失败时返回 nil：直通的字节照常写给客户端，统计失败
// 不能让请求失败。
func passthroughUsage(ctx context.Context, meta *convmeta.Values, resp Response) *dto.Usage {
	if !bytes.Contains(resp.Body, []byte("usage")) {
		return nil
	}
	decoded, err := dialect.DecodeResponse(resp.From, resp.Body)
	if err != nil {
		return nil
	}
	result, err := relayconvert.ConvertResponse(ctx, meta, relayFormatOf(resp.From), decoded)
	if err != nil {
		return nil
	}
	return result.Usage
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

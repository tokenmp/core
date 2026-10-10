package relay

import (
	"context"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/tokenmp/core/dialect"
)

// StreamOptions 是流式转换会话的创建参数，取自客户端请求里与响应信封相关的字段。
type StreamOptions struct {
	// ID 是转发给客户端的响应 id 前缀。
	ID string
	// Model 是客户端请求的模型名，写进响应信封。
	Model string
	// Created 是响应创建时间（Unix 秒）。
	Created int64
	// IncludeUsage 要求上游在流末尾给出用量。
	IncludeUsage bool
	// EmitSequenceNumber 打开 OpenAI Responses 的 sequence_number 字段。
	EmitSequenceNumber bool
}

// Stream 是一条上游流的转换会话。每条上游流对应一个实例，不可并发复用。
type Stream struct {
	engine *Engine
	from   dialect.Name
	to     dialect.Name
	state  *relayconvert.ResponseStreamState
	meta   *convmeta.Values
}

// NewStream 创建转换会话。
//
// from 是渠道方言，to 是客户端方言。两方言相同时返回 ErrSameDialect：那种情况
// 应当按字节管道转发，经过 JSON 往返会丢掉未建模字段。
func (e *Engine) NewStream(from, to dialect.Name, originModel, upstreamModel string, opts StreamOptions) (*Stream, error) {
	if from == to {
		return nil, ErrSameDialect
	}
	state, err := relayconvert.NewResponseStreamState(
		from.RelayFormat(),
		to.RelayFormat(),
		relayconvert.ResponseStreamOptions{
			ID:                 opts.ID,
			Model:              opts.Model,
			Created:            opts.Created,
			IncludeUsage:       opts.IncludeUsage,
			EmitSequenceNumber: opts.EmitSequenceNumber,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 流式会话创建失败: %w", from, to, err)
	}
	return &Stream{
		engine: e,
		from:   from,
		to:     to,
		state:  state,
		meta:   e.responseMeta(originModel, upstreamModel),
	}, nil
}

// Convert 转换一条上游流式事件。data 是 SSE 的 data 负载。返回空切片表示这条
// 事件不承载内容，调用方跳过即可。
//
// meta.IsStream 在会话内固定为真，供上游为 Gemini 的转换路径判断目标请求是否
// 需要流式字段。
func (s *Stream) Convert(ctx context.Context, data []byte) ([]dialect.StreamEvent, error) {
	value, ok, err := dialect.DecodeStreamEvent(s.from, data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s.meta.IsStream = true
	results, err := relayconvert.ConvertStreamResponseChunk(ctx, s.meta, s.state, value)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 流式分片失败: %w", s.from, s.to, err)
	}
	return s.encode(results)
}

// Finalize 结束会话，补发终止事件与最终用量。上游流结束后必须调用一次，
// 否则部分转换路径不会补出终止帧。
func (s *Stream) Finalize(ctx context.Context) ([]dialect.StreamEvent, error) {
	results, err := relayconvert.FinalizeStreamResponse(ctx, s.meta, s.state)
	if err != nil {
		return nil, fmt.Errorf("relay: %s 转 %s 流式收尾失败: %w", s.from, s.to, err)
	}
	return s.encode(results)
}

// Fail 在流中途失败时补出错误事件。第二个返回值表示是否补出了事件。
func (s *Stream) Fail(code, message, param string) ([]dialect.StreamEvent, bool) {
	results, ok := s.state.FailResponsesStream(code, message, param)
	if !ok {
		return nil, false
	}
	events, err := s.encode(results)
	if err != nil {
		return nil, false
	}
	return events, true
}

// Usage 返回会话累计的用量，可能为 nil。
func (s *Stream) Usage() *dto.Usage {
	return s.state.Usage()
}

// Diagnostics 返回会话累计的转换损耗。
func (s *Stream) Diagnostics() []types.ConversionDiagnostic {
	return s.state.Diagnostics()
}

// encode 把转换结果编成可写出的流式事件。
func (s *Stream) encode(results []relayconvert.ResponseResult) ([]dialect.StreamEvent, error) {
	if len(results) == 0 {
		return nil, nil
	}
	events := make([]dialect.StreamEvent, 0, len(results))
	for _, result := range results {
		event, err := dialect.EncodeStreamEvent(s.to, result.Value)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

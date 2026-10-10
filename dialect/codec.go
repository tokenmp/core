package dialect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
)

// StreamEvent 是一条待写出的流式事件。
type StreamEvent struct {
	// Event 是 SSE 的 event 名。四种方言里只有 Claude Messages 与 OpenAI
	// Responses 用命名事件，另外两种为空。
	Event string
	// Data 是事件的 JSON 负载。
	Data []byte
}

// doneMarker 是 OpenAI Chat Completions 的流结束标记，它不是 JSON。
const doneMarker = "[DONE]"

// DecodeRequest 把请求体解析成 RelayKit 的请求 DTO。返回的具体类型由方言决定：
// OpenAI Chat 是 *dto.GeneralOpenAIRequest，OpenAI Responses 是
// *dto.OpenAIResponsesRequest，Claude 是 *dto.ClaudeRequest，Gemini 是
// *dto.GeminiChatRequest。
func DecodeRequest(name Name, body []byte) (any, error) {
	var target any
	switch name {
	case OpenAIChat:
		target = &dto.GeneralOpenAIRequest{}
	case OpenAIResponses:
		target = &dto.OpenAIResponsesRequest{}
	case ClaudeMessages:
		target = &dto.ClaudeRequest{}
	case GeminiGenerate:
		target = &dto.GeminiChatRequest{}
	default:
		return nil, fmt.Errorf("dialect: 未知方言 %q", name)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, fmt.Errorf("dialect: 解析 %s 请求失败: %w", name, err)
	}
	return target, nil
}

// DecodeResponse 把非流式响应体解析成 RelayKit 的响应 DTO。
func DecodeResponse(name Name, body []byte) (any, error) {
	var target any
	switch name {
	case OpenAIChat:
		target = &dto.OpenAITextResponse{}
	case OpenAIResponses:
		target = &dto.OpenAIResponsesResponse{}
	case ClaudeMessages:
		target = &dto.ClaudeResponse{}
	case GeminiGenerate:
		target = &dto.GeminiChatResponse{}
	default:
		return nil, fmt.Errorf("dialect: 未知方言 %q", name)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, fmt.Errorf("dialect: 解析 %s 响应失败: %w", name, err)
	}
	return target, nil
}

// DecodeStreamEvent 解析一条上游流式事件的 data 负载。第二个返回值为假表示
// 这条事件不承载内容（结束标记、心跳），调用方应跳过它。
//
// 空负载与 OpenAI Chat 的 [DONE] 标记都属于跳过之列；Claude 与 OpenAI
// Responses 的 ping 事件同理。
func DecodeStreamEvent(name Name, data []byte) (any, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if name == OpenAIChat && string(trimmed) == doneMarker {
		return nil, false, nil
	}
	value, err := DecodeStreamResponse(name, trimmed)
	if err != nil {
		return nil, false, err
	}
	if isHeartbeat(name, value) {
		return nil, false, nil
	}
	return value, true, nil
}

// DecodeStreamResponse 把流式事件的负载解析成对应方言的流式 DTO。
//
// 流式与非流式的 DTO 不同名也不同形（例如 OpenAI Chat 的分片是 delta 而不是
// message），这里必须分开解析，否则转换器会因类型不符直接报错。
func DecodeStreamResponse(name Name, data []byte) (any, error) {
	var target any
	switch name {
	case OpenAIChat:
		target = &dto.ChatCompletionsStreamResponse{}
	case OpenAIResponses:
		target = &dto.ResponsesStreamResponse{}
	case ClaudeMessages, GeminiGenerate:
		// Claude 与 Gemini 的流式事件与非流式响应同形。
		return DecodeResponse(name, data)
	default:
		return nil, fmt.Errorf("dialect: 未知方言 %q", name)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return nil, fmt.Errorf("dialect: 解析 %s 流式事件失败: %w", name, err)
	}
	return target, nil
}

// isHeartbeat 判断事件是否是保活心跳。
func isHeartbeat(name Name, value any) bool {
	switch name {
	case ClaudeMessages:
		response, ok := value.(*dto.ClaudeResponse)
		return ok && response.Type == "ping"
	case OpenAIResponses:
		response, ok := value.(*dto.ResponsesStreamResponse)
		return ok && response.Type == "ping"
	default:
		return false
	}
}

// Encode 把 DTO 编成 JSON 字节。
func Encode(value any) ([]byte, error) {
	return json.Marshal(value)
}

// EncodeStreamEvent 把转换结果编成一条可写出的流式事件。
//
// 转成 OpenAI Responses 时 RelayKit 返回 relayconvert.ChatToResponsesStreamEvent
// 包装体，事件名在包装体的 Type 上而负载在 Payload 上；其它情况事件名取自
// DTO 自身的 type 字段。
func EncodeStreamEvent(name Name, value any) (StreamEvent, error) {
	payload := value
	event := ""
	if wrapper, ok := value.(relayconvert.ChatToResponsesStreamEvent); ok {
		payload = wrapper.Payload
		event = wrapper.Type
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return StreamEvent{}, fmt.Errorf("dialect: 编码 %s 流式事件失败: %w", name, err)
	}
	if event == "" {
		switch typed := payload.(type) {
		case *dto.ResponsesStreamResponse:
			event = typed.Type
		case dto.ResponsesStreamResponse:
			event = typed.Type
		case *dto.ClaudeResponse:
			event = typed.Type
		case dto.ClaudeResponse:
			event = typed.Type
		}
	}
	return StreamEvent{Event: event, Data: data}, nil
}

// DoneEvent 返回 OpenAI Chat Completions 的流结束标记。
func DoneEvent() []byte {
	return []byte(doneMarker)
}

// IsDoneMarker 报告一段 data 负载是否是流结束标记。
func IsDoneMarker(data []byte) bool {
	return string(bytes.TrimSpace(data)) == doneMarker
}

// ErrUnsupportedPayload 表示载荷类型不属于该方言。
var ErrUnsupportedPayload = errors.New("dialect: 载荷类型与方言不匹配")

// CheckPayload 校验载荷类型是否与方言一致，供测试与断言使用。
func CheckPayload(name Name, value any) error {
	format, ok := FromRelayFormat(name.RelayFormat())
	if !ok || format != name {
		return ErrUnsupportedPayload
	}
	switch name {
	case OpenAIChat:
		switch value.(type) {
		case *dto.GeneralOpenAIRequest, *dto.OpenAITextResponse, *dto.ChatCompletionsStreamResponse:
			return nil
		}
	case OpenAIResponses:
		switch value.(type) {
		case *dto.OpenAIResponsesRequest, *dto.OpenAIResponsesResponse, *dto.ResponsesStreamResponse:
			return nil
		}
	case ClaudeMessages:
		switch value.(type) {
		case *dto.ClaudeRequest, *dto.ClaudeResponse:
			return nil
		}
	case GeminiGenerate:
		switch value.(type) {
		case *dto.GeminiChatRequest, *dto.GeminiChatResponse:
			return nil
		}
	}
	return fmt.Errorf("%w: %s / %T", ErrUnsupportedPayload, name, value)
}

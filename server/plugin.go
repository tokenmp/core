package server

import "encoding/json"

// requestPayload 是 onRequest 的输入。
type requestPayload struct {
	// Dialect 是客户端方言。
	Dialect string `json:"dialect"`
	// Model 是本次请求的模型名。
	Model string `json:"model"`
	// Stream 是本次请求的流式意图。
	Stream bool `json:"stream"`
	// Body 是请求体。
	Body json.RawMessage `json:"body"`
}

// requestRewrite 是 onRequest 的输出。未给出的字段保持原值。
type requestRewrite struct {
	// Model 非空时替换模型名，进而改变选路。
	Model *string `json:"model,omitempty"`
	// Body 非空时替换请求体。
	Body json.RawMessage `json:"body,omitempty"`
}

// responsePayload 是 onResponse 的输入。
type responsePayload struct {
	// Dialect 是客户端方言。
	Dialect string `json:"dialect"`
	// Model 是本次请求的模型名。
	Model string `json:"model"`
	// Body 是已经成形的响应体。
	Body json.RawMessage `json:"body"`
}

// responseRewrite 是 onResponse 的输出。
type responseRewrite struct {
	// Body 非空时替换响应体。
	Body json.RawMessage `json:"body,omitempty"`
}

// eventPayload 是 onEvent 的输入。
type eventPayload struct {
	// Dialect 是客户端方言。
	Dialect string `json:"dialect"`
	// Event 是 SSE 的事件名，可能为空。
	Event string `json:"event"`
	// Data 是事件的 JSON 负载。
	Data json.RawMessage `json:"data"`
}

// eventRewrite 是 onEvent 的输出。
type eventRewrite struct {
	// Event 非空时替换事件名。
	Event *string `json:"event,omitempty"`
	// Data 非空时替换事件负载。
	Data json.RawMessage `json:"data,omitempty"`
}

// streamEvent 是一条流式事件。
type streamEvent struct {
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data"`
}

// streamEndPayload 是 onStreamEnd 的输入。
type streamEndPayload struct {
	// Dialect 是客户端方言。
	Dialect string `json:"dialect"`
	// Model 是本次请求的模型名。
	Model string `json:"model"`
	// Usage 是累计用量，可能为 null。
	Usage json.RawMessage `json:"usage"`
	// Events 是收尾阶段补发的分片。
	Events []streamEvent `json:"events"`
}

// streamEndRewrite 是 onStreamEnd 的输出，只允许替换分片列表。
type streamEndRewrite struct {
	// Events 非空时替换补发的分片。
	Events []streamEvent `json:"events,omitempty"`
}

// mergeRequest 把 onRequest 的改写合并回负载。
//
// 合并之后下一个插件看到的仍然是完整的 {dialect, model, stream, body}，
// 而不是只看到上一个插件的返回值。
func mergeRequest(payload, rewrite []byte) ([]byte, error) {
	var current requestPayload
	if err := json.Unmarshal(payload, &current); err != nil {
		return nil, err
	}
	var change requestRewrite
	if err := json.Unmarshal(rewrite, &change); err != nil {
		return nil, err
	}
	changed := false
	if change.Model != nil && *change.Model != "" {
		current.Model = *change.Model
		changed = true
	}
	if len(change.Body) > 0 {
		current.Body = change.Body
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return json.Marshal(current)
}

// mergeResponse 把 onResponse 的改写合并回负载。
func mergeResponse(payload, rewrite []byte) ([]byte, error) {
	var current responsePayload
	if err := json.Unmarshal(payload, &current); err != nil {
		return nil, err
	}
	var change responseRewrite
	if err := json.Unmarshal(rewrite, &change); err != nil {
		return nil, err
	}
	if len(change.Body) == 0 {
		return nil, nil
	}
	current.Body = change.Body
	return json.Marshal(current)
}

// mergeEvent 把 onEvent 的改写合并回负载。
func mergeEvent(payload, rewrite []byte) ([]byte, error) {
	var current eventPayload
	if err := json.Unmarshal(payload, &current); err != nil {
		return nil, err
	}
	var change eventRewrite
	if err := json.Unmarshal(rewrite, &change); err != nil {
		return nil, err
	}
	changed := false
	if change.Event != nil && *change.Event != "" {
		current.Event = *change.Event
		changed = true
	}
	if len(change.Data) > 0 {
		current.Data = change.Data
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return json.Marshal(current)
}

// mergeStreamEnd 把 onStreamEnd 的改写合并回负载。
func mergeStreamEnd(payload, rewrite []byte) ([]byte, error) {
	var current streamEndPayload
	if err := json.Unmarshal(payload, &current); err != nil {
		return nil, err
	}
	var change streamEndRewrite
	if err := json.Unmarshal(rewrite, &change); err != nil {
		return nil, err
	}
	if change.Events == nil {
		return nil, nil
	}
	current.Events = change.Events
	return json.Marshal(current)
}

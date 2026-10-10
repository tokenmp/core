package dialect

import (
	"encoding/json"
	"fmt"
)

// modelOnly 只取请求体里的模型名，不做完整建模。
type modelOnly struct {
	Model string `json:"model"`
}

// ModelOf 从请求体里读出模型名。
//
// 只有 Gemini 的模型名在 URL 路径上，因此该方言返回空串且不报错，调用方需自行
// 从路径取用。其余三方言返回请求体里的 model 字段，缺失时返回错误。
func ModelOf(name Name, body []byte) (string, error) {
	if name == GeminiGenerate {
		return "", nil
	}
	var parsed modelOnly
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("dialect: 解析 %s 请求的模型名失败: %w", name, err)
	}
	if parsed.Model == "" {
		return "", fmt.Errorf("dialect: %s 请求缺少 model 字段", name)
	}
	return parsed.Model, nil
}

// streamOnly 只取请求体里的流式意图。
type streamOnly struct {
	Stream *bool `json:"stream"`
}

// StreamIntent 返回请求的流式意图。
//
// Gemini 的流式只体现在路径上，因此该方言直接用 hint。其余三方言以请求体为准，
// 体里没有 stream 字段时退回 hint。
func StreamIntent(name Name, body []byte, hint bool) (bool, error) {
	if name == GeminiGenerate {
		return hint, nil
	}
	var parsed streamOnly
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, fmt.Errorf("dialect: 解析 %s 请求的 stream 字段失败: %w", name, err)
	}
	if parsed.Stream == nil {
		return hint, nil
	}
	return *parsed.Stream, nil
}

// sessionOnly 只取请求体里的会话标识。
type sessionOnly struct {
	PromptCacheKey string `json:"prompt_cache_key"`
}

// SessionKeyOf 返回客户端在请求体里给出的会话标识，没有时返回空串。
//
// 只有 OpenAI 的两个方言有 prompt_cache_key 字段，它是 OpenAI 为缓存路由专门
// 提供的。Claude 与 Gemini 把缓存挂在别处（cache_control 断点、cachedContent
// 引用），体里没有对应的标识，因此返回空串，调用方应改用请求头。
//
// 解析失败返回空串而不是错误：调用方在此之前已经用 DecodeRequest 校验过请求体，
// 这里再报一次错只会多出一条没有信息量的分支。
func SessionKeyOf(name Name, body []byte) string {
	if name != OpenAIChat && name != OpenAIResponses {
		return ""
	}
	var parsed sessionOnly
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.PromptCacheKey
}

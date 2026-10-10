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
